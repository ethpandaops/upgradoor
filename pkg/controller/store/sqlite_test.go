package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

func testCert(name string, roles ...string) *identity.Certificate {
	return &identity.Certificate{
		Claims: identity.Claims{
			Issuer:     identity.Issuer,
			Network:    "testnet-0",
			Subject:    name,
			Generation: 1,
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

// reporting is testRequest of a host that reports the validator range
// [start, end) from its marker file.
func reporting(start, end uint64, needs ...protocol.Need) *protocol.SyncRequest {
	req := testRequest(needs...)
	req.Validators = &protocol.Validators{
		Start: start, End: end,
		Source: protocol.ValidatorsFromMarker, State: protocol.ValidatorsKnown,
	}

	return req
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

// addHost registers a host through RecordSync; validators is the range it
// reports, nil for none.
func addHost(t *testing.T, s Store, name string, validators *identity.ValidatorRange) *fleet.Host {
	t.Helper()

	req := testRequest()
	if validators != nil {
		req = reporting(validators.Start, validators.End)
	}

	res, err := s.RecordSync(context.Background(), &HostSync{
		Cert:       testCert(name),
		RemoteAddr: "10.0.0.1:1234",
		Request:    req,
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

	first := New(quietLog(), path)
	require.NoError(t, first.Start(ctx))

	addHost(t, first, "node-1", &identity.ValidatorRange{Start: 0, End: 100})
	inv := testInventory(20)
	require.NoError(t, first.SaveInventory(ctx, "node-1", inv))
	require.NoError(t, setHold(ctx, first, "node-1", true, "investigating"))
	require.NoError(t, first.Commit(ctx, &Changes{
		Rollouts:   []*fleet.Rollout{testRollout("r1", fleet.RolloutActive, 0, "node-1")},
		Leases:     []*fleet.Lease{testLease("l1", "node-1", fleet.LeaseRunning, 0)},
		Directives: []*fleet.Directive{testDirective("d1", "node-1", fleet.DirectivePending, 0)},
		Audit:      []fleet.AuditEntry{{Actor: "scheduler", Action: "lease.granted", Target: "node-1"}},
	}))

	settings := fleet.DefaultSettings()
	settings.Paused = true
	require.NoError(t, first.SaveSettings(ctx, fleet.DefaultSettings(), settings))
	require.NoError(t, first.Stop())

	second := New(quietLog(), path)
	require.NoError(t, second.Start(ctx))

	defer func() { require.NoError(t, second.Stop()) }()

	host, err := second.GetHost(ctx, "node-1")
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "investigating", host.HoldReason)
	assert.Equal(t, inv.Hash, host.InventoryHash)
	assert.Equal(t, reporting(0, 100), host.Status)
	assert.Equal(t, uint64(100), host.Weight())

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

	loaded, err := second.LoadSettings(ctx, fleet.DefaultSettings())
	require.NoError(t, err)
	assert.Equal(t, settings, loaded)
}

func TestSchemaVersion(t *testing.T) {
	ctx := context.Background()

	for _, changed := range []int{0, schemaVersion + 1} {
		t.Run(fmt.Sprintf("version %d", changed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgradoor.db")

			s := New(quietLog(), path)
			require.NoError(t, s.Start(ctx))
			addHost(t, s, "node-1", nil)
			require.NoError(t, s.Stop())

			db, err := sql.Open(driverName, "file:"+escapeURIPath(path))
			require.NoError(t, err)

			defer func() { require.NoError(t, db.Close()) }()

			_, err = db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, changed))
			require.NoError(t, err)

			other := New(quietLog(), path)
			err = other.Start(ctx)
			require.ErrorIs(t, err, ErrSchemaVersion)
			require.ErrorContains(t, err, "delete the database file")
			require.ErrorContains(t, err, path)

			_, err = other.ListHosts(ctx)
			require.ErrorIs(t, err, ErrClosed, "a refused database is not left open")

			// The refused start changed nothing.
			var version, hosts int

			require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM hosts`).Scan(&hosts))
			assert.Equal(t, changed, version)
			assert.Equal(t, 1, hosts)
		})
	}
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
		rounds  = 4
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
							Cert:    testCert(host),
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
						report(setHold(ctx, s, host, i%2 == 0, "round"))
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
			assert.Equal(t, writers*len(inv.Packages), countRows(t, s, "packages"))
		})
	}
}

// queryPlan returns the lines of EXPLAIN QUERY PLAN for a query.
func queryPlan(t *testing.T, s Store, query string, args []any) []string {
	t.Helper()

	rows, err := rawDB(t, s).Query(`EXPLAIN QUERY PLAN `+query, args...)
	require.NoError(t, err)

	defer func() { require.NoError(t, rows.Close()) }()

	var plan []string

	for rows.Next() {
		var (
			id, parent, unused int
			detail             string
		)

		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))

		plan = append(plan, detail)
	}

	require.NoError(t, rows.Err())

	return plan
}

// fillHistory stores ended leases, closed rollouts and answered directives,
// and a few open ones, so that the planner has something to choose between.
func fillHistory(t *testing.T, s Store, ended int) {
	t.Helper()

	changes := &Changes{}

	for i := range ended {
		host := fmt.Sprintf("node-%d", i%8)
		state := []fleet.LeaseState{fleet.LeaseReleased, fleet.LeaseFailed, fleet.LeaseExpired}[i%3]
		closed := []fleet.RolloutState{
			fleet.RolloutCompleted, fleet.RolloutSuperseded, fleet.RolloutAborted,
		}[i%3]
		answered := []fleet.DirectiveState{
			fleet.DirectiveAccepted, fleet.DirectiveRejected, fleet.DirectiveCancelled,
		}[i%3]

		changes.Leases = append(changes.Leases, testLease(fmt.Sprintf("ended-%d", i), host, state, i))
		changes.Rollouts = append(changes.Rollouts, testRollout(fmt.Sprintf("closed-%d", i), closed, i, host))
		// An answered directive belongs to a rollout that is closed by now, or
		// to none (a hold, a recheck).
		directive := testDirective(fmt.Sprintf("answered-%d", i), host, answered, i)
		directive.RolloutID = []string{fmt.Sprintf("closed-%d", i), ""}[i%2]
		changes.Directives = append(changes.Directives, directive)
	}

	for i, state := range fleet.OpenLeaseStates() {
		changes.Leases = append(changes.Leases,
			testLease(fmt.Sprintf("open-%d", i), fmt.Sprintf("node-%d", i), state, ended+i))
	}

	changes.Rollouts = append(changes.Rollouts,
		testRollout("active", fleet.RolloutActive, ended, "node-0"),
		testRollout("halted", fleet.RolloutHalted, ended+1, "node-1"))
	pending := testDirective("pending", "node-0", fleet.DirectivePending, ended)
	pending.RolloutID = "active"
	changes.Directives = append(changes.Directives, pending)

	require.NoError(t, s.Commit(context.Background(), changes))

	_, err := rawDB(t, s).Exec(`ANALYZE`)
	require.NoError(t, err)
}

// What the scheduler reads on every tick, and what the API reads on every
// sync of an agent, must not read a whole table that only ever grows: the
// leases, rollouts and directives that ended are kept. Every such query
// finds its rows through an index, so a tick costs what is open, however
// long the controller has run and however large the fleet is.
func TestTickQueriesDoNotScanTheHistory(t *testing.T) {
	tick := []struct {
		name  string
		table string
		query func() (string, []any)
	}{
		{"open leases", "leases", func() (string, []any) {
			return leasesQuery(LeaseFilter{OpenOnly: true})
		}},
		{"the open lease of a host", "leases", func() (string, []any) {
			return leasesQuery(LeaseFilter{OpenOnly: true, Host: "node-1"})
		}},
		{"open rollouts", "rollouts", func() (string, []any) {
			return rolloutsQuery(RolloutFilter{OpenOnly: true})
		}},
		{"members of a rollout", "rollout_hosts", func() (string, []any) {
			return rolloutHostsQuery, []any{"active"}
		}},
		{"pending directives", "directives", func() (string, []any) {
			return directivesQuery(DirectiveFilter{PendingOnly: true})
		}},
		{"directives of the open manual rollouts", "directives", func() (string, []any) {
			return directivesQuery(DirectiveFilter{Rollouts: []string{"active", "halted"}})
		}},
		{"pending directives of a host", "directives", func() (string, []any) {
			return directivesQuery(DirectiveFilter{PendingOnly: true, Host: "node-1"})
		}},
		{"a lease by its ID", "leases", func() (string, []any) {
			return `SELECT data FROM leases WHERE id = ?`, []any{"open-0"}
		}},
	}

	// With and without statistics: a database that was never analysed must
	// choose the index too.
	for _, filled := range []bool{false, true} {
		for _, backend := range backends {
			t.Run(fmt.Sprintf("%s/filled=%t", backend.name, filled), func(t *testing.T) {
				s := backend.open(t)

				if filled {
					fillHistory(t, s, 600)
				}

				for _, q := range tick {
					query, args := q.query()
					plan := queryPlan(t, s, query, args)
					require.NotEmpty(t, plan, q.name)

					for _, line := range plan {
						assert.False(t, strings.HasPrefix(line, "SCAN "+q.table),
							"%s reads the whole table: %s\n%s", q.name, line, strings.Join(plan, "\n"))
					}
				}
			})
		}
	}
}

// The open states of the queries are the ones Terminal does not name.
func TestOpenQueriesReturnExactlyTheOpenRows(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			fillHistory(t, s, 30)

			leases, err := s.ListLeases(ctx, LeaseFilter{OpenOnly: true})
			require.NoError(t, err)
			require.Len(t, leases, 3)

			for _, l := range leases {
				assert.False(t, l.State.Terminal(), l.ID)
			}

			all, err := s.ListLeases(ctx, LeaseFilter{})
			require.NoError(t, err)

			open := 0

			for _, l := range all {
				if !l.State.Terminal() {
					open++
				}
			}

			assert.Equal(t, 3, open)
			assert.Len(t, all, 33)

			rollouts, err := s.ListRollouts(ctx, RolloutFilter{OpenOnly: true})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"active", "halted"}, rolloutIDs(rollouts))
		})
	}

	for _, state := range []fleet.LeaseState{
		fleet.LeaseGranted, fleet.LeaseRunning, fleet.LeaseVerifying,
		fleet.LeaseReleased, fleet.LeaseFailed, fleet.LeaseExpired,
	} {
		assert.Equal(t, !state.Terminal(), containsState(fleet.OpenLeaseStates(), state), state)
	}
}

func containsState(states []fleet.LeaseState, state fleet.LeaseState) bool {
	for _, s := range states {
		if s == state {
			return true
		}
	}

	return false
}

// A commit sets holds in the transaction of its leases: either both are
// stored or neither.
func TestCommitHolds(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			_, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-1"), Request: testRequest(), At: at(0)})
			require.NoError(t, err)

			lease := testLease("l1", "node-1", fleet.LeaseRunning, 10)
			lease.Cancelled = &fleet.LeaseCancel{
				At: at(20).Add(1500 * 1000), By: "alice", State: fleet.LeaseRunning, Reason: "taken away",
			}

			require.NoError(t, s.Commit(ctx, &Changes{
				Leases: []*fleet.Lease{lease},
				Holds:  []HostHold{{Host: "node-1", Wanted: true, Reason: "lease ended by operator"}},
			}))

			host, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.True(t, host.HoldWanted)
			assert.Equal(t, "lease ended by operator", host.HoldReason)
			assert.Nil(t, host.Offline)

			until := at(90)
			mark := &fleet.OfflineMark{Reason: "debugging", Until: &until, By: fleet.HoldByTeam}
			require.NoError(t, s.Commit(ctx, &Changes{Offline: []HostOffline{{Host: "node-1", Mark: mark}}}))

			host, err = s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			require.NotNil(t, host.Offline)
			assert.Equal(t, "debugging", host.Offline.Reason)
			assert.True(t, host.Offline.Until.Equal(until))
			assert.Equal(t, fleet.HoldByTeam, host.Offline.By)

			require.NoError(t, s.Commit(ctx, &Changes{Offline: []HostOffline{{Host: "node-1"}}}))

			host, err = s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.Nil(t, host.Offline, "lifted")
			require.ErrorIs(t, s.Commit(ctx, &Changes{Offline: []HostOffline{{Host: "nope"}}}), ErrNotFound)

			got, err := s.GetLease(ctx, "l1")
			require.NoError(t, err)
			require.NotNil(t, got.Cancelled)
			assert.Equal(t, "alice", got.Cancelled.By)
			assert.Equal(t, fleet.LeaseRunning, got.Cancelled.State)
			assert.True(t, got.Cancelled.At.Equal(storedTime(lease.Cancelled.At)), "millisecond precision")
			assert.True(t, lease.Cancelled.At.Equal(at(20).Add(1500*1000)), "the caller's lease is not changed")

			// A hold of a host that does not exist fails the whole commit.
			other := testLease("l2", "node-2", fleet.LeaseRunning, 30)
			err = s.Commit(ctx, &Changes{
				Leases: []*fleet.Lease{other},
				Holds:  []HostHold{{Host: "node-2", Wanted: true, Reason: "x"}},
			})
			require.ErrorIs(t, err, ErrNotFound)

			_, err = s.GetLease(ctx, "l2")
			require.ErrorIs(t, err, ErrNotFound)

			assert.False(t, (&Changes{Holds: []HostHold{{Host: "node-1"}}}).Empty())
		})
	}
}
