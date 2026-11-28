package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// insertBatchRows is how many rows one multi-row INSERT carries. With at
	// most 11 columns that stays far below SQLite's bound-parameter limit.
	insertBatchRows = 250

	// DefaultPackageSearchLimit is the number of package names
	// SearchPackages returns when the caller passes no limit.
	DefaultPackageSearchLimit = 50
	// MaxPackageSearchLimit caps the number of package names SearchPackages
	// returns.
	MaxPackageSearchLimit = 500
)

// ErrInventoryInvalid is returned by SaveInventory when Packages or
// Upgradable is not sorted by name then arch, as the protocol requires, or
// lists the same (name, arch) twice. It means the payload is bad, not the
// database.
var ErrInventoryInvalid = errors.New("store: inventory is not valid")

// SaveInventory replaces the host's inventory, packages and upgradable rows
// in one transaction and sets hosts.inventory_hash. It returns ErrNotFound
// for an unknown host and ErrInventoryInvalid when a list is unsorted or has a
// duplicate entry. inv.Hash is stored as given: the caller checked it.
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

	err := validateInventory(inv)
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
	if strings.TrimSpace(query) == "" {
		return []PackageMatch{}, nil
	}

	page, err := s.ListPackages(ctx, &PackageFilter{Query: query, Limit: limit})
	if err != nil {
		return nil, err
	}

	return page.Matches, nil
}

// Ranks of a name in a search: see PackageFilter.
const (
	rankExact  = 0
	rankPrefix = 1
	rankOther  = 2
)

// packageCursor is where a page of names ends: the rank, the sort key and
// the name of its last entry. As a string it is the rank's digit, for a
// non-name sort the key and ":", and the name.
type packageCursor struct {
	rank int
	key  int
	name string
}

func (c packageCursor) format(sort PackageSort) string {
	if sort == PackageSortName {
		return strconv.Itoa(c.rank) + c.name
	}

	return strconv.Itoa(c.rank) + strconv.Itoa(c.key) + ":" + c.name
}

func parsePackageCursor(after string, sort PackageSort) (packageCursor, error) {
	if after == "" {
		// Before every name.
		return packageCursor{rank: -1}, nil
	}

	rank := int(after[0] - '0')
	if rank < rankExact || rank > rankOther || len(after) < 2 {
		return packageCursor{}, ErrBadCursor
	}

	if sort == PackageSortName {
		return packageCursor{rank: rank, name: after[1:]}, nil
	}

	digits, name, ok := strings.Cut(after[1:], ":")

	key, err := strconv.Atoi(digits)
	if !ok || err != nil || key < 0 || name == "" {
		return packageCursor{}, ErrBadCursor
	}

	return packageCursor{rank: rank, key: key, name: name}, nil
}

// packageNames selects the names of one page.
type packageNames struct {
	query string
	show  PackageShow
	sort  PackageSort
	after packageCursor
	limit int
}

