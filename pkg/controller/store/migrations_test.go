package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
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

// Step 2 closes the open rollouts of roll groups, drops the broken marks of
// the old units and the policy of the default group; everything else stays.
func TestMigrateContainerLabels(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgradoor.db")

	steps, err := loadMigrations()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(steps), 2)

	s, ok := New(quietLog(), path).(*sqliteStore)
	require.True(t, ok)

	db := fileDB(t, path)
	require.NoError(t, s.migrate(ctx, db, steps[:1]))

	_, err = db.Exec(`
		INSERT INTO hosts (name, cert_hash, first_seen, last_seen, broken)
		VALUES ('node-1', 'h', 1, 1, '{"group:execution#geth":"l-1","packages":"l-2"}');
		INSERT INTO rollouts (id, kind, key, state, created_at, updated_at, data) VALUES
			('r-open', 'group', 'group:execution#geth@aa', 'active', 1, 1767225600000,
			 '{"id":"r-open","kind":"group","key":"group:execution#geth@aa","state":"active","group":"execution","client":"geth","hosts":[]}'),
			('r-done', 'group', 'group:execution#geth@bb', 'completed', 1, 2, '{"id":"r-done","kind":"group","state":"completed"}'),
			('r-pkg', 'packages', 'packages:x', 'active', 1, 2, '{"id":"r-pkg","kind":"packages","state":"active"}');
		INSERT INTO settings (key, value) VALUES ('policyDefaultGroup', '"manual"'), ('policyImage', '"manual"');`)
	require.NoError(t, err)

	require.NoError(t, s.migrate(ctx, db, steps))

	require.NoError(t, s.Start(ctx))
	t.Cleanup(func() { _ = s.Stop() })

	r, err := s.GetRollout(ctx, "r-open")
	require.NoError(t, err)
	assert.Equal(t, fleet.RolloutSuperseded, r.State)
	assert.Contains(t, r.Reason, "kinds and teams")
	require.NotNil(t, r.EndedAt)
	assert.Equal(t, time.UnixMilli(1767225600000).UTC(), r.EndedAt.UTC())

	for id, want := range map[string]fleet.RolloutState{"r-done": fleet.RolloutCompleted, "r-pkg": fleet.RolloutActive} {
		r, err = s.GetRollout(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, r.State, id)
	}

	hosts, err := s.ListHosts(ctx)
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	assert.Equal(t, map[string]string{"packages": "l-2"}, hosts[0].Broken)

	settings, err := s.LoadSettings(ctx, fleet.DefaultSettings())
	require.NoError(t, err, "the removed setting is gone")
	assert.Equal(t, fleet.PolicyManual, settings.PolicyImage)
}

