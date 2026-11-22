package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"

	"github.com/sirupsen/logrus"
)

// migrationFiles are the steps of the schema: NNNN_<name>.sql, numbered from
// 1 without a gap. Step 1 creates every table of an empty database; each
// further step changes the schema of the step before it. A released step is
// never edited.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// migration is one step of the schema.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations returns the embedded steps in order. It fails on a file
// that is not named as a step and on a gap in the numbering.
func loadMigrations() ([]migration, error) {
	// ReadDir returns the entries sorted by name, so in order of their number.
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read schema steps: %w", err)
	}

	steps := make([]migration, 0, len(entries))

	for _, entry := range entries {
		m := migrationName.FindStringSubmatch(entry.Name())
		if m == nil {
			return nil, fmt.Errorf("store: schema step %s is not named NNNN_<name>.sql", entry.Name())
		}

		version, err := strconv.Atoi(m[1])
		if err != nil || version != len(steps)+1 {
			return nil, fmt.Errorf("store: schema step %s is out of order: step %d is expected",
				entry.Name(), len(steps)+1)
		}

		raw, err := fs.ReadFile(migrationFiles, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read schema step %s: %w", entry.Name(), err)
		}

		steps = append(steps, migration{version: version, name: m[2], sql: string(raw)})
	}

	if len(steps) == 0 {
		return nil, fmt.Errorf("store: no schema steps")
	}

	return steps, nil
}

// migrate brings the database to the last of steps. PRAGMA user_version
// holds the number of the last step applied; an empty database is at 0. Each
// missing step is one write transaction together with its number, so a step
// that fails leaves the database at the step before it, and two processes
// starting on the same file cannot both apply one. A database with a higher
// number than this binary knows, and one that holds tables without a number,
// is refused unchanged.
func (s *sqliteStore) migrate(ctx context.Context, db *sql.DB, steps []migration) error {
	for {
		applied, err := s.migrateStep(ctx, db, steps)
		if err != nil {
			return err
		}

		if applied == nil {
			return nil
		}

		s.log.WithFields(logrus.Fields{"step": applied.version, "name": applied.name}).
			Info("database schema upgraded")
	}
}

// migrateStep applies the next missing step and returns it, or nil when the
// database is at the last one.
func (s *sqliteStore) migrateStep(ctx context.Context, db *sql.DB, steps []migration) (*migration, error) {
	var applied *migration

	err := inTx(ctx, db, func(tx *sql.Tx) error {
		var version, objects int

		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("store: read schema version: %w", err)
		}

		switch {
		case version == len(steps):
			return nil
		case version < 0 || version > len(steps):
			return fmt.Errorf("%w: %s is at schema step %d, this binary knows %d: "+
				"it was written by a newer upgradoor-controller",
				ErrSchemaVersion, s.path, version, len(steps))
		case version == 0:
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master`).Scan(&objects); err != nil {
				return fmt.Errorf("store: read schema: %w", err)
			}

			if objects != 0 {
				return fmt.Errorf("%w: %s holds tables but no schema step: "+
					"it is not a database of upgradoor-controller", ErrSchemaVersion, s.path)
			}
		}

		step := &steps[version]

		if _, err := tx.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("store: schema step %d (%s): %w", step.version, step.name, err)
		}

		// PRAGMA takes no bound parameters; the value is an int.
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(step.version)); err != nil {
			return fmt.Errorf("store: set schema version %d: %w", step.version, err)
		}

		applied = step

		return nil
	})
	if err != nil {
		return nil, err
	}

	return applied, nil
}