// sql returns the query for the names of one page, each with its rank and
// sort key.
//
// Sorted by name without a query, the page is a range of the (name,
// version) index: it starts at the cursor and stops after limit names. A
// query matches a part of the name, which no index can look up, and a count
// sort needs every name's count: those walk the index, not the table.
func (n *packageNames) sql() (query string, args []any) {
	rank, key := "0", "0"

	var where, having []string

	if n.query != "" {
		escaped := escapeLike(n.query)
		// LIKE is case-insensitive for ASCII, which is all Debian package
		// names contain.
		rank = `CASE WHEN name = ? COLLATE NOCASE THEN 0 WHEN name LIKE ? ESCAPE '\' THEN 1 ELSE 2 END`

		args = append(args, n.query, escaped+"%")

		where = append(where, `name LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escaped+"%")
	}

	switch n.sort {
	case PackageSortVersions:
		key = "COUNT(DISTINCT version)"
	case PackageSortHosts:
		key = "COUNT(DISTINCT host)"
	case PackageSortOutdated:
		key = "(SELECT COUNT(DISTINCT u.host) FROM upgradable u WHERE u.name = packages.name)"
	case PackageSortName:
	}

	switch n.show {
	case PackageShowOutdated:
		where = append(where, "name IN (SELECT name FROM upgradable)")
	case PackageShowSecurity:
		where = append(where, "name IN (SELECT name FROM upgradable WHERE security = 1)")
	case PackageShowDiffering:
		having = append(having, "COUNT(DISTINCT version) > 1")
	case PackageShowAll:
	}

	if n.query == "" && n.sort == PackageSortName {
		where = append(where, "name > ?")
		args = append(args, n.after.name)
	} else {
		having = append(having, "(rank > ? OR (rank = ? AND (k < ? OR (k = ? AND name > ?))))")
		args = append(args, n.after.rank, n.after.rank, n.after.key, n.after.key, n.after.name)
	}

	query = "SELECT name, " + rank + " AS rank, " + key + " AS k FROM packages"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	query += " GROUP BY name"
	if len(having) > 0 {
		query += " HAVING " + strings.Join(having, " AND ")
	}

	switch {
	case n.sort != PackageSortName:
		query += " ORDER BY rank, k DESC, name LIMIT ?"
	case n.query != "":
		query += " ORDER BY rank, name LIMIT ?"
	default:
		query += " ORDER BY name LIMIT ?"
	}

	return query, append(args, n.limit)
}

// packageCountsSQL returns the queries that count the names a query
// selects: all and differing from the packages, outdated and security from
// the upgradable lists, whose names are all installed.
func packageCountsSQL(query string) (installed, upgradable string, args []any) {
	installed = `SELECT COUNT(*), COALESCE(SUM(n > 1), 0)
		FROM (SELECT COUNT(DISTINCT version) AS n FROM packages%s GROUP BY name)`
	upgradable = `SELECT COUNT(DISTINCT name), COUNT(DISTINCT CASE WHEN security = 1 THEN name END)
		FROM upgradable%s`

	where := ""
	if query != "" {
		where = ` WHERE name LIKE ? ESCAPE '\'`
		args = []any{"%" + escapeLike(query) + "%"}
	}

	return fmt.Sprintf(installed, where), fmt.Sprintf(upgradable, where), args
}

// ListPackages returns one page of package names with the versions installed
// across the fleet, the hosts on each and the upgrades offered to them.
// Versions are sorted as strings, hosts by name.
func (s *sqliteStore) ListPackages(ctx context.Context, filter *PackageFilter) (*PackagePage, error) {
	query := strings.TrimSpace(filter.Query)
	limit := filter.Limit

	switch {
	case limit <= 0:
		limit = DefaultPackageSearchLimit
	case limit > MaxPackageSearchLimit:
		limit = MaxPackageSearchLimit
	}

	after, err := parsePackageCursor(filter.After, filter.Sort)
	if err != nil {
		return nil, fmt.Errorf("store: list packages: %w", err)
	}

	page := &PackagePage{Matches: make([]PackageMatch, 0, limit), Total: -1}

	err = s.read(ctx, func(tx *sql.Tx) error {
		// One more than the page: whether another page follows.
		names := packageNames{query: query, show: filter.Show, sort: filter.Sort, after: after, limit: limit + 1}
		namesQuery, args := names.sql()
		last := after

		scanErr := queryRows(ctx, tx, namesQuery, args, func(row rowScanner) error {
			var cur packageCursor
			if rowErr := row.Scan(&cur.name, &cur.rank, &cur.key); rowErr != nil {
				return rowErr
			}

			if len(page.Matches) == limit {
				page.Next = last.format(filter.Sort)

				return nil
			}

			page.Matches = append(page.Matches, PackageMatch{
				Name: cur.name, Versions: []PackageVersion{}, Upgrades: []PackageUpgrade{},
			})
			last = cur

			return nil
		})
		if scanErr != nil {
			return scanErr
		}

		if filter.After == "" {
			counts, countErr := countPackages(ctx, tx, query)
			if countErr != nil {
				return countErr
			}

			page.Counts = counts

			switch filter.Show {
			case PackageShowDiffering:
				page.Total = counts.Differing
			case PackageShowOutdated:
				page.Total = counts.Outdated
			case PackageShowSecurity:
				page.Total = counts.Security
			case PackageShowAll:
				page.Total = counts.All
			}
		}

		if len(page.Matches) == 0 {
			return nil
		}

		return fillPackageMatches(ctx, tx, page.Matches, filter.Compact)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list packages: %w", err)
	}

	return page, nil
}

// countPackages counts the names query selects under each show.
func countPackages(ctx context.Context, tx *sql.Tx, query string) (*PackageCounts, error) {
	installed, upgradable, args := packageCountsSQL(query)
	counts := &PackageCounts{}

	if err := tx.QueryRowContext(ctx, installed, args...).Scan(&counts.All, &counts.Differing); err != nil {
		return nil, err
	}

	if err := tx.QueryRowContext(ctx, upgradable, args...).Scan(&counts.Outdated, &counts.Security); err != nil {
		return nil, err
	}

	return counts, nil
}

// fillPackageMatches adds the versions, hosts and upgrades of the named
// matches. Compact leaves out the hosts of each name's most common version
// and of the upgrades.
func fillPackageMatches(ctx context.Context, tx *sql.Tx, matches []PackageMatch, compact bool) error {
	index := make(map[string]int, len(matches))
	names := make([]any, 0, len(matches))

	for i := range matches {
		index[matches[i].Name] = i
		names = append(names, matches[i].Name)
	}

	in := placeholders(len(names))

	// DISTINCT host: a host can have one version for two architectures.
	err := queryRows(ctx, tx, `
		SELECT name, version, COUNT(DISTINCT host) FROM packages
		WHERE name IN (`+in+`)
		GROUP BY name, version
		ORDER BY name, version`,
		names,
		func(row rowScanner) error {
			var v PackageVersion

			var name string
			if rowErr := row.Scan(&name, &v.Version, &v.Count); rowErr != nil {
				return rowErr
			}

			v.Hosts = []string{}
			match := &matches[index[name]]
			match.Versions = append(match.Versions, v)

			return nil
		})
	if err != nil {
		return err
	}

	hostsQuery := `SELECT DISTINCT name, version, host FROM packages WHERE name IN (` + in + `)`
	hostsArgs := names

	if compact {
		skip := make([]string, 0, len(matches))
		hostsArgs = slices.Clone(names)

		for i := range matches {
			if top := mostCommonVersion(matches[i].Versions); top >= 0 {
				skip = append(skip, "(?, ?)")
				hostsArgs = append(hostsArgs, matches[i].Name, matches[i].Versions[top].Version)
			}
		}

		if len(skip) > 0 {
			hostsQuery += ` AND (name, version) NOT IN (VALUES ` + strings.Join(skip, ", ") + `)`
		}
	}

	err = queryRows(ctx, tx, hostsQuery+` ORDER BY name, version, host`, hostsArgs,
		func(row rowScanner) error {
			var name, version, host string
			if rowErr := row.Scan(&name, &version, &host); rowErr != nil {
				return rowErr
			}

			match := &matches[index[name]]
			if i := slices.IndexFunc(match.Versions, func(v PackageVersion) bool {
				return v.Version == version
			}); i >= 0 {
				match.Versions[i].Hosts = append(match.Versions[i].Hosts, host)
			}

			return nil
		})
	if err != nil {
		return err
	}

	return fillPackageUpgrades(ctx, tx, matches, index, names, compact)
}

// fillPackageUpgrades adds the upgrades of the matches, with their hosts
// unless compact.
func fillPackageUpgrades(
	ctx context.Context, tx *sql.Tx, matches []PackageMatch, index map[string]int, names []any, compact bool,
) error {
	in := placeholders(len(names))

	err := queryRows(ctx, tx, `
		SELECT name, current, candidate, MAX(security), MAX(blocked), COUNT(DISTINCT host)
		FROM upgradable WHERE name IN (`+in+`)
		GROUP BY name, current, candidate
		ORDER BY name, current, candidate`,
		names,
		func(row rowScanner) error {
			var (
				name string
				u    PackageUpgrade
			)

			if rowErr := row.Scan(&name, &u.Current, &u.Candidate, &u.Security, &u.Blocked, &u.Count); rowErr != nil {
				return rowErr
			}

			u.Hosts = []string{}
			match := &matches[index[name]]
			match.Upgrades = append(match.Upgrades, u)

			return nil
		})
	if err != nil || compact {
		return err
	}

	return queryRows(ctx, tx, `
		SELECT DISTINCT name, current, candidate, host FROM upgradable
		WHERE name IN (`+in+`)
		ORDER BY name, current, candidate, host`,
		names,
		func(row rowScanner) error {
			var name, current, candidate, host string
			if rowErr := row.Scan(&name, &current, &candidate, &host); rowErr != nil {
				return rowErr
			}

			match := &matches[index[name]]
			if i := slices.IndexFunc(match.Upgrades, func(u PackageUpgrade) bool {
				return u.Current == current && u.Candidate == candidate
			}); i >= 0 {
				match.Upgrades[i].Hosts = append(match.Upgrades[i].Hosts, host)
			}

			return nil
		})
}

// mostCommonVersion returns the index of the version on the most hosts, the
// first of a tie; -1 for none.
func mostCommonVersion(versions []PackageVersion) int {
	top := -1

	for i := range versions {
		if top < 0 || versions[i].Count > versions[top].Count {
			top = i
		}
	}

	return top
}
