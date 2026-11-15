package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// rolloutHostsQuery reads the members of one rollout.
const rolloutHostsQuery = `
	SELECT host, state, stage, need_id, lease_id, error, updated_at
	FROM rollout_hosts WHERE rollout_id = ? ORDER BY host`

// The queries below are read on every reconcile tick (the open rollouts, the
// open leases, the pending directives) and on every sync of an agent (its
// open lease, its pending directives). None of the three tables is pruned, so
// each of them names the states it wants: a positive list is what lets SQLite
// use the index on state, where "state NOT IN (...)" reads the whole table.

// rolloutsQuery builds the query of ListRollouts.
func rolloutsQuery(filter RolloutFilter) (query string, args []any) {
	var cond conditions

	if filter.OpenOnly {
		open := fleet.OpenRolloutStates()
		states := make([]any, 0, len(open))

		for _, state := range open {
			states = append(states, string(state))
		}

		cond.add(`state IN (`+placeholders(len(open))+`)`, states...)
	}

	query = `SELECT data FROM rollouts` + cond.where() + ` ORDER BY created_at DESC, id DESC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`

		cond.args = append(cond.args, filter.Limit)
	}

	return query, cond.args
}

// leasesQuery builds the query of ListLeases.
func leasesQuery(filter LeaseFilter) (query string, args []any) {
	var cond conditions

	if filter.Host != "" {
		cond.add(`host = ?`, filter.Host)
	}

	if filter.OpenOnly {
		// The states come from the one definition of "open":
		// fleet.LeaseState.Terminal.
		open := fleet.OpenLeaseStates()
		states := make([]any, 0, len(open))

		for _, state := range open {
			states = append(states, string(state))
		}

		cond.add(`state IN (`+placeholders(len(open))+`)`, states...)
	}

	query = `SELECT data FROM leases` + cond.where() + ` ORDER BY granted_at DESC, id DESC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`

		cond.args = append(cond.args, filter.Limit)
	}

	return query, cond.args
}

// directivesQuery builds the query of ListDirectives.
func directivesQuery(filter DirectiveFilter) (query string, args []any) {
	var cond conditions

	if filter.Host != "" {
		cond.add(`host = ?`, filter.Host)
	}

	if filter.PendingOnly {
		cond.add(`state = ?`, string(fleet.DirectivePending))
	}

	query = `SELECT id, host, kind, container, reason, state, rollout_id, error, created_by,
		created_at, updated_at FROM directives` + cond.where() + ` ORDER BY created_at, id`

	return query, cond.args
}

