package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMigrations(t *testing.T) {
	t.Parallel()

	steps, err := loadMigrations()
	require.NoError(t, err)
	require.NotEmpty(t, steps)

	for i, step := range steps {
		assert.Equal(t, i+1, step.version)
		assert.NotEmpty(t, step.name)
		assert.NotEmpty(t, step.sql)
	}

	assert.Equal(t, "init", steps[0].name)
}

// fileDB opens the database file at path next to a Store.
func fileDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open(driverName, "file:"+escapeURIPath(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()

	var version int

	require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))

	return version
}

func TestMigrate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	first := migration{version: 1, name: "init", sql: `
		CREATE TABLE things (name TEXT PRIMARY KEY);
		INSERT INTO things VALUES ('kept');`}
	second := migration{version: 2, name: "size", sql: `
		ALTER TABLE things ADD COLUMN size INTEGER NOT NULL DEFAULT 7;`}
	broken := migration{version: 2, name: "broken", sql: `
		ALTER TABLE things ADD COLUMN size INTEGER NOT NULL DEFAULT 7;
		INSERT INTO nowhere VALUES (1);`}

	open := func(t *testing.T) (*sqliteStore, *sql.DB) {
		t.Helper()

		path := filepath.Join(t.TempDir(), "upgradoor.db")
		s, ok := New(quietLog(), path).(*sqliteStore)
		require.True(t, ok)

		return s, fileDB(t, path)
	}

	sizeOf := func(t *testing.T, db *sql.DB) (size int, err error) {
		t.Helper()

		return size, db.QueryRow(`SELECT size FROM things WHERE name = 'kept'`).Scan(&size)
	}

	t.Run("an empty database gets every step", func(t *testing.T) {
		t.Parallel()

		s, db := open(t)
		require.NoError(t, s.migrate(ctx, db, []migration{first, second}))
		assert.Equal(t, 2, userVersion(t, db))

		size, err := sizeOf(t, db)
		require.NoError(t, err)
		assert.Equal(t, 7, size)
	})

	t.Run("an older database gets the missing steps and keeps its rows", func(t *testing.T) {
		t.Parallel()

		s, db := open(t)
		require.NoError(t, s.migrate(ctx, db, []migration{first}))
		assert.Equal(t, 1, userVersion(t, db))

		require.NoError(t, s.migrate(ctx, db, []migration{first, second}))
		assert.Equal(t, 2, userVersion(t, db))

		size, err := sizeOf(t, db)
		require.NoError(t, err)
		assert.Equal(t, 7, size)

		// Nothing is applied twice.
		require.NoError(t, s.migrate(ctx, db, []migration{first, second}))
		assert.Equal(t, 2, userVersion(t, db))
	})

	t.Run("a step that fails leaves the step before it", func(t *testing.T) {
		t.Parallel()

		s, db := open(t)
		err := s.migrate(ctx, db, []migration{first, broken})
		require.ErrorContains(t, err, "schema step 2 (broken)")
		assert.Equal(t, 1, userVersion(t, db))

		_, err = sizeOf(t, db)
		require.Error(t, err, "the failed step left nothing behind")
	})

	t.Run("a database of a newer binary is refused", func(t *testing.T) {
		t.Parallel()

		s, db := open(t)
		require.NoError(t, s.migrate(ctx, db, []migration{first, second}))

		err := s.migrate(ctx, db, []migration{first})
		require.ErrorIs(t, err, ErrSchemaVersion)
		require.ErrorContains(t, err, "newer upgradoor-controller")
		assert.Equal(t, 2, userVersion(t, db))
	})

	t.Run("tables without a step are refused", func(t *testing.T) {
		t.Parallel()

		s, db := open(t)
		_, err := db.Exec(`CREATE TABLE things (name TEXT PRIMARY KEY)`)
		require.NoError(t, err)

		err = s.migrate(ctx, db, []migration{first})
		require.ErrorIs(t, err, ErrSchemaVersion)
		assert.Equal(t, 0, userVersion(t, db))
	})
}

func TestStartRefusesANewerDatabase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgradoor.db")

	s := New(quietLog(), path)
	require.NoError(t, s.Start(ctx))
	addHost(t, s, "node-1", nil)
	require.NoError(t, s.Stop())

	steps, err := loadMigrations()
	require.NoError(t, err)

	db := fileDB(t, path)
	assert.Equal(t, len(steps), userVersion(t, db))

	_, err = db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(steps)+1))
	require.NoError(t, err)

	other := New(quietLog(), path)
	err = other.Start(ctx)
	require.ErrorIs(t, err, ErrSchemaVersion)
	require.ErrorContains(t, err, path)

	_, err = other.ListHosts(ctx)
	require.ErrorIs(t, err, ErrClosed, "a refused database is not left open")

	// The refused start changed nothing.
	var hosts int

	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM hosts`).Scan(&hosts))
	assert.Equal(t, len(steps)+1, userVersion(t, db))
	assert.Equal(t, 1, hosts)
}
