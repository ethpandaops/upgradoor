package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// base is the reference time of the fixtures: UTC, whole milliseconds, so it
// survives the INTEGER millisecond columns unchanged.
var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// at returns base plus sec seconds.
func at(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

func quietLog() logrus.FieldLogger {
	log, _ := logrustest.NewNullLogger()

	return log
}

// startStore starts s and stops it when the test ends.
func startStore(t *testing.T, s Store) Store {
	t.Helper()

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { assert.NoError(t, s.Stop()) })

	return s
}

// newFileStore returns a started store on a fresh database file.
func newFileStore(t *testing.T) Store {
	t.Helper()

	return startStore(t, New(quietLog(), filepath.Join(t.TempDir(), "upgradoor.db")))
}

// backends are the two kinds of database a test runs against.
var backends = []struct {
	name string
	open func(t *testing.T) Store
}{
	{"file", newFileStore},
	{"memory", newMemoryStore},
}

// newMemoryStore returns a started store on a private in-memory database.
func newMemoryStore(t *testing.T) Store {
	t.Helper()

	return startStore(t, NewMemory(quietLog()))
}

// rawDB gives a test direct SQL access to the store's database.
func rawDB(t *testing.T, s Store) *sql.DB {
	t.Helper()

	impl, ok := s.(*sqliteStore)
	require.True(t, ok)

	return impl.writer
}

func countRows(t *testing.T, s Store, table string) int {
	t.Helper()

	var n int

	require.NoError(t, rawDB(t, s).QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n))

	return n
}

func testCert(name string, validators *identity.ValidatorRange, roles ...string) *identity.Certificate {
	return &identity.Certificate{
		Claims: identity.Claims{
			Issuer:     identity.Issuer,
			Network:    "testnet-0",
			Subject:    name,
			Generation: 1,
			Validators: validators,
			Roles:      roles,
		},
		Raw:  "raw-" + name,
		Hash: "hash-" + name,
	}
}

func testRequest(needs ...protocol.Need) *protocol.SyncRequest {
	return &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    "v0.1.0",
		SentAt:          at(0),
		BootID:          "boot-1",
		UptimeSeconds:   3600,
		Verbs:           []protocol.Verb{protocol.VerbRoll, protocol.VerbReboot},
		Hold:            protocol.Hold{Local: true, LocalReason: "debugging"},
		Containers: []protocol.Container{{
			Name:      "geth",
			ID:        "abc",
			Image:     "ethpandaops/geth:master",
			ImageID:   "sha256:1",
			Digest:    "sha256:2",
			State:     "running",
			StartedAt: at(-600),
			Managed:   true,
			Kind:      protocol.KindEL,
			Registry:  &protocol.RegistryCheck{Digest: "sha256:3", CheckedAt: at(-5)},
		}},
		Node: protocol.NodeHealth{
			EL: &protocol.ELHealth{Reachable: true, Peers: 12, Head: 100},
			CL: &protocol.CLHealth{Reachable: true, Peers: 30, HeadSlot: 3200},
		},
		OS: protocol.OSSummary{
			ID: "debian", VersionID: "13", PrettyName: "Debian 13", Arch: "amd64",
			RunningKernel: "6.12.1", NewestKernel: "6.12.2", RebootRequired: true,
			RebootPackages: []string{"linux-image-amd64"},
		},
		Pending:       protocol.Pending{Upgradable: 4, Security: 1, Blocked: 2},
		InventoryHash: "inv-hash",
		Needs:         needs,
	}
}

func imageNeed(id string) protocol.Need {
	return protocol.Need{
		ID:   id,
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: "geth", Image: "ethpandaops/geth:master",
			FromDigest: "sha256:2", ToDigest: "sha256:3",
		},
	}
}

