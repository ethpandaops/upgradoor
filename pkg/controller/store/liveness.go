package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// SaveLiveness upserts per-host liveness rows by (host, epoch).
func (s *sqliteStore) SaveLiveness(ctx context.Context, rows []fleet.Liveness) error {
	if len(rows) == 0 {
		return nil
	}

	err := s.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO validator_liveness (host, epoch, live, total) VALUES (?, ?, ?, ?)
			ON CONFLICT(host, epoch) DO UPDATE SET
				live = excluded.live,
				total = excluded.total`)
		if err != nil {
			return err
		}

		defer func() { _ = stmt.Close() }()

		for i := range rows {
			row := &rows[i]

			_, err := stmt.ExecContext(ctx,
				row.Host, int64(row.Epoch), int64(row.Live), int64(row.Total))
			if err != nil {
				return fmt.Errorf("host %s epoch %d: %w", row.Host, row.Epoch, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("store: save liveness: %w", err)
	}

	return nil
}

// ListLiveness returns the rows of the newest `epochs` epochs for which any
// row exists, newest epoch first and by host within an epoch.
func (s *sqliteStore) ListLiveness(ctx context.Context, epochs int) ([]fleet.Liveness, error) {
	result := make([]fleet.Liveness, 0, 64)

	if epochs <= 0 {
		return result, nil
	}

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `
			SELECT host, epoch, live, total FROM validator_liveness
			WHERE epoch IN (
				SELECT DISTINCT epoch FROM validator_liveness ORDER BY epoch DESC LIMIT ?)
			ORDER BY epoch DESC, host`,
			[]any{epochs},
			func(row rowScanner) error {
				var (
					host               string
					epoch, live, total int64
				)

				if err := row.Scan(&host, &epoch, &live, &total); err != nil {
					return err
				}

				result = append(result, fleet.Liveness{
					Host:  host,
					Epoch: uint64(epoch),
					Live:  uint64(live),
					Total: uint64(total),
				})

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list liveness: %w", err)
	}

	return result, nil
}

// PruneLiveness deletes rows older than beforeEpoch.
func (s *sqliteStore) PruneLiveness(ctx context.Context, beforeEpoch uint64) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM validator_liveness WHERE epoch < ?`, int64(beforeEpoch))

		return err
	})
	if err != nil {
		return fmt.Errorf("store: prune liveness: %w", err)
	}

	return nil
}
