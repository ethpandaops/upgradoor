package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const hostColumns = `name, cert_hash, generation, validators_start, validators_end, roles,
	first_seen, last_seen, remote_addr, status, inventory_hash, hold_wanted, hold_reason`

// RecordSync upserts the host named by the certificate subject. first_seen
// is kept for a known host. With a nil Request only the identity, last_seen
// and remote address are updated; the stored status and needs stay.
func (s *sqliteStore) RecordSync(ctx context.Context, in *HostSync) (*SyncResult, error) {
	if in == nil || in.Cert == nil {
		return nil, errors.New("store: record sync: certificate is required")
	}

	cert := in.Cert
	if cert.Subject == "" {
		return nil, errors.New("store: record sync: certificate has no subject")
	}

	at := in.At
	if at.IsZero() {
		at = time.Now()
	}

	rolesJSON, status, err := encodeSync(in)
	if err != nil {
		return nil, fmt.Errorf("store: record sync of %s: %w", cert.Subject, err)
	}

	var valStart, valEnd sql.NullInt64
	if cert.Validators != nil {
		valStart = sql.NullInt64{Int64: int64(cert.Validators.Start), Valid: true}
		valEnd = sql.NullInt64{Int64: int64(cert.Validators.End), Valid: true}
	}

	result := &SyncResult{}

	err = s.write(ctx, func(tx *sql.Tx) error {
		var previous string

		txErr := tx.QueryRowContext(ctx,
			`SELECT cert_hash FROM hosts WHERE name = ?`, cert.Subject).Scan(&previous)

		switch {
		case errors.Is(txErr, sql.ErrNoRows):
			result.Created = true
		case txErr != nil:
			return txErr
		case previous != cert.Hash:
			result.PreviousCertHash = previous
		}

		_, txErr = tx.ExecContext(ctx, `
			INSERT INTO hosts (name, cert_hash, generation, validators_start, validators_end,
				roles, first_seen, last_seen, remote_addr, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				cert_hash = excluded.cert_hash,
				generation = excluded.generation,
				validators_start = excluded.validators_start,
				validators_end = excluded.validators_end,
				roles = excluded.roles,
				last_seen = excluded.last_seen,
				remote_addr = excluded.remote_addr,
				status = COALESCE(excluded.status, hosts.status)`,
			cert.Subject, cert.Hash, int64(cert.Generation), valStart, valEnd,
			rolesJSON, millis(at), millis(at), in.RemoteAddr, status)
		if txErr != nil {
			return txErr
		}

		if in.Request != nil {
			txErr = replaceNeeds(ctx, tx, cert.Subject, in.Request.Needs, at)
			if txErr != nil {
				return txErr
			}
		}

		result.Host, txErr = getHost(ctx, tx, cert.Subject)

		return txErr
	})
	if err != nil {
		return nil, fmt.Errorf("store: record sync of %s: %w", cert.Subject, err)
	}

	return result, nil
}

// encodeSync returns the roles and status columns of a sync. The status is
// NULL without a request.
func encodeSync(in *HostSync) (roles string, status sql.NullString, err error) {
	list := in.Cert.Roles
	if list == nil {
		list = []string{}
	}

	raw, err := json.Marshal(list)
	if err != nil {
		return "", status, fmt.Errorf("encode roles: %w", err)
	}

	roles = string(raw)

	if in.Request != nil {
		raw, err = json.Marshal(in.Request)
		if err != nil {
			return "", status, fmt.Errorf("encode status: %w", err)
		}

		status = sql.NullString{String: string(raw), Valid: true}
	}

	return roles, status, nil
}