// addHost registers a host through RecordSync.
func addHost(t *testing.T, s Store, name string, validators *identity.ValidatorRange) *fleet.Host {
	t.Helper()

	res, err := s.RecordSync(context.Background(), &HostSync{
		Cert:       testCert(name, validators),
		RemoteAddr: "10.0.0.1:1234",
		Request:    testRequest(),
		At:         at(0),
	})
	require.NoError(t, err)

	return res.Host
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("methods fail before start and after stop", func(t *testing.T) {
		s := New(quietLog(), filepath.Join(t.TempDir(), "x.db"))

		_, err := s.ListHosts(ctx)
		require.ErrorIs(t, err, ErrClosed)
		require.ErrorIs(t, s.AppendAudit(ctx, fleet.AuditEntry{Action: "a"}), ErrClosed)
		require.NoError(t, s.Stop(), "stop before start is a no-op")

		require.NoError(t, s.Start(ctx))
		require.Error(t, s.Start(ctx), "second start is refused")
		require.NoError(t, s.Stop())
		require.NoError(t, s.Stop(), "stop twice is a no-op")

		_, err = s.ListHosts(ctx)
		require.ErrorIs(t, err, ErrClosed)
	})

	t.Run("creates the directory and a private file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a b?c#d%e", "nested", "upgradoor.db")
		startStore(t, New(quietLog(), path))

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("an existing database is made private", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "upgradoor.db")

		// A database with data in it and a WAL left behind, both readable
		// by everyone - what a restored backup can look like.
		first := New(quietLog(), path)
		require.NoError(t, first.Start(ctx))
		addHost(t, first, "node-1", nil)
		require.NoError(t, first.Stop())

		require.NoError(t, os.WriteFile(path+"-wal", nil, 0o600))

		for _, file := range []string{path, path + "-wal"} {
			require.NoError(t, os.Chmod(file, 0o664))
		}

		s := startStore(t, New(quietLog(), path))

		for _, file := range []string{path, path + "-wal", path + "-shm"} {
			info, err := os.Stat(file)
			require.NoError(t, err, file)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), file)
		}

		hosts, err := s.ListHosts(ctx)
		require.NoError(t, err)
		assert.Len(t, hosts, 1, "the data is untouched")
	})

	t.Run("restrictFile", func(t *testing.T) {
		dir := t.TempDir()

		require.NoError(t, restrictFile(filepath.Join(dir, "missing")), "no file is fine")

		tests := []struct {
			mode, want os.FileMode
		}{
			{0o600, 0o600},
			{0o644, 0o600},
			{0o666, 0o600},
			{0o640, 0o600},
			{0o400, 0o400}, // never widened
			{0o444, 0o400},
		}

		for _, tt := range tests {
			path := filepath.Join(dir, fmt.Sprintf("f-%o", tt.mode))
			require.NoError(t, os.WriteFile(path, nil, 0o600))
			require.NoError(t, os.Chmod(path, tt.mode))
			require.NoError(t, restrictFile(path))

			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, info.Mode().Perm(), "from %o", tt.mode)
		}
	})

	t.Run("a database that is not in WAL mode is refused", func(t *testing.T) {
		// What the DSN pragma leaves behind where WAL cannot be enabled: a
		// rollback journal and no error.
		path := filepath.Join(t.TempDir(), "upgradoor.db")

		db, err := sql.Open(driverName, "file:"+path+"?_pragma=journal_mode(DELETE)")
		require.NoError(t, err)

		defer func() { assert.NoError(t, db.Close()) }()

		require.ErrorIs(t, checkJournalMode(ctx, db), ErrNoWAL)

		_, err = db.Exec(`PRAGMA journal_mode = WAL`)
		require.NoError(t, err)
		require.NoError(t, checkJournalMode(ctx, db))
	})

	t.Run("empty path is refused", func(t *testing.T) {
		require.Error(t, New(quietLog(), "").Start(ctx))
	})

	t.Run("cancelled context", func(t *testing.T) {
		s := newFileStore(t)

		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		_, err := s.ListHosts(cancelled)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, s.AppendAudit(cancelled, fleet.AuditEntry{Action: "a"}), context.Canceled)

		// The store is still usable afterwards.
		require.NoError(t, s.AppendAudit(ctx, fleet.AuditEntry{Action: "a"}))
	})

	t.Run("file pragmas", func(t *testing.T) {
		s := newFileStore(t)
		impl, ok := s.(*sqliteStore)
		require.True(t, ok)

		for _, db := range []*sql.DB{impl.writer, impl.reader} {
			var (
				journal     string
				synchronous int
				foreignKeys int
				busy        int
			)

			require.NoError(t, db.QueryRow(`PRAGMA journal_mode`).Scan(&journal))
			require.NoError(t, db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous))
			require.NoError(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys))
			require.NoError(t, db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy))

			assert.Equal(t, "wal", journal)
			assert.Equal(t, 2, synchronous, "FULL")
			assert.Equal(t, 1, foreignKeys)
			assert.Equal(t, int(busyTimeout.Milliseconds()), busy)
		}

		_, err := impl.reader.Exec(`DELETE FROM hosts`)
		require.Error(t, err, "reader connections are query-only")
	})

	t.Run("memory stores are private", func(t *testing.T) {
		a := startStore(t, NewMemory(quietLog()))
		b := startStore(t, NewMemory(quietLog()))

		addHost(t, a, "node-1", nil)

		hosts, err := b.ListHosts(ctx)
		require.NoError(t, err)
		assert.Empty(t, hosts)
	})
}

func TestRestartPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgradoor.db")

	log, hook := logrustest.NewNullLogger()
	applied := func() int {
		n := 0

		for _, entry := range hook.AllEntries() {
			if entry.Message == "applied database migration" {
				n++
			}
		}

		return n
	}

	first := New(log, path)
	require.NoError(t, first.Start(ctx))
	assert.Equal(t, 1, applied())

	addHost(t, first, "node-1", &identity.ValidatorRange{Start: 0, End: 100})
	inv := testInventory(20)
	require.NoError(t, first.SaveInventory(ctx, "node-1", inv))
	require.NoError(t, first.SetHostHold(ctx, "node-1", true, "investigating"))
	require.NoError(t, first.Commit(ctx, &Changes{
		Rollouts:   []*fleet.Rollout{testRollout("r1", fleet.RolloutActive, 0, "node-1")},
		Leases:     []*fleet.Lease{testLease("l1", "node-1", fleet.LeaseRunning, 0)},
		Directives: []*fleet.Directive{testDirective("d1", "node-1", fleet.DirectivePending, 0)},
		Audit:      []fleet.AuditEntry{{Actor: "scheduler", Action: "lease.granted", Target: "node-1"}},
	}))
	require.NoError(t, first.SaveLiveness(ctx, []fleet.Liveness{{Host: "node-1", Epoch: 7, Live: 99, Total: 100}}))

	settings := fleet.DefaultSettings()
	settings.Paused = true
	require.NoError(t, first.SaveSettings(ctx, fleet.DefaultSettings(), settings))
	require.NoError(t, first.Stop())

	hook.Reset()

	second := New(log, path)
	require.NoError(t, second.Start(ctx))

	defer func() { require.NoError(t, second.Stop()) }()

	assert.Zero(t, applied(), "migrations are not applied again")

	var version int

	require.NoError(t, rawDB(t, second).QueryRow(`PRAGMA user_version`).Scan(&version))
	assert.Equal(t, 1, version)

	host, err := second.GetHost(ctx, "node-1")
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "investigating", host.HoldReason)
	assert.Equal(t, inv.Hash, host.InventoryHash)
	assert.Equal(t, testRequest(), host.Status)

	gotInv, err := second.GetInventory(ctx, "node-1")
	require.NoError(t, err)
	assert.Equal(t, inv, gotInv)

	rollout, err := second.GetRollout(ctx, "r1")
	require.NoError(t, err)
	assert.Equal(t, testRollout("r1", fleet.RolloutActive, 0, "node-1"), rollout)

	lease, err := second.GetLease(ctx, "l1")
	require.NoError(t, err)
	assert.Equal(t, testLease("l1", "node-1", fleet.LeaseRunning, 0), lease)

	directives, err := second.ListDirectives(ctx, DirectiveFilter{})
	require.NoError(t, err)
	assert.Equal(t, []*fleet.Directive{testDirective("d1", "node-1", fleet.DirectivePending, 0)}, directives)

	audit, err := second.ListAudit(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, audit, 1)
	assert.Equal(t, "lease.granted", audit[0].Action)

	liveness, err := second.ListLiveness(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, []fleet.Liveness{{Host: "node-1", Epoch: 7, Live: 99, Total: 100}}, liveness)

	loaded, err := second.LoadSettings(ctx, fleet.DefaultSettings())
	require.NoError(t, err)
	assert.Equal(t, settings, loaded)
}

