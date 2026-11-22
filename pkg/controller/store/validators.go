package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// ValidatorRanges returns the stored validator ranges inventory.
func (s *sqliteStore) ValidatorRanges(ctx context.Context) (*fleet.ValidatorRanges, error) {
	out := &fleet.ValidatorRanges{}

	err := s.read(ctx, func(tx *sql.Tx) error {
		var fetchedAt int64

		err := tx.QueryRowContext(ctx, `
			SELECT url, etag, last_modified, fetched_at FROM validator_ranges_source`).
			Scan(&out.URL, &out.ETag, &out.LastModified, &fetchedAt)

		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			out.FetchedAt = fromMillis(fetchedAt)
		}

		return queryRows(ctx, tx,
			`SELECT start_index, end_index, host FROM validator_ranges ORDER BY start_index`, nil,
			func(row rowScanner) error {
				var (
					r          fleet.HostRange
					start, end int64
				)

				if scanErr := row.Scan(&start, &end, &r.Host); scanErr != nil {
					return scanErr
				}

				r.ValidatorRange = identity.ValidatorRange{Start: uint64(start), End: uint64(end)}
				out.Ranges = append(out.Ranges, r)

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: validator ranges: %w", err)
	}

	return out, nil
}

// SetValidatorRanges replaces the stored validator ranges inventory.
func (s *sqliteStore) SetValidatorRanges(ctx context.Context, ranges *fleet.ValidatorRanges) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM validator_ranges`); err != nil {
			return err
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO validator_ranges_source (id, url, etag, last_modified, fetched_at)
			VALUES (1, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET url = excluded.url, etag = excluded.etag,
				last_modified = excluded.last_modified, fetched_at = excluded.fetched_at`,
			ranges.URL, ranges.ETag, ranges.LastModified, millis(ranges.FetchedAt))
		if err != nil {
			return err
		}

		if len(ranges.Ranges) == 0 {
			return nil
		}

		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO validator_ranges (start_index, end_index, host) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}

		defer func() { _ = stmt.Close() }()

		for i := range ranges.Ranges {
			r := &ranges.Ranges[i]
			if _, err := stmt.ExecContext(ctx, int64(r.Start), int64(r.End), r.Host); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("store: set validator ranges: %w", err)
	}

	return nil
}

// hostValidators returns the stored validator ranges by host, ordered by
// start: of the one host when name is set, else of all.
func hostValidators(
	ctx context.Context, tx *sql.Tx, name string,
) (map[string][]identity.ValidatorRange, error) {
	query := `SELECT host, start_index, end_index FROM validator_ranges ORDER BY start_index`
	args := []any(nil)

	if name != "" {
		query = `SELECT host, start_index, end_index FROM validator_ranges
			WHERE host = ? ORDER BY start_index`
		args = []any{name}
	}

	out := make(map[string][]identity.ValidatorRange, 64)

	err := queryRows(ctx, tx, query, args, func(row rowScanner) error {
		var (
			host       string
			start, end int64
		)

		if err := row.Scan(&host, &start, &end); err != nil {
			return err
		}

		out[host] = append(out[host], identity.ValidatorRange{Start: uint64(start), End: uint64(end)})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("validator ranges: %w", err)
	}

	return out, nil
}
