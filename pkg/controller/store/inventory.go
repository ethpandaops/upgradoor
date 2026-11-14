package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// insertBatchRows is how many rows one multi-row INSERT carries. With at
	// most 8 columns that stays far below SQLite's bound-parameter limit.
	insertBatchRows = 250

	// DefaultPackageSearchLimit is the number of package names
	// SearchPackages returns when the caller passes no limit.
	DefaultPackageSearchLimit = 50
	// MaxPackageSearchLimit caps the number of package names SearchPackages
	// returns.
	MaxPackageSearchLimit = 500
)

// ErrInventoryHash is returned by SaveInventory when Inventory.Hash is not
// the hash of the inventory's content.
var ErrInventoryHash = errors.New("store: inventory hash does not match its content")

// ErrInventoryInvalid is returned by SaveInventory when Packages or
// Upgradable is not sorted by name then arch, as the protocol requires, or
// lists the same (name, arch) twice. Like ErrInventoryHash it means the
// payload is bad, not the database.
var ErrInventoryInvalid = errors.New("store: inventory is not valid")

// SaveInventory replaces the host's inventory, packages and upgradable rows
// in one transaction and sets hosts.inventory_hash. It returns ErrNotFound
// for an unknown host, ErrInventoryHash when inv.Hash is wrong and
// ErrInventoryInvalid when a list is unsorted or has a duplicate entry.
//
// The lists are stored as rows without a position, so only an inventory in
// the protocol's order can be given back by GetInventory as it was sent;
// anything else is refused instead of being silently reordered under a hash
// that would no longer match.
func (s *sqliteStore) SaveInventory(
	ctx context.Context, host string, inv *protocol.Inventory,
) error {
	if inv == nil {
		return errors.New("store: save inventory: inventory is required")
	}

	hash, err := inv.ComputeHash()
	if err != nil {
		return fmt.Errorf("store: save inventory of %s: %w", host, err)
	}

	if hash != inv.Hash {
		return fmt.Errorf("store: save inventory of %s: %w", host, ErrInventoryHash)
	}

	err = validateInventory(inv)
	if err != nil {
		return fmt.Errorf("store: save inventory of %s: %w", host, err)
	}

	osJSON, err := json.Marshal(inv.OS)
	if err != nil {
		return fmt.Errorf("store: save inventory of %s: encode os: %w", host, err)
	}

	kernelsJSON, err := json.Marshal(inv.Kernels)
	if err != nil {
		return fmt.Errorf("store: save inventory of %s: encode kernels: %w", host, err)
	}

	err = s.write(ctx, func(tx *sql.Tx) error {
		return replaceInventory(ctx, tx, host, inv, string(osJSON), string(kernelsJSON))
	})
	if err != nil {
		return fmt.Errorf("store: save inventory of %s: %w", host, err)
	}

	return nil
}

// validateInventory checks that Packages and Upgradable are strictly
// ascending by (name, arch): sorted, and without duplicates.
func validateInventory(inv *protocol.Inventory) error {
	err := checkAscending("packages", len(inv.Packages), func(i int) (string, string) {
		return inv.Packages[i].Name, inv.Packages[i].Arch
	})
	if err != nil {
		return err
	}

	return checkAscending("upgradable", len(inv.Upgradable), func(i int) (string, string) {
		return inv.Upgradable[i].Name, inv.Upgradable[i].Arch
	})
}

// checkAscending reports the first entry of a list that is not greater than
// its predecessor. Strings compare bytewise, which is also the order of the
// tables' BINARY collation that GetInventory reads them back in.
func checkAscending(list string, n int, key func(i int) (name, arch string)) error {
	if n == 0 {
		return nil
	}

	prevName, prevArch := key(0)

	for i := 1; i < n; i++ {
		name, arch := key(i)

		order := strings.Compare(prevName, name)
		if order == 0 {
			order = strings.Compare(prevArch, arch)
		}

		switch {
		case order == 0:
			return fmt.Errorf("%w: %s[%d]: %s (%s) is listed twice",
				ErrInventoryInvalid, list, i, name, arch)
		case order > 0:
			return fmt.Errorf("%w: %s[%d]: %s (%s) is not sorted by name then arch",
				ErrInventoryInvalid, list, i, name, arch)
		}

		prevName, prevArch = name, arch
	}

	return nil
}