func TestSchemaNewerThanBinary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgradoor.db")

	s := New(quietLog(), path)
	require.NoError(t, s.Start(ctx))
	addHost(t, s, "node-1", nil)

	_, err := rawDB(t, s).Exec(`PRAGMA user_version = 99`)
	require.NoError(t, err)
	require.NoError(t, s.Stop())

	newer := New(quietLog(), path)
	require.ErrorIs(t, newer.Start(ctx), ErrSchemaTooNew)

	_, err = newer.ListHosts(ctx)
	require.ErrorIs(t, err, ErrClosed, "a refused database is not left open")

	// The refused start changed nothing.
	db, err := sql.Open(driverName, "file:"+escapeURIPath(path))
	require.NoError(t, err)

	defer func() { require.NoError(t, db.Close()) }()

	var version, hosts int

	require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM hosts`).Scan(&hosts))
	assert.Equal(t, 99, version)
	assert.Equal(t, 1, hosts)
}

func TestApplyMigrations(t *testing.T) {
	ctx := context.Background()

	open := func(t *testing.T) *sql.DB {
		t.Helper()

		db, err := openPool(ctx, "file:"+escapeURIPath(filepath.Join(t.TempDir(), "m.db"))+
			"?_txlock=immediate", 1)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, db.Close()) })

		return db
	}

	version := func(t *testing.T, db *sql.DB) int {
		t.Helper()

		var v int

		require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&v))

		return v
	}

	tableExists := func(t *testing.T, db *sql.DB, name string) bool {
		t.Helper()

		var n int

		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n))

		return n == 1
	}

	one := migration{version: 1, name: "0001_a.sql", sql: `CREATE TABLE a (x INTEGER); INSERT INTO a VALUES (1);`}
	two := migration{version: 2, name: "0002_b.sql", sql: `CREATE TABLE b (y INTEGER); INSERT INTO b VALUES (2);`}
	bad := migration{version: 2, name: "0002_bad.sql", sql: `CREATE TABLE c (z INTEGER); INSERT INTO missing VALUES (1);`}

	t.Run("applies in order and only once", func(t *testing.T) {
		db := open(t)

		v, err := applyMigrations(ctx, quietLog(), db, []migration{one})
		require.NoError(t, err)
		assert.Equal(t, 1, v)

		// Re-running would fail with "table a already exists" if it were
		// applied again.
		v, err = applyMigrations(ctx, quietLog(), db, []migration{one, two})
		require.NoError(t, err)
		assert.Equal(t, 2, v)
		assert.Equal(t, 2, version(t, db))

		v, err = applyMigrations(ctx, quietLog(), db, []migration{one, two})
		require.NoError(t, err)
		assert.Equal(t, 2, v)

		var rows int

		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM a`).Scan(&rows))
		assert.Equal(t, 1, rows)
	})

	t.Run("a failing step is rolled back whole", func(t *testing.T) {
		db := open(t)

		v, err := applyMigrations(ctx, quietLog(), db, []migration{one, bad})
		require.Error(t, err)
		assert.Equal(t, 1, v)
		assert.Equal(t, 1, version(t, db))
		assert.True(t, tableExists(t, db, "a"))
		assert.False(t, tableExists(t, db, "c"), "the half-applied step left nothing behind")

		// A fixed binary continues from version 1.
		v, err = applyMigrations(ctx, quietLog(), db, []migration{one, two})
		require.NoError(t, err)
		assert.Equal(t, 2, v)
	})

	t.Run("newer database is refused untouched", func(t *testing.T) {
		db := open(t)

		_, err := applyMigrations(ctx, quietLog(), db, []migration{one, two})
		require.NoError(t, err)

		v, err := applyMigrations(ctx, quietLog(), db, []migration{one})
		require.ErrorIs(t, err, ErrSchemaTooNew)
		assert.Equal(t, 2, v)
		assert.Equal(t, 2, version(t, db))
	})
}

func TestLoadMigrations(t *testing.T) {
	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }

	tests := []struct {
		name    string
		files   fstest.MapFS
		want    []int
		wantErr string
	}{
		{
			name: "sorted by version",
			files: fstest.MapFS{
				"migrations/0002_b.sql": file("b"),
				"migrations/0001_a.sql": file("a"),
				"migrations/0010_c.txt": file("ignored"),
			},
			want: []int{1, 2},
		},
		{
			name:    "gap",
			files:   fstest.MapFS{"migrations/0001_a.sql": file("a"), "migrations/0003_c.sql": file("c")},
			wantErr: "contiguous",
		},
		{
			name:    "duplicate version",
			files:   fstest.MapFS{"migrations/0001_a.sql": file("a"), "migrations/1_b.sql": file("b")},
			wantErr: "contiguous",
		},
		{
			name:    "no separator",
			files:   fstest.MapFS{"migrations/0001.sql": file("a")},
			wantErr: "<version>_<name>.sql",
		},
		{
			name:    "bad version",
			files:   fstest.MapFS{"migrations/abc_a.sql": file("a")},
			wantErr: "bad version",
		},
		{
			name:    "version zero",
			files:   fstest.MapFS{"migrations/0000_a.sql": file("a")},
			wantErr: "bad version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadMigrations(tt.files)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)

			versions := make([]int, 0, len(got))
			for _, m := range got {
				versions = append(versions, m.version)
			}

			assert.Equal(t, tt.want, versions)
		})
	}

	t.Run("embedded migrations are valid", func(t *testing.T) {
		got, err := loadMigrations(migrationFiles)
		require.NoError(t, err)
		require.NotEmpty(t, got)
		assert.Equal(t, "0001_init.sql", got[0].name)
	})
}

