package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// SaveSlotParticipation upserts slot rows by slot and replaces the per-host
// rows of every slot given.
func (s *sqliteStore) SaveSlotParticipation(ctx context.Context, rows []fleet.SlotParticipation) error {
	if len(rows) == 0 {
		return nil
	}

	err := s.write(ctx, func(tx *sql.Tx) error {
		for i := range rows {
			if err := saveSlot(ctx, tx, &rows[i]); err != nil {
				return fmt.Errorf("slot %d: %w", rows[i].Slot, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("store: save slot participation: %w", err)
	}

	return nil
}

func saveSlot(ctx context.Context, tx *sql.Tx, row *fleet.SlotParticipation) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO slot_participation (slot, proposed, assigned, included) VALUES (?, ?, ?, ?)
		ON CONFLICT(slot) DO UPDATE SET
			proposed = excluded.proposed,
			assigned = excluded.assigned,
			included = excluded.included`,
		int64(row.Slot), row.Proposed, int64(row.Assigned), int64(row.Included))
	if err != nil {
		return err
	}

	// Only the hosts that missed attestations in the slot have a row; a late
	// inclusion takes one away again. Replacing them is simplest.
	_, err = tx.ExecContext(ctx,
		`DELETE FROM slot_participation_hosts WHERE slot = ?`, int64(row.Slot))
	if err != nil {
		return err
	}

	for i := range row.Hosts {
		host := &row.Hosts[i]
		if host.Included >= host.Assigned {
			// Nothing missed: nothing to say about the host.
			continue
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO slot_participation_hosts (slot, host, assigned, included)
			VALUES (?, ?, ?, ?)`,
			int64(row.Slot), host.Host, int64(host.Assigned), int64(host.Included))
		if err != nil {
			return fmt.Errorf("host %s: %w", host.Host, err)
		}
	}

	return nil
}

// ListSlotParticipation returns the slot rows from fromSlot on, oldest first,
// without their per-host part.
func (s *sqliteStore) ListSlotParticipation(
	ctx context.Context, fromSlot uint64,
) ([]fleet.SlotParticipation, error) {
	result := make([]fleet.SlotParticipation, 0, 256)

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `
			SELECT slot, proposed, assigned, included FROM slot_participation
			WHERE slot >= ? ORDER BY slot`, []any{int64(fromSlot)},
			func(row rowScanner) error {
				var (
					slot, assigned, included int64
					proposed                 bool
				)

				if err := row.Scan(&slot, &proposed, &assigned, &included); err != nil {
					return err
				}

				result = append(result, fleet.SlotParticipation{
					Slot:     uint64(slot),
					Proposed: proposed,
					Assigned: uint64(assigned),
					Included: uint64(included),
				})

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list slot participation: %w", err)
	}

	return result, nil
}

// SumHostSlotParticipation sums the per-host rows from fromSlot on over
// buckets of bucketSlots slots (bucket = slot / bucketSlots), ordered by
// bucket and host. The sums are made in the database: a long window on a
// fleet of many hosts is hundreds of thousands of rows.
func (s *sqliteStore) SumHostSlotParticipation(
	ctx context.Context, fromSlot, bucketSlots uint64,
) ([]fleet.HostSlotBucket, error) {
	result := make([]fleet.HostSlotBucket, 0, 256)

	if bucketSlots == 0 {
		return result, nil
	}

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `
			SELECT slot / ? AS bucket, host, SUM(assigned), SUM(included)
			FROM slot_participation_hosts
			WHERE slot >= ? GROUP BY bucket, host ORDER BY bucket, host`,
			[]any{int64(bucketSlots), int64(fromSlot)},
			func(row rowScanner) error {
				var (
					host                       string
					bucket, assigned, included int64
				)

				if err := row.Scan(&bucket, &host, &assigned, &included); err != nil {
					return err
				}

				result = append(result, fleet.HostSlotBucket{
					Bucket:   uint64(bucket),
					Host:     host,
					Assigned: uint64(assigned),
					Included: uint64(included),
				})

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: sum host slot participation: %w", err)
	}

	return result, nil
}

// SumHostSlotRuns sums the rows in the database per host and stretch of gap
// slots (rows that close are in one run anyway) and joins those pieces into
// runs: a host that was down for the whole window costs one row per gap
// slots, not one per slot.
func (s *sqliteStore) SumHostSlotRuns(
	ctx context.Context, fromSlot, toSlot, gap, minSlots uint64, perSlot []string,
) ([]fleet.HostSlotRun, error) {
	if perSlot == nil {
		perSlot = []string{}
	}

	split, err := json.Marshal(perSlot)
	if err != nil {
		return nil, fmt.Errorf("store: sum host slot runs: %w", err)
	}

	pieces := make([]fleet.HostSlotRun, 0, 256)

	err = s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `
			SELECT host, MIN(slot), MAX(slot), COUNT(*), SUM(assigned), SUM(included)
			FROM slot_participation_hosts INDEXED BY slot_participation_hosts_host
			WHERE slot >= ? AND slot <= ? AND included < assigned
			GROUP BY host, CASE WHEN host IN (SELECT value FROM json_each(?)) THEN slot ELSE slot / ? END
			ORDER BY host, MIN(slot)`,
			[]any{int64(fromSlot), int64(toSlot), string(split), int64(max(gap, 1))},
			func(row rowScanner) error {
				var (
					r                                      fleet.HostSlotRun
					first, last, slots, assigned, included int64
				)

				if scanErr := row.Scan(&r.Host, &first, &last, &slots, &assigned, &included); scanErr != nil {
					return scanErr
				}

				r.First, r.Last, r.Slots = uint64(first), uint64(last), uint64(slots)
				r.Assigned, r.Included = uint64(assigned), uint64(included)
				pieces = append(pieces, r)

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: sum host slot runs: %w", err)
	}

	return joinSlotRuns(pieces, gap, minSlots, perSlot), nil
}

// joinSlotRuns joins pieces of a host (ordered by host and first slot) that
// lie no more than gap slots apart, and keeps the runs of at least minSlots
// slots and every piece of a host in perSlot.
func joinSlotRuns(pieces []fleet.HostSlotRun, gap, minSlots uint64, perSlot []string) []fleet.HostSlotRun {
	runs := make([]fleet.HostSlotRun, 0, len(pieces)/4+1)

	for i := range pieces {
		p := pieces[i]
		last := len(runs) - 1

		if last >= 0 && runs[last].Host == p.Host && p.First-runs[last].Last <= gap &&
			!slices.Contains(perSlot, p.Host) {
			runs[last].Last = p.Last
			runs[last].Slots += p.Slots
			runs[last].Assigned += p.Assigned
			runs[last].Included += p.Included

			continue
		}

		runs = append(runs, p)
	}

	kept := runs[:0]

	for _, r := range runs {
		if r.Slots >= minSlots || slices.Contains(perSlot, r.Host) {
			kept = append(kept, r)
		}
	}

	return kept
}

// PruneSlotParticipation deletes the rows of slots older than beforeSlot.
func (s *sqliteStore) PruneSlotParticipation(ctx context.Context, beforeSlot uint64) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM slot_participation WHERE slot < ?`, int64(beforeSlot))
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx,
			`DELETE FROM slot_participation_hosts WHERE slot < ?`, int64(beforeSlot))

		return err
	})
	if err != nil {
		return fmt.Errorf("store: prune slot participation: %w", err)
	}

	return nil
}