// replaceInventory writes the inventory rows of one host inside tx.
func replaceInventory(
	ctx context.Context, tx *sql.Tx, host string, inv *protocol.Inventory, osJSON, kernelsJSON string,
) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE hosts SET inventory_hash = ? WHERE name = ?`, inv.Hash, host)
	if err != nil {
		return err
	}

	err = requireRow(res)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO inventories (host, hash, collected_at, os, kernels)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET
			hash = excluded.hash,
			collected_at = excluded.collected_at,
			os = excluded.os,
			kernels = excluded.kernels`,
		host, inv.Hash, millis(inv.CollectedAt), osJSON, kernelsJSON)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM packages WHERE host = ?`, host)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM upgradable WHERE host = ?`, host)
	if err != nil {
		return err
	}

	err = insertBatched(ctx, tx,
		`INSERT INTO packages (host, name, version, arch) VALUES `, 4, len(inv.Packages),
		func(args []any, i int) []any {
			p := &inv.Packages[i]

			return append(args, host, p.Name, p.Version, p.Arch)
		})
	if err != nil {
		return fmt.Errorf("packages: %w", err)
	}

	err = insertBatched(ctx, tx,
		`INSERT INTO upgradable
			(host, name, arch, current, candidate, origin, security, blocked) VALUES `,
		8, len(inv.Upgradable),
		func(args []any, i int) []any {
			u := &inv.Upgradable[i]

			return append(args, host, u.Name, u.Arch, u.Current, u.Candidate, u.Origin,
				u.Security, u.Blocked)
		})
	if err != nil {
		return fmt.Errorf("upgradable: %w", err)
	}

	return nil
}

// insertBatched inserts rows with multi-row INSERT statements. prefix ends
// in "VALUES "; appendRow appends the cols arguments of row i.
func insertBatched(
	ctx context.Context, tx *sql.Tx, prefix string, cols, rows int,
	appendRow func(args []any, i int) []any,
) error {
	if rows == 0 {
		return nil
	}

	tuple := "(" + placeholders(cols) + ")"
	full := prefix + strings.Repeat(tuple+", ", insertBatchRows-1) + tuple
	args := make([]any, 0, min(rows, insertBatchRows)*cols)

	for start := 0; start < rows; start += insertBatchRows {
		n := min(insertBatchRows, rows-start)
		query := full

		if n < insertBatchRows {
			query = prefix + strings.Repeat(tuple+", ", n-1) + tuple
		}

		args = args[:0]
		for i := start; i < start+n; i++ {
			args = appendRow(args, i)
		}

		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}

	return nil
}

// GetInventory returns the stored inventory or ErrNotFound, as it was saved:
// Packages and Upgradable sorted by name then arch, and ComputeHash() of the
// result equal to its Hash. A list that is empty comes back as an empty
// slice, unless the agent sent it as null (which hashes differently): then
// it comes back nil. CollectedAt, which is not part of the hash, comes back
// in UTC with millisecond precision.
func (s *sqliteStore) GetInventory(ctx context.Context, host string) (*protocol.Inventory, error) {
	var inv *protocol.Inventory

	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error

		inv, err = loadInventory(ctx, tx, host)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: get inventory of %s: %w", host, err)
	}

	return inv, nil
}