func TestEscapeLike(t *testing.T) {
	tests := []struct{ in, want string }{
		{"libc6", "libc6"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
		{`%_\`, `\%\_\\`},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, escapeLike(tt.in))
	}
}

// TestConcurrentUse runs writers and readers of every kind against one store
// at once; with -race it also checks the store for data races.
func TestConcurrentUse(t *testing.T) {
	const (
		writers = 6
		readers = 6
		rounds  = 15
	)

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			for w := range writers {
				addHost(t, s, fmt.Sprintf("node-%d", w), nil)
			}

			inv := testInventory(200)

			var wg sync.WaitGroup

			errs := make(chan error, (writers+readers)*rounds*6)
			report := func(err error) {
				if err != nil {
					errs <- err
				}
			}

			for w := range writers {
				wg.Add(1)

				go func() {
					defer wg.Done()

					host := fmt.Sprintf("node-%d", w)

					for i := range rounds {
						_, err := s.RecordSync(ctx, &HostSync{
							Cert:    testCert(host, nil),
							Request: testRequest(imageNeed(fmt.Sprintf("need-%d", i%3))),
							At:      at(i),
						})
						report(err)
						report(s.SaveInventory(ctx, host, inv))
						report(s.Commit(ctx, &Changes{
							Rollouts: []*fleet.Rollout{
								testRollout(fmt.Sprintf("r-%d-%d", w, i), fleet.RolloutActive, i, host),
							},
							Leases: []*fleet.Lease{
								testLease(fmt.Sprintf("l-%d-%d", w, i), host, fleet.LeaseGranted, i),
							},
							Audit: []fleet.AuditEntry{{Actor: "scheduler", Action: "lease.granted", Target: host}},
						}))
						report(s.AppendAudit(ctx, fleet.AuditEntry{Actor: "agent:" + host, Action: "sync"}))
						report(s.SaveLiveness(ctx, []fleet.Liveness{
							{Host: host, Epoch: uint64(i), Live: 1, Total: 1},
						}))
						report(s.SetHostHold(ctx, host, i%2 == 0, "round"))
					}
				}()
			}

			for range readers {
				wg.Add(1)

				go func() {
					defer wg.Done()

					for range rounds {
						_, err := s.ListHosts(ctx)
						report(err)

						rollouts, err := s.ListRollouts(ctx, RolloutFilter{OpenOnly: true})
						report(err)

						for _, r := range rollouts {
							if len(r.Hosts) != 1 {
								report(fmt.Errorf("rollout %s seen with %d hosts", r.ID, len(r.Hosts)))
							}
						}

						_, err = s.ListLeases(ctx, LeaseFilter{OpenOnly: true})
						report(err)
						_, err = s.ListAudit(ctx, AuditFilter{Limit: 50})
						report(err)
						_, err = s.SearchPackages(ctx, "pkg-00", 10)
						report(err)
						_, err = s.ListLiveness(ctx, 3)
						report(err)

						got, err := s.GetInventory(ctx, "node-0")
						if err == nil && len(got.Packages) != len(inv.Packages) {
							report(fmt.Errorf("inventory seen with %d packages", len(got.Packages)))
						}
					}
				}()
			}

			wg.Wait()
			close(errs)

			for err := range errs {
				require.NoError(t, err)
			}

			assert.Equal(t, writers, countRows(t, s, "hosts"))
			assert.Equal(t, writers*rounds, countRows(t, s, "rollouts"))
			assert.Equal(t, writers*rounds, countRows(t, s, "rollout_hosts"))
			assert.Equal(t, writers*rounds, countRows(t, s, "leases"))
			assert.Equal(t, writers*rounds*2, countRows(t, s, "audit"))
			assert.Equal(t, writers*rounds, countRows(t, s, "validator_liveness"))
			assert.Equal(t, writers*len(inv.Packages), countRows(t, s, "packages"))
		})
	}
}