// Step 3 drops the controller's hold columns, the holds in them with them:
// the agents own their holds. The offline mark loses its "by". The removed
// readyEpochs setting goes.
func TestMigrateHolds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgradoor.db")

	steps, err := loadMigrations()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(steps), 3)

	s, ok := New(quietLog(), path).(*sqliteStore)
	require.True(t, ok)

	db := fileDB(t, path)
	require.NoError(t, s.migrate(ctx, db, steps[:2]))

	_, err = db.Exec(`
		INSERT INTO hosts (name, cert_hash, first_seen, last_seen, hold_wanted, hold_reason, hold_by, hold_actor, offline)
		VALUES
			('free', 'h', 1, 1, 0, '', '', '', ''),
			('teams', 'h', 1, 1, 1, 'debugging', 'team:reth,geth', 'bob',
			 '{"reason":"broken on purpose","by":"team"}');
		INSERT INTO settings (key, value) VALUES ('readyEpochs', '2'), ('paused', 'true');`)
	require.NoError(t, err)

	require.NoError(t, s.migrate(ctx, db, steps))

	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('hosts') WHERE name LIKE 'hold%'`)
	require.NoError(t, err)

	var left []string

	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		left = append(left, name)
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Empty(t, left, "no hold column is left")

	require.NoError(t, s.Start(ctx))
	t.Cleanup(func() { _ = s.Stop() })

	hosts, err := s.ListHosts(ctx)
	require.NoError(t, err)
	require.Len(t, hosts, 2)

	byName := make(map[string]*fleet.Host, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
		assert.False(t, h.Held(), h.Name)
	}

	assert.Equal(t, &fleet.OfflineMark{Reason: "broken on purpose"}, byName["teams"].Offline)

	settings, err := s.LoadSettings(ctx, fleet.DefaultSettings())
	require.NoError(t, err, "the removed setting is gone")
	assert.True(t, settings.Paused)
}

// Step 3 marks the members a lease was granted for as served.
func TestMigrateServedMembers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgradoor.db")

	steps, err := loadMigrations()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(steps), 3)

	s, ok := New(quietLog(), path).(*sqliteStore)
	require.True(t, ok)

	db := fileDB(t, path)
	require.NoError(t, s.migrate(ctx, db, steps[:2]))

	_, err = db.Exec(`
		INSERT INTO rollouts (id, kind, key, state, created_at, updated_at, data)
		VALUES ('r', 'packages', 'packages:x', 'active', 1, 2, '{"id":"r","kind":"packages","state":"active"}');
		INSERT INTO rollout_hosts (rollout_id, host, state, stage, lease_id, updated_at) VALUES
			('r', 'released', 'done', 3, 'l-1', 1),
			('r', 'leased', 'leased', 3, 'l-2', 1),
			('r', 'failed', 'failed', 3, '', 1),
			('r', 'cancelled', 'cancelled', 3, '', 1),
			('r', 'waiting', 'pending', 3, '', 1),
			('r', 'gone', 'done', 3, '', 1);`)
	require.NoError(t, err)

	require.NoError(t, s.migrate(ctx, db, steps))

	require.NoError(t, s.Start(ctx))
	t.Cleanup(func() { _ = s.Stop() })

	r, err := s.GetRollout(ctx, "r")
	require.NoError(t, err)

	served := make(map[string]bool, len(r.Hosts))
	for _, m := range r.Hosts {
		served[m.Host] = m.Served
	}

	assert.Equal(t, map[string]bool{
		"released": true, "leased": true, "failed": true, "cancelled": true, "waiting": false, "gone": false,
	}, served)
}

// Step 5 arms the deployment window of a store that serves a running fleet
// and opens none on a fresh one.
func TestMigrateDeployWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	steps, err := loadMigrations()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(steps), 5)

	for _, tt := range []struct {
		name  string
		rows  string
		armed bool
	}{
		{name: "fresh"},
		{name: "hosts", rows: `INSERT INTO hosts (name, cert_hash, first_seen, last_seen) VALUES ('a', 'h', 1, 1)`, armed: true},
		{name: "rollouts", rows: `INSERT INTO rollouts (id, kind, key, state, created_at, updated_at, data)
			VALUES ('r', 'packages', 'packages:x', 'completed', 1, 2, '{"id":"r"}')`, armed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "upgradoor.db")
			s, ok := New(quietLog(), path).(*sqliteStore)
			require.True(t, ok)

			db := fileDB(t, path)
			require.NoError(t, s.migrate(ctx, db, steps[:4]))

			if tt.rows != "" {
				_, err := db.Exec(tt.rows)
				require.NoError(t, err)
			}

			before := time.Now().Add(-time.Second)

			require.NoError(t, s.migrate(ctx, db, steps))
			require.NoError(t, s.Start(ctx))
			t.Cleanup(func() { _ = s.Stop() })

			w, err := s.DeployWindow(ctx)
			require.NoError(t, err)

			if !tt.armed {
				assert.Equal(t, fleet.DeployWindowState{}, w, "the controller opens the window")

				return
			}

			require.NotNil(t, w.ArmedAt)
			assert.WithinRange(t, *w.ArmedAt, before, time.Now().Add(time.Second))
			assert.Equal(t, *w.ArmedAt, w.StartedAt)

			w, err = s.StartDeployWindow(ctx, time.Now())
			require.NoError(t, err)
			assert.NotNil(t, w.ArmedAt, "the armed window stays armed")
		})
	}
}
