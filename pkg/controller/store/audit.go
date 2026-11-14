package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

const (
	// DefaultAuditLimit is the number of entries ListAudit returns when the
	// filter has no limit.
	DefaultAuditLimit = 100
	// MaxAuditLimit caps the number of entries ListAudit returns.
	MaxAuditLimit = 1000
)

// AppendAudit appends entries in one transaction; IDs are assigned in
// argument order and a zero Time is set to now.
func (s *sqliteStore) AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error {
	if len(entries) == 0 {
		return nil
	}

	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := insertAudit(ctx, tx, entries)

		return err
	})
	if err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}

	return nil
}

// insertAudit inserts entries and returns them as stored: with their ID and
// with a zero Time replaced by now. The input is not modified.
func insertAudit(
	ctx context.Context, tx *sql.Tx, entries []fleet.AuditEntry,
) ([]fleet.AuditEntry, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO audit (time, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}

	defer func() { _ = stmt.Close() }()

	now := time.Now()
	stored := make([]fleet.AuditEntry, 0, len(entries))

	for i := range entries {
		entry := entries[i]
		if entry.Time.IsZero() {
			entry.Time = now
		}

		var detail sql.NullString

		if entry.Detail != nil {
			raw, err := json.Marshal(entry.Detail)
			if err != nil {
				return nil, fmt.Errorf("audit entry %d (%s): encode detail: %w", i, entry.Action, err)
			}

			detail = sql.NullString{String: string(raw), Valid: true}
		}

		res, err := stmt.ExecContext(ctx,
			millis(entry.Time), entry.Actor, entry.Action, entry.Target, detail)
		if err != nil {
			return nil, fmt.Errorf("audit entry %d (%s): %w", i, entry.Action, err)
		}

		entry.ID, err = res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("audit entry %d (%s): %w", i, entry.Action, err)
		}

		stored = append(stored, entry)
	}

	return stored, nil
}

// ListAudit returns entries, newest first. Limit 0 means DefaultAuditLimit;
// it is capped at MaxAuditLimit.
func (s *sqliteStore) ListAudit(
	ctx context.Context, filter AuditFilter,
) ([]fleet.AuditEntry, error) {
	limit := filter.Limit

	switch {
	case limit <= 0:
		limit = DefaultAuditLimit
	case limit > MaxAuditLimit:
		limit = MaxAuditLimit
	}

	var cond conditions

	if filter.Target != "" {
		cond.add(`target = ?`, filter.Target)
	}

	if filter.ActionPrefix != "" {
		// substr compares exactly (LIKE would ignore case and need
		// escaping); it counts characters, as does length.
		cond.add(`substr(action, 1, length(?)) = ?`, filter.ActionPrefix, filter.ActionPrefix)
	}

	if filter.BeforeID > 0 {
		cond.add(`id < ?`, filter.BeforeID)
	}

	query := `SELECT id, time, actor, action, target, detail FROM audit` + cond.where() +
		` ORDER BY id DESC LIMIT ?`
	args := make([]any, 0, len(cond.args)+1)
	args = append(append(args, cond.args...), limit)

	entries := make([]fleet.AuditEntry, 0, min(limit, DefaultAuditLimit))

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, query, args, func(row rowScanner) error {
			var (
				entry  fleet.AuditEntry
				at     int64
				detail sql.NullString
			)

			err := row.Scan(&entry.ID, &at, &entry.Actor, &entry.Action, &entry.Target, &detail)
			if err != nil {
				return err
			}

			entry.Time = fromMillis(at)

			if detail.Valid {
				if err := json.Unmarshal([]byte(detail.String), &entry.Detail); err != nil {
					return fmt.Errorf("decode detail of entry %d: %w", entry.ID, err)
				}
			}

			entries = append(entries, entry)

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}

	return entries, nil
}