// ListRollouts returns rollouts with their hosts, newest first (by creation
// time). The hosts of a rollout are ordered by host name.
func (s *sqliteStore) ListRollouts(
	ctx context.Context, filter RolloutFilter,
) ([]*fleet.Rollout, error) {
	query, args := rolloutsQuery(filter)

	rollouts := make([]*fleet.Rollout, 0, 16)

	err := s.read(ctx, func(tx *sql.Tx) error {
		err := queryRows(ctx, tx, query, args, func(row rowScanner) error {
			rollout, err := scanRollout(row)
			if err != nil {
				return err
			}

			rollouts = append(rollouts, rollout)

			return nil
		})
		if err != nil {
			return err
		}

		return loadRolloutHosts(ctx, tx, rollouts)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list rollouts: %w", err)
	}

	return rollouts, nil
}

// GetRollout returns one rollout or ErrNotFound.
func (s *sqliteStore) GetRollout(ctx context.Context, id string) (*fleet.Rollout, error) {
	var rollout *fleet.Rollout

	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error

		rollout, err = scanRollout(
			tx.QueryRowContext(ctx, `SELECT data FROM rollouts WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		return loadRolloutHosts(ctx, tx, []*fleet.Rollout{rollout})
	})
	if err != nil {
		return nil, fmt.Errorf("store: get rollout %s: %w", id, err)
	}

	return rollout, nil
}

func scanRollout(row rowScanner) (*fleet.Rollout, error) {
	var data string
	if err := row.Scan(&data); err != nil {
		return nil, err
	}

	rollout := &fleet.Rollout{}
	if err := json.Unmarshal([]byte(data), rollout); err != nil {
		return nil, fmt.Errorf("decode rollout: %w", err)
	}

	return rollout, nil
}

// loadRolloutHosts fills Hosts of every rollout from rollout_hosts.
func loadRolloutHosts(ctx context.Context, tx *sql.Tx, rollouts []*fleet.Rollout) error {
	if len(rollouts) == 0 {
		return nil
	}

	stmt, err := tx.PrepareContext(ctx, rolloutHostsQuery)
	if err != nil {
		return err
	}

	defer func() { _ = stmt.Close() }()

	for _, rollout := range rollouts {
		rollout.Hosts = make([]fleet.RolloutHost, 0, 8)

		rows, err := stmt.QueryContext(ctx, rollout.ID)
		if err != nil {
			return err
		}

		for rows.Next() {
			var (
				host      fleet.RolloutHost
				state     string
				stage     int
				updatedAt int64
			)

			err := rows.Scan(&host.Host, &state, &stage, &host.NeedID, &host.LeaseID,
				&host.Error, &updatedAt)
			if err != nil {
				return errors.Join(err, rows.Close())
			}

			host.State = fleet.HostState(state)
			host.Stage = fleet.Stage(stage)
			host.UpdatedAt = fromMillis(updatedAt)
			rollout.Hosts = append(rollout.Hosts, host)
		}

		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}

	return nil
}

// ListLeases returns leases, newest first (by grant time).
func (s *sqliteStore) ListLeases(ctx context.Context, filter LeaseFilter) ([]*fleet.Lease, error) {
	query, args := leasesQuery(filter)

	leases := make([]*fleet.Lease, 0, 16)

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, query, args, func(row rowScanner) error {
			lease, err := scanLease(row)
			if err != nil {
				return err
			}

			leases = append(leases, lease)

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list leases: %w", err)
	}

	return leases, nil
}

// GetLease returns one lease or ErrNotFound.
func (s *sqliteStore) GetLease(ctx context.Context, id string) (*fleet.Lease, error) {
	var lease *fleet.Lease

	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error

		lease, err = scanLease(tx.QueryRowContext(ctx, `SELECT data FROM leases WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: get lease %s: %w", id, err)
	}

	return lease, nil
}

func scanLease(row rowScanner) (*fleet.Lease, error) {
	var data string
	if err := row.Scan(&data); err != nil {
		return nil, err
	}

	lease := &fleet.Lease{}
	if err := json.Unmarshal([]byte(data), lease); err != nil {
		return nil, fmt.Errorf("decode lease: %w", err)
	}

	return lease, nil
}

// ListDirectives returns directives, oldest first (by creation time).
func (s *sqliteStore) ListDirectives(
	ctx context.Context, filter DirectiveFilter,
) ([]*fleet.Directive, error) {
	query, args := directivesQuery(filter)

	directives := make([]*fleet.Directive, 0, 16)

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, query, args, func(row rowScanner) error {
			var (
				d                    fleet.Directive
				kind, state          string
				createdAt, updatedAt int64
			)

			err := row.Scan(&d.ID, &d.Host, &kind, &d.Container, &d.Reason, &state,
				&d.RolloutID, &d.Error, &d.CreatedBy, &createdAt, &updatedAt)
			if err != nil {
				return err
			}

			d.Kind = protocol.DirectiveKind(kind)
			d.State = fleet.DirectiveState(state)
			d.CreatedAt = fromMillis(createdAt)
			d.UpdatedAt = fromMillis(updatedAt)
			directives = append(directives, &d)

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list directives: %w", err)
	}

	return directives, nil
}

// Commit applies a set of scheduler changes in one transaction: either all
// of it is stored or none. On success the IDs (and defaulted times) of the
// appended audit entries are written back into changes.Audit.
//
// Every time of a rollout, lease (including its Operation) or directive is
// stored in UTC with millisecond precision, the precision of the schema's time columns, and
// read back like that. The objects passed in are not changed, so a value
// that carried nanoseconds or another zone differs from its reloaded copy;
// compare with time.Time.Equal after truncating, or truncate before Commit.
func (s *sqliteStore) Commit(ctx context.Context, changes *Changes) error {
	if changes == nil || changes.Empty() {
		return nil
	}

	var audit []fleet.AuditEntry

	err := s.write(ctx, func(tx *sql.Tx) error {
		for i, rollout := range changes.Rollouts {
			if err := upsertRollout(ctx, tx, rollout); err != nil {
				return fmt.Errorf("rollout %d: %w", i, err)
			}
		}

		for i, lease := range changes.Leases {
			if err := upsertLease(ctx, tx, lease); err != nil {
				return fmt.Errorf("lease %d: %w", i, err)
			}
		}

		for i, directive := range changes.Directives {
			if err := upsertDirective(ctx, tx, directive); err != nil {
				return fmt.Errorf("directive %d: %w", i, err)
			}
		}

		for _, hold := range changes.Holds {
			if err := setHostHold(ctx, tx, hold.Host, hold.Wanted, hold.Reason, hold.By); err != nil {
				return fmt.Errorf("hold of %s: %w", hold.Host, err)
			}
		}

		var err error

		audit, err = insertAudit(ctx, tx, changes.Audit)

		return err
	})
	if err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}

	copy(changes.Audit, audit)

	return nil
}

func upsertRollout(ctx context.Context, tx *sql.Tx, rollout *fleet.Rollout) error {
	if rollout == nil {
		return errors.New("rollout is nil")
	}

	if rollout.ID == "" {
		return errors.New("rollout has no id")
	}

	// The member hosts live in rollout_hosts, not in the JSON.
	bare := *rollout
	bare.Hosts = nil
	bare.CreatedAt = storedTime(bare.CreatedAt)
	bare.UpdatedAt = storedTime(bare.UpdatedAt)
	bare.EndedAt = storedTimePtr(bare.EndedAt)

	data, err := json.Marshal(&bare)
	if err != nil {
		return fmt.Errorf("encode rollout %s: %w", rollout.ID, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO rollouts (id, kind, key, state, created_at, updated_at, ended_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			key = excluded.key,
			state = excluded.state,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			ended_at = excluded.ended_at,
			data = excluded.data`,
		rollout.ID, string(rollout.Kind), rollout.Key, string(rollout.State),
		millis(rollout.CreatedAt), millis(rollout.UpdatedAt), nullMillis(rollout.EndedAt),
		string(data))
	if err != nil {
		return fmt.Errorf("store rollout %s: %w", rollout.ID, err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM rollout_hosts WHERE rollout_id = ?`, rollout.ID)
	if err != nil {
		return fmt.Errorf("clear hosts of rollout %s: %w", rollout.ID, err)
	}

	err = insertBatched(ctx, tx, `
		INSERT INTO rollout_hosts
			(rollout_id, host, state, stage, need_id, lease_id, error, updated_at) VALUES `,
		8, len(rollout.Hosts),
		func(args []any, i int) []any {
			h := &rollout.Hosts[i]

			return append(args, rollout.ID, h.Host, string(h.State), int(h.Stage), h.NeedID,
				h.LeaseID, h.Error, millis(h.UpdatedAt))
		})
	if err != nil {
		return fmt.Errorf("store hosts of rollout %s: %w", rollout.ID, err)
	}

	return nil
}

func upsertLease(ctx context.Context, tx *sql.Tx, lease *fleet.Lease) error {
	if lease == nil {
		return errors.New("lease is nil")
	}

	if lease.ID == "" {
		return errors.New("lease has no id")
	}

	// The times in the JSON get the precision of the time columns.
	stored := *lease
	stored.GrantedAt = storedTime(stored.GrantedAt)
	stored.ExpiresAt = storedTime(stored.ExpiresAt)
	stored.AgentDoneAt = storedTimePtr(stored.AgentDoneAt)
	stored.SyncedSince = storedTimePtr(stored.SyncedSince)
	stored.SyncedUntil = storedTimePtr(stored.SyncedUntil)
	stored.EndedAt = storedTimePtr(stored.EndedAt)

	if lease.Cancelled != nil {
		cancelled := *lease.Cancelled
		cancelled.At = storedTime(cancelled.At)
		stored.Cancelled = &cancelled
	}

	if lease.Operation != nil {
		operation := *lease.Operation
		operation.StartedAt = storedTime(operation.StartedAt)
		operation.UpdatedAt = storedTime(operation.UpdatedAt)
		stored.Operation = &operation
	}

	data, err := json.Marshal(&stored)
	if err != nil {
		return fmt.Errorf("encode lease %s: %w", lease.ID, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO leases (id, host, state, granted_at, expires_at, ended_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			host = excluded.host,
			state = excluded.state,
			granted_at = excluded.granted_at,
			expires_at = excluded.expires_at,
			ended_at = excluded.ended_at,
			data = excluded.data`,
		lease.ID, lease.Host, string(lease.State), millis(lease.GrantedAt),
		millis(lease.ExpiresAt), nullMillis(lease.EndedAt), string(data))
	if err != nil {
		return fmt.Errorf("store lease %s: %w", lease.ID, err)
	}

	return nil
}

func upsertDirective(ctx context.Context, tx *sql.Tx, d *fleet.Directive) error {
	if d == nil {
		return errors.New("directive is nil")
	}

	if d.ID == "" {
		return errors.New("directive has no id")
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO directives (id, host, kind, container, reason, state, rollout_id, error,
			created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			host = excluded.host,
			kind = excluded.kind,
			container = excluded.container,
			reason = excluded.reason,
			state = excluded.state,
			rollout_id = excluded.rollout_id,
			error = excluded.error,
			created_by = excluded.created_by,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`,
		d.ID, d.Host, string(d.Kind), d.Container, d.Reason, string(d.State), d.RolloutID,
		d.Error, d.CreatedBy, millis(d.CreatedAt), millis(d.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store directive %s: %w", d.ID, err)
	}

	return nil
}