// replaceNeeds replaces the host's needs rows. A need that was already open
// keeps its first_seen; a repeated need ID is stored once.
func replaceNeeds(
	ctx context.Context, tx *sql.Tx, host string, needs []protocol.Need, at time.Time,
) error {
	known := make(map[string]int64, len(needs))

	err := queryRows(ctx, tx, `SELECT id, first_seen FROM needs WHERE host = ?`, []any{host},
		func(row rowScanner) error {
			var (
				id        string
				firstSeen int64
			)

			if err := row.Scan(&id, &firstSeen); err != nil {
				return err
			}

			known[id] = firstSeen

			return nil
		})
	if err != nil {
		return fmt.Errorf("read needs: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM needs WHERE host = ?`, host); err != nil {
		return fmt.Errorf("clear needs: %w", err)
	}

	stored := make(map[string]struct{}, len(needs))

	for i := range needs {
		need := &needs[i]
		if _, dup := stored[need.ID]; dup {
			continue
		}

		stored[need.ID] = struct{}{}

		detail, err := json.Marshal(need)
		if err != nil {
			return fmt.Errorf("encode need %q: %w", need.ID, err)
		}

		firstSeen, ok := known[need.ID]
		if !ok {
			firstSeen = millis(at)
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO needs (host, id, kind, detail, first_seen) VALUES (?, ?, ?, ?, ?)`,
			host, need.ID, string(need.Kind), string(detail), firstSeen)
		if err != nil {
			return fmt.Errorf("store need %q: %w", need.ID, err)
		}
	}

	return nil
}

// GetHost returns one host or ErrNotFound.
func (s *sqliteStore) GetHost(ctx context.Context, name string) (*fleet.Host, error) {
	var host *fleet.Host

	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error

		host, err = getHost(ctx, tx, name)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: get host %s: %w", name, err)
	}

	return host, nil
}

func getHost(ctx context.Context, tx *sql.Tx, name string) (*fleet.Host, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM hosts WHERE name = ?`, name)

	host, err := scanHost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	return host, err
}

// ListHosts returns all hosts ordered by name.
func (s *sqliteStore) ListHosts(ctx context.Context) ([]*fleet.Host, error) {
	hosts := make([]*fleet.Host, 0, 32)

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `SELECT `+hostColumns+` FROM hosts ORDER BY name`, nil,
			func(row rowScanner) error {
				host, err := scanHost(row)
				if err != nil {
					return err
				}

				hosts = append(hosts, host)

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list hosts: %w", err)
	}

	return hosts, nil
}

func scanHost(row rowScanner) (*fleet.Host, error) {
	var (
		host                fleet.Host
		generation          int64
		valStart, valEnd    sql.NullInt64
		roles               string
		firstSeen, lastSeen int64
		status              sql.NullString
	)

	err := row.Scan(&host.Name, &host.CertHash, &generation, &valStart, &valEnd, &roles,
		&firstSeen, &lastSeen, &host.RemoteAddr, &status, &host.InventoryHash,
		&host.HoldWanted, &host.HoldReason)
	if err != nil {
		return nil, err
	}

	host.Generation = uint64(generation)
	host.FirstSeen = fromMillis(firstSeen)
	host.LastSeen = fromMillis(lastSeen)

	if valStart.Valid && valEnd.Valid {
		host.Validators = &identity.ValidatorRange{
			Start: uint64(valStart.Int64),
			End:   uint64(valEnd.Int64),
		}
	}

	if err := json.Unmarshal([]byte(roles), &host.Roles); err != nil {
		return nil, fmt.Errorf("decode roles of %s: %w", host.Name, err)
	}

	if host.Roles == nil {
		host.Roles = []string{}
	}

	if status.Valid {
		host.Status = &protocol.SyncRequest{}
		if err := json.Unmarshal([]byte(status.String), host.Status); err != nil {
			return nil, fmt.Errorf("decode status of %s: %w", host.Name, err)
		}
	}

	return &host, nil
}

// SetHostHold sets the controller-side hold switch. It returns ErrNotFound
// for an unknown host.
func (s *sqliteStore) SetHostHold(
	ctx context.Context, name string, wanted bool, reason string,
) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE hosts SET hold_wanted = ?, hold_reason = ? WHERE name = ?`,
			wanted, reason, name)
		if err != nil {
			return err
		}

		return requireRow(res)
	})
	if err != nil {
		return fmt.Errorf("store: set hold of %s: %w", name, err)
	}

	return nil
}

// DeleteHost removes a host with its inventory, packages, upgradable, needs
// and liveness rows. Rollouts, leases, directives and audit entries stay. It
// returns ErrNotFound for an unknown host.
func (s *sqliteStore) DeleteHost(ctx context.Context, name string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		// inventories, packages, upgradable and needs go with the host by
		// ON DELETE CASCADE; liveness has no foreign key because the beacon
		// service may store rows for a host before or after it exists.
		_, err := tx.ExecContext(ctx, `DELETE FROM validator_liveness WHERE host = ?`, name)
		if err != nil {
			return err
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM hosts WHERE name = ?`, name)
		if err != nil {
			return err
		}

		return requireRow(res)
	})
	if err != nil {
		return fmt.Errorf("store: delete host %s: %w", name, err)
	}

	return nil
}

// requireRow turns "no row affected" into ErrNotFound.
func requireRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		return ErrNotFound
	}

	return nil
}