// loadInventory reads the inventory of one host inside tx.
func loadInventory(ctx context.Context, tx *sql.Tx, host string) (*protocol.Inventory, error) {
	inv := &protocol.Inventory{}

	var (
		collectedAt int64
		osJSON      string
		kernelsJSON string
	)

	err := tx.QueryRowContext(ctx,
		`SELECT hash, collected_at, os, kernels FROM inventories WHERE host = ?`, host,
	).Scan(&inv.Hash, &collectedAt, &osJSON, &kernelsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, err
	}

	inv.CollectedAt = fromMillis(collectedAt)

	err = json.Unmarshal([]byte(osJSON), &inv.OS)
	if err != nil {
		return nil, fmt.Errorf("decode os: %w", err)
	}

	err = json.Unmarshal([]byte(kernelsJSON), &inv.Kernels)
	if err != nil {
		return nil, fmt.Errorf("decode kernels: %w", err)
	}

	var count int

	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM packages WHERE host = ?`, host).Scan(&count)
	if err != nil {
		return nil, err
	}

	inv.Packages = make([]protocol.Package, 0, count)

	err = queryRows(ctx, tx,
		`SELECT name, version, arch FROM packages WHERE host = ? ORDER BY name, arch`,
		[]any{host},
		func(row rowScanner) error {
			var p protocol.Package
			if scanErr := row.Scan(&p.Name, &p.Version, &p.Arch); scanErr != nil {
				return scanErr
			}

			inv.Packages = append(inv.Packages, p)

			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("packages: %w", err)
	}

	inv.Upgradable = make([]protocol.Upgradable, 0, 16)

	err = queryRows(ctx, tx, `
		SELECT name, arch, current, candidate, origin, security, blocked
		FROM upgradable WHERE host = ? ORDER BY name, arch`,
		[]any{host},
		func(row rowScanner) error {
			var u protocol.Upgradable

			scanErr := row.Scan(&u.Name, &u.Arch, &u.Current, &u.Candidate, &u.Origin,
				&u.Security, &u.Blocked)
			if scanErr != nil {
				return scanErr
			}

			inv.Upgradable = append(inv.Upgradable, u)

			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("upgradable: %w", err)
	}

	err = restoreNilLists(inv)
	if err != nil {
		return nil, err
	}

	return inv, nil
}

// restoreNilLists turns empty lists of a loaded inventory back into nil
// where that is what the stored hash was computed over. The tables cannot
// tell an empty list from a null one, but the hash can: null and [] encode
// differently. An inventory with both lists filled needs no check.
func restoreNilLists(inv *protocol.Inventory) error {
	if len(inv.Packages) > 0 && len(inv.Upgradable) > 0 {
		return nil
	}

	candidates := [...]struct{ nilPackages, nilUpgradable bool }{
		{false, false}, {false, true}, {true, false}, {true, true},
	}

	for _, c := range candidates {
		if (c.nilPackages && len(inv.Packages) > 0) ||
			(c.nilUpgradable && len(inv.Upgradable) > 0) {
			continue
		}

		probe := *inv
		if c.nilPackages {
			probe.Packages = nil
		}

		if c.nilUpgradable {
			probe.Upgradable = nil
		}

		hash, err := probe.ComputeHash()
		if err != nil {
			return err
		}

		if hash == inv.Hash {
			inv.Packages, inv.Upgradable = probe.Packages, probe.Upgradable

			return nil
		}
	}

	// No form matches (the rows were changed behind the store's back):
	// leave the empty lists.
	return nil
}

// SearchPackages finds installed packages whose name contains query,
// ignoring ASCII case. Names are ordered: the exact match, then names that
// start with query, then the rest, each alphabetically. limit bounds the
// number of names (0 means DefaultPackageSearchLimit, at most
// MaxPackageSearchLimit). Versions are sorted as strings, hosts by name. A
// blank query matches nothing.
func (s *sqliteStore) SearchPackages(
	ctx context.Context, query string, limit int,
) ([]PackageMatch, error) {
	query = strings.TrimSpace(query)
	matches := make([]PackageMatch, 0, 16)

	if query == "" {
		return matches, nil
	}

	switch {
	case limit <= 0:
		limit = DefaultPackageSearchLimit
	case limit > MaxPackageSearchLimit:
		limit = MaxPackageSearchLimit
	}

	escaped := escapeLike(query)

	err := s.read(ctx, func(tx *sql.Tx) error {
		names := make([]any, 0, limit)

		// LIKE is case-insensitive for ASCII, which is all Debian package
		// names contain. The scan runs over the (name, version) index.
		err := queryRows(ctx, tx, `
			SELECT name FROM packages
			WHERE name LIKE ? ESCAPE '\'
			GROUP BY name
			ORDER BY CASE
				WHEN name = ? COLLATE NOCASE THEN 0
				WHEN name LIKE ? ESCAPE '\' THEN 1
				ELSE 2 END, name
			LIMIT ?`,
			[]any{"%" + escaped + "%", query, escaped + "%", limit},
			func(row rowScanner) error {
				var name string
				if err := row.Scan(&name); err != nil {
					return err
				}

				names = append(names, name)

				return nil
			})
		if err != nil {
			return err
		}

		if len(names) == 0 {
			return nil
		}

		index := make(map[string]int, len(names))

		for i, name := range names {
			name, ok := name.(string)
			if !ok {
				continue
			}

			index[name] = i

			matches = append(matches, PackageMatch{Name: name, Versions: []PackageVersion{}})
		}

		// DISTINCT: a host can have one version for two architectures.
		return queryRows(ctx, tx, `
			SELECT DISTINCT name, version, host FROM packages
			WHERE name IN (`+placeholders(len(names))+`)
			ORDER BY name, version, host`,
			names,
			func(row rowScanner) error {
				var name, version, host string
				if err := row.Scan(&name, &version, &host); err != nil {
					return err
				}

				match := &matches[index[name]]

				last := len(match.Versions) - 1
				if last < 0 || match.Versions[last].Version != version {
					match.Versions = append(match.Versions,
						PackageVersion{Version: version, Hosts: make([]string, 0, 8)})
					last++
				}

				match.Versions[last].Hosts = append(match.Versions[last].Hosts, host)

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: search packages: %w", err)
	}

	return matches, nil
}
