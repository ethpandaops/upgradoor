package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// ErrSchemaTooNew is returned by Start when the database was written by a
// newer binary than this one. Nothing is changed in that case.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this binary")

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one schema step. Applying it moves the database from
// version-1 to version.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the embedded migrations. Files are named
// "<version>_<name>.sql"; versions must be 1, 2, 3, ... without gaps.
func loadMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}

	migrations := make([]migration, 0, len(names))

	for _, path := range names {
		name := strings.TrimPrefix(path, "migrations/")

		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q: name must be <version>_<name>.sql", name)
		}

		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("store: migration %q: bad version %q", name, prefix)
		}

		raw, err := fs.ReadFile(files, path)
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", name, err)
		}

		migrations = append(migrations, migration{version: version, name: name, sql: string(raw)})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	for i, m := range migrations {
		if m.version != i+1 {
			return nil, fmt.Errorf(
				"store: migration %q: expected version %d, versions must be contiguous", m.name, i+1)
		}
	}

	return migrations, nil
}

// migrate brings the database to the newest schema version and returns it.
// The version is kept in PRAGMA user_version, which is part of the database
// file and transactional: each step and its version bump commit together, so
// a crash leaves the database at a whole version and a restart continues
// from there.
func migrate(ctx context.Context, log logrus.FieldLogger, db *sql.DB) (int, error) {
	migrations, err := loadMigrations(migrationFiles)
	if err != nil {
		return 0, err
	}

	return applyMigrations(ctx, log, db, migrations)
}

func applyMigrations(
	ctx context.Context, log logrus.FieldLogger, db *sql.DB, migrations []migration,
) (int, error) {
	var current int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}

	if current > len(migrations) {
		return current, fmt.Errorf(
			"%w: database is at version %d, binary knows up to %d",
			ErrSchemaTooNew, current, len(migrations))
	}

	for _, m := range migrations[current:] {
		err := inTx(ctx, db, func(tx *sql.Tx) error {
			// The version is re-read inside the write transaction so two
			// processes starting on the same file cannot both apply a step.
			var at int
			if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&at); err != nil {
				return err
			}

			if at >= m.version {
				return nil
			}

			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}

			// PRAGMA takes no bound parameters; the value is an int.
			_, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version))

			return err
		})
		if err != nil {
			return current, fmt.Errorf("store: apply migration %s: %w", m.name, err)
		}

		current = m.version

		log.WithField("migration", m.name).Info("applied database migration")
	}

	return current, nil
}
