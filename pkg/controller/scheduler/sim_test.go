package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The simulation drives the real Service over an in-memory store with a
// random fleet, simulated agents, a simulated registry and network, random
// operator actions, controller restarts (some after an outage longer than
// leaseTtl) and store write failures. Nodes come back from an operation
// syncing, without their EL or slow to find peers, lose peers they had, and
// agents go silent while their host is checked for readiness. In every fifth
// seed the beacon node does not serve the liveness endpoint for the whole
// run; every fourth run begins before the chain's genesis, and in every
// seventh the beacon node announces a slot phase far in the future that is
// out of bounds. Now and then the controller restarts on closed rollouts as
// the builds before this one wrote them, with the fields those builds kept.
// A person ends a lease now and then.
// The store gives every row back as the SQLite store does (fakestore_test.go).
// After
// every step the oracle (sim_oracle_test.go) asserts the invariants of
// docs/scheduler.md from the world's own state; at the end of a run the world
// is healed and everything must converge.
//
// Reproduce a failure with UPGRADOOR_SIM_SEED=<seed>; run more seeds with
// UPGRADOOR_SIM_SEEDS=<n>.

const (
	simActor = "sim-operator"
	// simShown is how many of the last steps a failure report shows.
	simShown = 40
	// simConvergeTicks bounds the convergence phase.
	simConvergeTicks = 6000
)

// simReasons are the forms a waiting reason may take.
var simReasons = regexp.MustCompile(`^(held|agent silent|paused|rollout paused|` +
	`waiting for approval \(policy manual\)|policy paused|rollout halted: .+|` +
	`verb not allowed by the agent|network gate: .+|after .+|` +
	`budget [0-9.]+% of [0-9.]+%|host limit \d+ of \d+|next)$`)

// sim is one simulation run.
type sim struct {
	seed  uint64
	rng   *rand.Rand
	world *simWorld
	store *fakeStore
	svc   *service
	// since is when the controller last started.
	since    time.Time
	oracle   *oracle
	defaults fleet.Settings
	log      logrus.FieldLogger

	ids   int
	step  int
	trail []string
	// retried are the rollouts retried since the last observation; observed
	// is the store's write count at that observation.
	retried  []string
	observed int
	// skipped is the person's own record of the hosts skipped per rollout
	// key: set by a skip, cleared by a retry that names the host (or lifts
	// the skips of a closed rollout), by the host syncing without the need
	// while it is not held, and by forgetting the host.
	skipped map[string]map[string]bool
	// holds is the person's own record of the hosts on controller hold. It
	// outlives the controller's database.
	holds map[string]bool
	// registered is when the current database first saw each host.
	registered map[string]time.Time
	stats      simStats
}

// simStats counts what happened, so that the test can tell a simulation that
// exercised the rules from one that idled.
type simStats struct {
	Seeds, Hosts                          int
	Grants, Released, Failed, Expired     int
	Completed, Superseded, Aborted, Halts int
	ForcedGrants, ExemptGrants, Bundled   int
	Restarts, WriteFailures, Shadows      int
	LostDatabases, Adopted                int
	// Outages are restarts after more than leaseTtl of downtime and
	// OutageLeases the open leases they found; Unsupported are the seeds
	// without liveness and ProbeGrants the budgeted grants to validator
	// hosts in them.
	Outages, OutageLeases    int
	Unsupported, ProbeGrants int
	// HoldsAdopted counts hosts that were held on the agent when a
	// controller without a record of them first saw them.
	HoldsAdopted                     int
	Skips, SkipsLifted, SkipsCarried int
	// Repairs are the leases granted through the closed gate, BrokenMembers
	// the members in the repair stage, MixedLeases the failed leases in which
	// a step was done.
	Repairs, BrokenMembers, MixedLeases int
	// Forks are the changes of the slot duration and SettledClosed the
	// members of closed rollouts that got the outcome of a lease that ended
	// after the rollout had closed.
	Forks, SettledClosed int
	// Reopened are the expired leases that were taken up again.
	Reopened int
	// BeforeGenesis are the runs that began before the chain did,
	// FuturePhases those in which the beacon node announced a slot phase out
	// of bounds for the far future, PreviousBuilds the restarts on rows of
	// the builds before this one. The others count what the oracle saw by its
	// own record of the leases: broken hosts that were granted first
	// (BrokenFirst) and repaired (Repaired); hosts held back by a lease in
	// flight (FlightHeld), of those the ones behind a lease of an aborted
	// rollout (FlightAborted), behind a lease that was taken up again
	// (FlightReopened), in a rollout that a person's retry opened
	// (FlightRetried) and in a rollout that was open before the lease's
	// rollout was closed (FlightNextToOpen); the leases a person ended
	// (EndedByOperator) and the hosts those broke (BrokenByOperator); and the
	// hosts broken by a lease of a rollout that had been aborted
	// (BrokenAfterAbort).
	BeforeGenesis, FuturePhases, PreviousBuilds int
	BrokenFirst, Repaired                       int
	FlightHeld, FlightAborted, FlightReopened   int
	FlightRetried, FlightNextToOpen             int
	EndedByOperator, BrokenByOperator           int
	BrokenAfterAbort                            int
	ConvergeTicks, MaxConvergeTicks             int
	// Waits counts the waiting reasons seen in plans, by their first word.
	Waits map[string]int
}

func (a *simStats) add(b *simStats) {
	a.Seeds += b.Seeds
	a.Hosts += b.Hosts
	a.Grants += b.Grants
	a.Released += b.Released
	a.Failed += b.Failed
	a.Expired += b.Expired
	a.Completed += b.Completed
	a.Superseded += b.Superseded
	a.Aborted += b.Aborted
	a.Halts += b.Halts
	a.ForcedGrants += b.ForcedGrants
	a.ExemptGrants += b.ExemptGrants
	a.Bundled += b.Bundled
	a.Restarts += b.Restarts
	a.WriteFailures += b.WriteFailures
	a.Shadows += b.Shadows
	a.LostDatabases += b.LostDatabases
	a.Outages += b.Outages
	a.OutageLeases += b.OutageLeases
	a.Unsupported += b.Unsupported
	a.ProbeGrants += b.ProbeGrants
	a.HoldsAdopted += b.HoldsAdopted
	a.Adopted += b.Adopted
	a.Skips += b.Skips
	a.SkipsLifted += b.SkipsLifted
	a.SkipsCarried += b.SkipsCarried
	a.Repairs += b.Repairs
	a.BrokenMembers += b.BrokenMembers
	a.MixedLeases += b.MixedLeases
	a.Forks += b.Forks
	a.Reopened += b.Reopened
	a.BeforeGenesis += b.BeforeGenesis
	a.FuturePhases += b.FuturePhases
	a.PreviousBuilds += b.PreviousBuilds
	a.BrokenFirst += b.BrokenFirst
	a.Repaired += b.Repaired
	a.FlightHeld += b.FlightHeld
	a.FlightAborted += b.FlightAborted
	a.FlightReopened += b.FlightReopened
	a.FlightRetried += b.FlightRetried
	a.FlightNextToOpen += b.FlightNextToOpen
	a.EndedByOperator += b.EndedByOperator
	a.BrokenByOperator += b.BrokenByOperator
	a.BrokenAfterAbort += b.BrokenAfterAbort
	a.SettledClosed += b.SettledClosed
	a.ConvergeTicks += b.ConvergeTicks
	a.MaxConvergeTicks = max(a.MaxConvergeTicks, b.MaxConvergeTicks)

	if a.Waits == nil {
		a.Waits = make(map[string]int, len(b.Waits))
	}

	for k, v := range b.Waits {
		a.Waits[k] += v
	}
}

// countRollout counts the members in the repair stage and those that were
// settled after their rollout had closed.
func (s *sim) countRollout(r *fleet.Rollout) {
	for i := range r.Hosts {
		m := &r.Hosts[i]
		if r.State.Terminal() && r.EndedAt != nil && m.UpdatedAt.After(*r.EndedAt) &&
			(m.State == fleet.HostFailed || m.State == fleet.HostDone) {
			s.stats.SettledClosed++
		}

		if m.Stage == fleet.StageRepair {
			s.stats.BrokenMembers++
		}
	}
}

// countLease counts the repairs and the failed leases in which a step was
// done.
func (s *sim) countLease(l *fleet.Lease) {
	if l.Repair {
		s.stats.Repairs++
	}

	if l.State == fleet.LeaseFailed && l.Operation != nil &&
		slices.ContainsFunc(l.Operation.Steps, func(st protocol.Step) bool { return st.Status == protocol.StepDone }) {
		s.stats.MixedLeases++
	}
}

// countLedger takes over what the oracle counted. The oracle starts over
// when the database is lost.
func (s *sim) countLedger() {
	c := &s.oracle.counts

	s.stats.BrokenFirst += c.brokenFirst
	s.stats.Repaired += c.repaired
	s.stats.FlightHeld += c.flightHeld
	s.stats.FlightAborted += c.flightAborted
	s.stats.FlightReopened += c.flightReopened
	s.stats.FlightRetried += c.flightRetried
	s.stats.FlightNextToOpen += c.flightNextToOpen
	s.stats.EndedByOperator += c.endedByOperator
	s.stats.BrokenByOperator += c.brokenByOperator
	s.stats.BrokenAfterAbort += c.brokenAfterAbort
}

// count reads the run's totals off the audit trail and the store.
func (s *sim) count() {
	s.countLedger()

	for _, action := range s.store.auditActions() {
		switch action {
		case "lease.granted":
			s.stats.Grants++
		case "lease.released":
			s.stats.Released++
		case "lease.failed":
			s.stats.Failed++
		case "lease.expired":
			s.stats.Expired++
		case "lease.adopted":
			s.stats.Adopted++
		case "lease.reopened":
			s.stats.Reopened++
		case "rollout.completed":
			s.stats.Completed++
		case "rollout.superseded":
			s.stats.Superseded++
		case "rollout.halted":
			s.stats.Halts++
		case "action.abort":
			s.stats.Aborted++
		}
	}

	// A skip that outlived its rollout shows as a member that joined a later
	// rollout of the same key as skipped.
	seen := make(map[string]bool, 8)
	all, _, _ := s.store.view()

	s.stats.Forks = len(s.world.phases) - 1

	for _, r := range all {
		s.countRollout(r)

		for i := range r.Hosts {
			if !operatorSkipped(&r.Hosts[i]) && !strings.HasPrefix(r.Hosts[i].Error, skipLiftedPrefix) {
				continue
			}

			if id := r.Key + " " + r.Hosts[i].Host; seen[id] {
				s.stats.SkipsCarried++
			} else {
				seen[id] = true
			}
		}
	}

	rollouts, leases, _ := s.store.view()
	byID := make(map[string]*fleet.Rollout, len(rollouts))

	for _, r := range rollouts {
		byID[r.ID] = r
	}

	for _, l := range leases {
		ids := make(map[string]bool, len(l.Rollouts))
		exempt := len(l.Rollouts) > 0

		for _, id := range l.Rollouts {
			ids[id] = true
			exempt = exempt && byID[id].IgnoreBudget
		}

		if l.Force {
			s.stats.ForcedGrants++
		}

		s.countLease(l)

		if exempt {
			s.stats.ExemptGrants++
		}

		if s.world.livenessUnsupported && !exempt && l.Weight > 0 && l.GrantedBy != grantedByAdoption {
			s.stats.ProbeGrants++
		}

		if len(ids) > 1 {
			s.stats.Bundled++
		}
	}

	s.countWorld()
}

// countWorld counts what kind of world the run was in.
func (s *sim) countWorld() {
	s.stats.Hosts = len(s.world.agents)
	s.stats.Seeds = 1

	if s.world.start.Before(testGenesis) {
		s.stats.BeforeGenesis = 1
	}

	if s.world.bogus != nil {
		s.stats.FuturePhases = 1
	}

	if s.world.livenessUnsupported {
		s.stats.Unsupported = 1
	}
}

func newSim(seed uint64) (*sim, error) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	// Taken from the seed, not from the random stream, so that the other
	// seeds keep their histories: every fourth run begins 4 to 16 minutes
	// before the chain's genesis.
	start := simStart
	if seed%4 == 3 {
		start = testGenesis.Add(-time.Duration(4+seed%13) * time.Minute)
	}

	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	s := &sim{
		seed:    seed,
		rng:     rng,
		world:   newSimWorld(rng, start),
		store:   newFakeStore(),
		oracle:  newOracle(),
		log:     log,
		skipped: make(map[string]map[string]bool, 4),
		holds:   make(map[string]bool, 4),

		registered: make(map[string]time.Time, 16),
	}

	// Taken from the seed, not from the random stream, so that the other
	// seeds keep their histories.
	s.world.livenessUnsupported = seed%5 == 4

	// Every seventh run the beacon node announces a slot phase that is out of
	// bounds, for an epoch far beyond the run: slots of two hours, or a start
	// that does not follow from the phase before it. What is invalid in the
	// future invalidates nothing now.
	if seed%7 == 5 {
		at := 1_000_000 + seed%1000
		bogus := fleet.SlotPhase{Epoch: at, Start: s.world.epochStart(at), SlotMs: 2 * fleet.MaxSlotMs}

		if seed%2 == 0 {
			bogus.SlotMs = simSecondsPerSlot * 1000
			bogus.Start = bogus.Start.Add(time.Second)
		}

		s.world.bogus = &bogus
	}

	pick := func(values ...float64) float64 { return values[rng.IntN(len(values))] }
	policy := func() fleet.Policy {
		switch rng.IntN(8) {
		case 0:
			return fleet.PolicyManual
		case 1:
			return fleet.PolicyPaused
		default:
			return fleet.PolicyAuto
		}
	}

	s.defaults = fleet.Settings{
		MaxUnavailablePercent:   pick(0, 5, 10, 10, 20, 34, 50, 100),
		MinParticipationPercent: pick(66, 80, 90),
		MaxFinalityLagEpochs:    uint64(pick(2, 3, 5)),
		ReadyEpochs:             uint64(pick(0, 1, 1, 2)),
		MinPeers:                int(pick(1, 3, 3, 5)),
		LeaseTTL:                fleet.Duration(time.Duration(pick(60, 120, 300)) * time.Second),
		HostStaleAfter:          fleet.Duration(time.Duration(pick(30, 60, 120)) * time.Second),
		ReadinessTimeout:        fleet.Duration(time.Duration(pick(120, 300, 600)) * time.Second),
		PolicyImage:             policy(),
		PolicyPackages:          policy(),
		PolicyReboot:            policy(),
	}

	if err := s.defaults.Validate(); err != nil {
		return nil, fmt.Errorf("sim settings: %w", err)
	}

	if err := s.boot(s.store); err != nil {
		return nil, err
	}

	return s, s.tick(0)
}

func (s *sim) newID() string {
	s.ids++

	return fmt.Sprintf("x%05d", s.ids)
}

// service builds a scheduler over a store, as a controller start does.
func (s *sim) service(st *fakeStore, newID func() string) (*service, error) {
	svc, ok := New(s.log, Options{
		Store:    st,
		Health:   s.world.health,
		Defaults: s.defaults,
		Now:      func() time.Time { return s.world.now },
		NewID:    newID,
	}).(*service)
	if !ok {
		return nil, errors.New("sim: New did not return *service")
	}

	return svc, nil
}

// boot (re)starts the controller on the store. Its beacon service knows the
// epoch timing at once and what it has stored about liveness a little later.
func (s *sim) boot(st *fakeStore) error {
	svc, err := s.service(st, s.newID)
	s.svc = svc
	s.since = s.world.now

	if !s.world.calm {
		s.world.livenessUnknownUntil = later(s.world.livenessUnknownUntil,
			s.world.now.Add(time.Duration(s.rng.IntN(30))*time.Second))
	}

	return err
}

func (s *sim) note(format string, args ...any) {
	at := s.world.now.Sub(s.world.start).Truncate(time.Second)
	s.trail = append(s.trail, fmt.Sprintf("%4d +%-8s %s", s.step, at, fmt.Sprintf(format, args...)))
}

// report renders a failure so that the seed alone reproduces it.
func (s *sim) report(err error) string {
	settings, _ := s.store.LoadSettings(context.Background(), s.defaults)
	raw, _ := json.Marshal(settings)
	plan := s.svc.Plan()
	budget, _ := json.Marshal(plan.Budget)
	hosts, _ := json.Marshal(plan.Hosts)

	return fmt.Sprintf("seed %d, step %d: %v\nreplay: UPGRADOOR_SIM_SEED=%d go test -run TestSimulation ./pkg/controller/scheduler/\n"+
		"settings: %s\nfleet:\n%slast plan: budget %s gate %v\n  hosts %s\nlast steps:\n  %s",
		s.seed, s.step, err, s.seed, raw, s.world.describe(), budget, plan.Gate.Reasons, hosts,
		strings.Join(s.trail[max(0, len(s.trail)-simShown):], "\n  "))
}

// tick moves the clock, lets every agent that is not silent sync and
// reconciles.
func (s *sim) tick(d time.Duration) error {
	s.world.advance(d, s.store)

	if err := s.checkStarts(); err != nil {
		return err
	}

	if err := s.syncAgents(); err != nil {
		return err
	}

	return s.reconcile()
}

// checkStarts: an agent stops containers only under a lease that is open at
// that moment, unless its clock is slow and the lease expired on it. A lease the store has never seen (the database was lost) is
// adopted at the agent's next sync.
func (s *sim) checkStarts() error {
	for _, a := range s.world.agents {
		if a.op == nil || !a.op.Disrupted || !a.op.StartedAt.Equal(s.world.now) {
			continue
		}

		l, err := s.store.GetLease(context.Background(), a.op.LeaseID)

		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case (a.overrun || len(a.late) > 0) && l.State == fleet.LeaseExpired && l.Host == a.name:
			// Its clock is slow: it starts a lease the controller has expired,
			// or one the controller expired in favour of the lease it ran
			// late. The controller takes the lease up again.
		case l.State.Terminal() || l.Host != a.name:
			return fmt.Errorf("%s started to stop containers for lease %s, which is %s (host %s)",
				a.name, l.ID, l.State, l.Host)
		}
	}

	return nil
}

// syncAgents performs one sync per reachable agent: the request is stored
// as the API would, the response comes from Assignment.
func (s *sim) syncAgents() error {
	w := s.world
	ctx := context.Background()

	for _, a := range w.agents {
		if w.now.Before(a.silentUntil) {
			continue
		}

		req := w.request(a)
		host := &fleet.Host{Name: a.name, Validators: a.val, LastSeen: w.now, Status: req}

		if a.bootnode {
			host.Roles = []string{fleet.RoleBootnode}
		}

		if s.store.host(a.name) == nil {
			s.registered[a.name] = w.now
		}

		if s.store.recordSync(host) {
			s.stats.HoldsAdopted++
			s.note("%s: registered on hold, as its agent reports", a.name)
		}

		s.needsGone(a, req)

		a.lastSync = w.now
		a.lastSent = req

		resp, err := s.svc.Assignment(ctx, a.name)
		if err != nil {
			return fmt.Errorf("assignment of %s: %w", a.name, err)
		}

		if err := s.checkAssignment(a, resp); err != nil {
			return err
		}

		w.respond(a, resp)
	}

	return nil
}

// checkAssignment: a sync response carries only committed state.
func (s *sim) checkAssignment(a *simAgent, resp *Assignment) error {
	open, err := s.store.ListLeases(context.Background(), store.LeaseFilter{OpenOnly: true, Host: a.name})
	if err != nil {
		return err
	}

	switch {
	case resp.Lease == nil && len(open) == 0:
		return nil
	case resp.Lease == nil || len(open) == 0:
		return fmt.Errorf("assignment of %s: lease %v, but %d open leases are committed", a.name, resp.Lease, len(open))
	case resp.Lease.ID != open[0].ID:
		return fmt.Errorf("assignment of %s: lease %s, committed is %s", a.name, resp.Lease.ID, open[0].ID)
	}

	want := max(int64(open[0].ExpiresAt.Sub(s.world.now)/time.Second), 0)
	if resp.Lease.TTLSeconds != want {
		return fmt.Errorf("assignment of %s: ttl %d, %d remain", a.name, resp.Lease.TTLSeconds, want)
	}

	return nil
}

// reconcile runs one reconcile and observes its result. Now and then it
// also runs the same reconcile on a freshly built scheduler over a copy of
// the store: a restart must change no decision.
func (s *sim) reconcile() error {
	var (
		ctx    = context.Background()
		shadow *fakeStore
		ids    = s.ids
	)

	if !s.store.failing() && s.rng.IntN(10) == 0 {
		shadow = s.store.clone()
	}

	if err := s.svc.reconcile(ctx); err != nil {
		if errors.Is(err, errWrite) {
			s.note("  reconcile failed: %v", err)
			s.stats.WriteFailures++

			return s.observe(false)
		}

		return fmt.Errorf("reconcile: %w", err)
	}

	if shadow != nil {
		fresh, err := s.service(shadow, func() string {
			ids++

			return fmt.Sprintf("x%05d", ids)
		})
		if err != nil {
			return err
		}

		// Silence is counted from the scheduler's start, so the comparison is
		// between two schedulers that started at the same time; what a later
		// start changes is the outage step's business.
		fresh.since = s.svc.since

		if err := fresh.reconcile(ctx); err != nil {
			return fmt.Errorf("reconcile after a restart: %w", err)
		}

		if got, want := shadow.dump(), s.store.dump(); got != want {
			return fmt.Errorf("a restarted scheduler decided differently:\nrestarted:\n%s\nrunning:\n%s", got, want)
		}

		got, _ := json.Marshal(fresh.Plan())
		want, _ := json.Marshal(s.svc.Plan())

		if string(got) != string(want) {
			return fmt.Errorf("a restarted scheduler planned differently:\nrestarted: %s\nrunning: %s", got, want)
		}

		s.stats.Shadows++
	}

	if err := s.observe(true); err != nil {
		return err
	}

	return s.checkPlan()
}

// facts collects the world's state for the oracle.
func (s *sim) facts(fresh bool) *simFacts {
	w := s.world
	settings, _ := s.store.LoadSettings(context.Background(), s.defaults)

	f := &simFacts{
		now:            w.now,
		since:          s.since,
		settings:       settings,
		health:         w.health(),
		hosts:          make(map[string]*simHostFact, len(w.agents)),
		rows:           w.published,
		publishedEpoch: w.publishedEpoch,
		havePublished:  w.storedLiveness(),
		retried:        s.retried,
		skipped:        s.skipped,
		holds:          s.holds,
		registered:     s.registered,
		epochStart:     w.epochStart,
		epochOf:        w.epochOf,
		fresh:          fresh,
	}

	for _, a := range w.agents {
		registered := s.store.host(a.name)
		if registered == nil {
			continue
		}

		live := w.publishedLive(a)

		if w.livenessUnsupported {
			// The probe rule: the last sync as the agent sent it, and how long
			// ago that was as far as this controller can know.
			live = a.weight

			silent := w.now.Sub(later(a.lastSync, s.since)) > settings.HostStaleAfter.Std()
			if silent || (a.lastSent != nil && !oracleNodeBack(a.lastSent.Node, 1)) {
				live = 0
			}
		}

		f.hosts[a.name] = &simHostFact{
			weight:     a.weight,
			live:       live,
			holdWanted: registered.HoldWanted,
			lastSync:   a.lastSync,
			status:     a.lastSent,
			disrupted:  a.disrupted,
			late:       a.late,
			doneAt:     a.doneAt,
		}
	}

	return f
}

// observe hands the committed state to the oracle. It does so after every
// reconcile, also one that wrote nothing: a host that is stuck shows in
// reconciles that change nothing. An action that wrote nothing is not looked
// at again.
func (s *sim) observe(fresh bool) error {
	writes := s.store.writeCount()
	if !fresh && writes == s.observed && len(s.retried) == 0 {
		return nil
	}

	s.observed = writes

	rollouts, leases, directives := s.store.view()
	err := s.oracle.observe(s.facts(fresh), &simSnapshot{rollouts: rollouts, leases: leases, directives: directives})
	s.retried = nil

	return err
}

// checkPlan checks the published plan against the committed state.
func (s *sim) checkPlan() error {
	plan := s.svc.Plan()

	open, err := s.store.ListLeases(context.Background(), store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return err
	}

	leased := make(map[string]bool, len(open))
	for _, l := range open {
		leased[l.Host] = true
	}

	if plan.Budget.LeasedHosts != len(leased) {
		return fmt.Errorf("plan counts %d leased hosts, %d leases are open", plan.Budget.LeasedHosts, len(leased))
	}

	for i, q := range plan.Queue {
		switch {
		case q.Position != i+1:
			return fmt.Errorf("plan queue position %d at index %d", q.Position, i)
		case leased[q.Host]:
			return fmt.Errorf("plan queues %s, which holds a lease", q.Host)
		case !simReasons.MatchString(q.Reason):
			return fmt.Errorf("plan queues %s with undocumented reason %q", q.Host, q.Reason)
		case len(q.Rollouts) == 0:
			return fmt.Errorf("plan queues %s for no rollout", q.Host)
		}

		if s.stats.Waits == nil {
			s.stats.Waits = make(map[string]int, 12)
		}

		word, _, _ := strings.Cut(q.Reason, " ")
		s.stats.Waits[word]++
	}

	return nil
}

// act records the outcome of an operator action. Refusals and injected
// write failures are part of the game; anything else fails the run.
func (s *sim) act(what string, res *ActionResult, err error) error {
	switch {
	case err == nil:
		s.note("%s: %s", what, res.Message)
	case errors.Is(err, ErrInvalidAction), errors.Is(err, ErrUnknownHost),
		errors.Is(err, ErrUnknownRollout), errors.Is(err, ErrUnknownLease), errors.Is(err, errWrite):
		s.note("%s: %v", what, err)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}

	return s.observe(false)
}

func (s *sim) agent() *simAgent { return s.world.agents[s.rng.IntN(len(s.world.agents))] }

// someHosts picks one to three host names.
func (s *sim) someHosts() []string {
	names := make([]string, 0, 3)

	for range 1 + s.rng.IntN(3) {
		if name := s.agent().name; !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	return names
}

func (s *sim) duration(minSeconds, maxSeconds int) time.Duration {
	return time.Duration(minSeconds+s.rng.IntN(maxSeconds-minSeconds)) * time.Second
}

// pickRollout picks a rollout: usually an open one, preferring those that
// match want.
func (s *sim) pickRollout(want func(*fleet.Rollout) bool) *fleet.Rollout {
	all, _, _ := s.store.view()
	if len(all) == 0 {
		return nil
	}

	var open, wanted []*fleet.Rollout

	for _, r := range all {
		if !r.State.Terminal() {
			open = append(open, r)
		}

		if want != nil && want(r) {
			wanted = append(wanted, r)
		}
	}

	switch n := s.rng.IntN(10); {
	case len(wanted) > 0 && n < 8:
		return wanted[s.rng.IntN(len(wanted))]
	case len(open) > 0 && n < 9:
		return open[s.rng.IntN(len(open))]
	default:
		return all[s.rng.IntN(len(all))]
	}
}

// randomStep applies one random step: time passing, something happening in
// the world, an operator action, a controller restart or a store failure.
func (s *sim) randomStep() error {
	w := s.world

	switch n := s.rng.IntN(1000); {
	case n < 470:
		d := time.Duration([]int{2, 5, 5, 10, 10, 20, 30, 48}[s.rng.IntN(8)]) * time.Second
		s.note("tick +%s", d)

		return s.tick(d)
	case n < 520:
		image := w.images[s.rng.IntN(len(w.images))]

		// Every other time the tag that moves is one that a host is rolling
		// right now: the rollout is superseded while its lease is open.
		if running := s.rollingImages(); len(running) > 0 && s.rng.IntN(2) == 0 {
			image = running[s.rng.IntN(len(running))]
		}

		kind := digestGood

		switch s.rng.IntN(10) {
		case 0:
			kind = digestFails
		case 1:
			kind = digestMute
		}

		w.registry[image] = w.newDigest(kind)
		s.note("release %s -> %s (kind %d)", image, shortDigest(w.registry[image]), kind)
	case n < 540:
		w.pkgVersion++
		s.note("package release 1.%d", w.pkgVersion)
	case n < 560:
		a := s.agent()
		a.rebootRequired = true
		a.rebootSticks = s.rng.IntN(8) == 0
		s.note("%s: reboot required (sticks: %v)", a.name, a.rebootSticks)
	case n < 595:
		s.worldEvent()
	case n < 640:
		s.agentEvent()
	case n < 665:
		s.nodeEvent()
	case n < 690:
		s.stats.Restarts++

		if s.rng.IntN(4) == 0 {
			return s.outage()
		}

		s.note("controller restart")

		return s.boot(s.store)
	case n < 720:
		switch {
		case s.store.failing():
			s.store.setFailing(false, 0)
		case s.rng.IntN(4) == 0:
			s.store.setFailing(true, 0)
		default:
			s.store.setFailing(false, 1+s.rng.IntN(3))
		}

		s.note("store: failing=%v", s.store.failing())
	case n < 726:
		return s.loseDatabase()
	case n < 729:
		return s.previousBuild()
	default:
		return s.operatorAction()
	}

	w.sample()

	return nil
}

// outage takes the controller down for longer than leaseTtl and starts it
// again on the same store. While it is down the world goes on (agents work on
// what they hold, the beacon service stores nothing) but nobody can sync. The
// first reconcile after the start runs before any agent has synced again: the
// outage alone must not fail a lease.
func (s *sim) outage() error {
	ctx := context.Background()
	w := s.world

	settings, err := s.store.LoadSettings(ctx, s.defaults)
	if err != nil {
		return err
	}

	// Longer than leaseTtl, and every other time longer than readinessTimeout
	// too: a host that was being checked must not fail for it.
	down := settings.LeaseTTL.Std() + s.duration(1, 240)
	if s.rng.IntN(2) == 0 {
		down = max(down, settings.ReadinessTimeout.Std()+s.duration(1, 120))
	}

	s.note("controller down for %s, then restart", down)

	w.beaconDownUntil = later(w.beaconDownUntil, w.now.Add(down))

	for left := down; left > 0; {
		step := min(left, 10*time.Second)
		left -= step

		w.advance(step, s.store)

		if err = s.checkStarts(); err != nil {
			return err
		}
	}

	if err = s.boot(s.store); err != nil {
		return err
	}

	carried, err := s.store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return err
	}

	s.stats.Outages++
	s.stats.OutageLeases += len(carried)

	if err := s.reconcile(); err != nil {
		return err
	}

	for _, was := range carried {
		l, err := s.store.GetLease(ctx, was.ID)
		if err != nil {
			return err
		}

		if l.State == fleet.LeaseFailed && l.Error == reasonAgentSilent {
			return fmt.Errorf("lease %s of %s (%s) failed as %q on the first reconcile after an outage of %s",
				l.ID, l.Host, was.State, l.Error, down)
		}
	}

	return nil
}

// loseDatabase restarts the controller on an empty database: rollouts,
// leases, settings and liveness are gone. Agents that are in the middle of an
// operation keep reporting it, and the scheduler must adopt those leases.
func (s *sim) loseDatabase() error {
	s.note("database lost")
	s.stats.LostDatabases++

	s.countLedger()

	s.store = newFakeStore()
	s.oracle = newOracle()
	s.observed = 0
	s.retried = nil
	s.skipped = make(map[string]map[string]bool, 4)
	s.registered = make(map[string]time.Time, 16)

	for _, a := range s.world.agents {
		s.world.dropped[a.name] = true
	}

	return s.boot(s.store)
}

// previousBuild restarts the controller on closed rollouts as the builds
// before this one wrote them. Those builds kept on a rollout what it took
// over from the rollouts before it and what it left to those after it
// (inherited, bequest, bequeathed, and carriedFrom on a member); this build
// keeps none of it and must read such rows without minding. The oldest of
// them never gave a member of a closed rollout the outcome of a lease that
// ended after the rollout was closed: such a member is still leased, by a
// lease that is long over. Who is broken is read off the lease table, which
// every build wrote alike, so none of this may change a decision.
func (s *sim) previousBuild() error {
	if s.store.failing() {
		return nil
	}

	rollouts, leases, _ := s.store.snapshot()
	rewritten := make([]*fleet.Rollout, 0, len(rollouts))

	for _, r := range rollouts {
		if !r.State.Terminal() || r.EndedAt == nil {
			continue
		}

		for _, l := range leases {
			m := r.Host(l.Host)
			served := slices.ContainsFunc(sortedKeys(l.Rollouts), func(need string) bool { return l.Rollouts[need] == r.ID })

			// A lease that ended after the rollout was closed was never
			// settled in it.
			if m == nil || !served || l.EndedAt == nil || !l.EndedAt.After(*r.EndedAt) || operatorSkipped(m) {
				continue
			}

			m.State, m.LeaseID, m.Error = fleet.HostLeased, l.ID, ""
		}

		legacy, err := s.legacyRow(r)
		if err != nil {
			return err
		}

		rewritten = append(rewritten, legacy)
	}

	s.note("the closed rollouts are rows of an earlier build (%d), controller restart", len(rewritten))
	s.stats.PreviousBuilds++

	s.store.put(rewritten...)

	return s.boot(s.store)
}

// legacyRow gives a rollout the fields that the builds before this one
// stored with it, at random, and reads it the way the store reads a row.
func (s *sim) legacyRow(r *fleet.Rollout) (*fleet.Rollout, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("legacy row: %w", err)
	}

	row := make(map[string]any, 24)
	if err = json.Unmarshal(raw, &row); err != nil {
		return nil, fmt.Errorf("legacy row: %w", err)
	}

	entry := func() map[string]any {
		e := map[string]any{"host": s.agent().name, "from": fmt.Sprintf("x%05d", s.rng.IntN(s.ids+1))}
		if s.rng.IntN(2) == 0 {
			e["lease"] = fmt.Sprintf("x%05d", s.rng.IntN(s.ids+1))
		}

		return e
	}

	if s.rng.IntN(2) == 0 {
		row["inherited"] = []any{entry()}
	}

	// The build before this one recorded the bequest with a marker; the one
	// before that without.
	switch s.rng.IntN(3) {
	case 0:
		row["bequest"] = []any{entry(), entry()}
		row["bequeathed"] = true
	case 1:
		row["bequest"] = []any{entry()}
	}

	if members, ok := row["hosts"].([]any); ok {
		for _, m := range members {
			if member, ok := m.(map[string]any); ok && s.rng.IntN(3) == 0 {
				member["carriedFrom"] = fmt.Sprintf("x%05d", s.rng.IntN(s.ids+1))
			}
		}
	}

	if raw, err = json.Marshal(row); err != nil {
		return nil, fmt.Errorf("legacy row: %w", err)
	}

	out := &fleet.Rollout{}
	if err = json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("a row of an earlier build cannot be read: %w", err)
	}

	return out, nil
}

// rollingImages lists the images that some agent is rolling right now.
func (s *sim) rollingImages() []string {
	var images []string

	for _, a := range s.world.agents {
		if a.op == nil || a.op.Phase.Terminal() || a.lease == nil {
			continue
		}

		for i := range a.lease.Needs {
			if n := &a.lease.Needs[i]; n.Kind == protocol.NeedImage && !slices.Contains(images, n.Image.Image) {
				images = append(images, n.Image.Image)
			}
		}
	}

	slices.Sort(images)

	return images
}

// worldEvent starts or ends trouble that has nothing to do with rollouts.
func (s *sim) worldEvent() {
	w := s.world

	switch s.rng.IntN(13) {
	case 0, 1, 2, 3:
		a := s.agent()
		if w.now.Before(a.outageUntil) {
			a.outageUntil = w.now
		} else {
			a.outageUntil = w.now.Add(s.duration(20, 600))
		}

		s.note("%s: outage until +%s", a.name, a.outageUntil.Sub(w.now))
	case 4, 5:
		w.dropPercent = uint64(5 + s.rng.IntN(60))
		w.dropUntil = w.now.Add(s.duration(30, 400))
		s.note("participation drops by %d%% for %s", w.dropPercent, w.dropUntil.Sub(w.now))
	case 6, 7:
		w.finalityStallSince = w.now
		w.finalityStallUntil = w.now.Add(s.duration(60, 500))
		s.note("finality stalls for %s", w.finalityStallUntil.Sub(w.now))
	case 8:
		w.beaconDownUntil = w.now.Add(s.duration(30, 300))
		s.note("beacon node down for %s", w.beaconDownUntil.Sub(w.now))
	case 11:
		// A fork changes the slot duration, in one to three epochs.
		slot := []time.Duration{3 * time.Second, 12 * time.Second, 4500 * time.Millisecond, 6 * time.Second}[s.rng.IntN(4)]
		if at := w.epochOf(w.now) + 1 + uint64(s.rng.IntN(3)); w.fork(at, slot) {
			s.note("fork at epoch %d: slots of %s", at, slot)
		}
	case 9:
		// One epoch or several in which nobody is seen attesting.
		w.blackoutUntil = w.now.Add(s.duration(20, 220))
		s.note("no attestations seen for %s", w.blackoutUntil.Sub(w.now))
	default:
		if s.rng.IntN(2) == 0 {
			w.livenessStallUntil = w.now.Add(s.duration(60, 400))
			s.note("liveness stalls for %s", w.livenessStallUntil.Sub(w.now))
		} else {
			w.livenessUnknownUntil = w.now.Add(s.duration(60, 400))
			s.note("liveness unknown for %s", w.livenessUnknownUntil.Sub(w.now))
		}
	}
}

// recovering picks a host whose agent has finished an operation and waits
// for the controller to find it ready, when there is one; else any host.
func (s *sim) recovering() *simAgent {
	var done []*simAgent

	for _, a := range s.world.agents {
		if a.op != nil && a.op.Phase == protocol.PhaseDone && a.op.Disrupted {
			done = append(done, a)
		}
	}

	if len(done) == 0 {
		return s.agent()
	}

	return done[s.rng.IntN(len(done))]
}

// nodeEvent gives one node trouble that the readiness check must notice,
// most often a node that has just come back from an operation: it falls
// behind, loses peers it had, loses its execution client, or its agent goes
// silent.
func (s *sim) nodeEvent() {
	w := s.world

	a := s.agent()
	if s.rng.IntN(4) != 0 {
		a = s.recovering()
	}

	switch s.rng.IntN(6) {
	case 0:
		a.syncingUntil = w.now.Add(s.duration(5, 120))
		a.syncingEL = s.rng.IntN(2) == 0
		s.note("%s: node syncing for %s (EL: %v)", a.name, a.syncingUntil.Sub(w.now), a.syncingEL)
	case 1, 2:
		a.peerCap = s.rng.IntN(4)
		a.peerCapUntil = w.now.Add(s.duration(5, 200))
		s.note("%s: at most %d peers for %s", a.name, a.peerCap, a.peerCapUntil.Sub(w.now))
	case 3:
		if s.rng.IntN(2) == 0 {
			a.elDownUntil = w.now.Add(s.duration(5, 120))
			s.note("%s: EL down for %s", a.name, a.elDownUntil.Sub(w.now))
		} else {
			a.elOfflineUntil = w.now.Add(s.duration(5, 120))
			s.note("%s: CL reports its EL offline for %s", a.name, a.elOfflineUntil.Sub(w.now))
		}
	default:
		a.silentUntil = w.now.Add(s.duration(20, 400))
		s.note("%s: silent for %s", a.name, a.silentUntil.Sub(w.now))
	}
}

// agentEvent changes something on one host.
func (s *sim) agentEvent() {
	w := s.world
	a := s.agent()

	switch s.rng.IntN(13) {
	case 10:
		a.dead = !a.dead
		s.note("%s: validators dead: %v", a.name, a.dead)
	case 11:
		a.sloppy = !a.sloppy
		s.note("%s: reports needs it may not serve: %v", a.name, a.sloppy)
	case 12:
		a.blind = !a.blind
		s.note("%s: registry polls see nothing: %v", a.name, a.blind)
	case 0, 1, 2, 3:
		a.silentUntil = w.now.Add(s.duration(10, 400))
		s.note("%s: silent for %s", a.name, a.silentUntil.Sub(w.now))
	case 4:
		// The agent restarts: it looks at its registries again.
		a.silentUntil = w.now.Add(s.duration(2, 20))
		a.nextPoll = a.silentUntil
		s.note("%s: agent restart", a.name)
	case 5:
		verb := allVerbs[s.rng.IntN(len(allVerbs))]
		if a.allows(verb) {
			a.verbs = slices.DeleteFunc(a.verbs, func(v protocol.Verb) bool { return v == verb })
		} else {
			a.verbs = append(a.verbs, verb)
		}

		s.note("%s: verbs now %v", a.name, a.verbs)
	case 6:
		c := a.containers[s.rng.IntN(len(a.containers))]
		if !c.managed || a.down {
			return
		}

		c.custom = !c.custom
		if !c.custom {
			// Ansible redeployed the configured image.
			c.running, c.seen, c.checkedAt = w.registry[c.image], w.registry[c.image], w.now
		}

		s.note("%s: container %s custom=%v", a.name, c.name, c.custom)
	case 7, 8:
		a.localHold = !a.localHold
		s.note("%s: local hold %v", a.name, a.localHold)
	default:
		// Someone redeploys the host by hand.
		if a.down {
			return
		}

		for _, c := range a.containers {
			if c.managed && !c.custom {
				c.running, c.seen, c.checkedAt = w.registry[c.image], w.registry[c.image], w.now
			}
		}

		s.note("%s: redeployed by hand", a.name)
	}
}

// operatorAction performs one random action of a person.
func (s *sim) operatorAction() error {
	ctx := context.Background()

	switch n := s.rng.IntN(100); {
	case n < 6:
		res, err := s.svc.Pause(ctx, simActor, ScopeRequest{Reason: "sim"})

		return s.act("pause", res, err)
	case n < 16:
		res, err := s.svc.Resume(ctx, simActor, ScopeRequest{})

		return s.act("resume", res, err)
	case n < 30:
		return s.rolloutAction(n)
	case n < 44:
		r := s.pickRollout(func(r *fleet.Rollout) bool {
			return r.State == fleet.RolloutHalted || r.State == fleet.RolloutAborted ||
				(s.rng.IntN(4) == 0 && len(skippedMembers(r)) > 0)
		})
		if r == nil {
			return nil
		}

		var hosts []string

		switch s.rng.IntN(5) {
		case 0:
			hosts = s.someHosts()
		case 1:
			hosts = skippedMembers(r)
		}

		return s.retry(r, hosts)
	case n < 50:
		hosts := s.someHosts()
		res, err := s.svc.Hold(ctx, simActor, HoldRequest{Hosts: hosts, Reason: "sim"})
		s.noteHolds(hosts)

		return s.act(fmt.Sprintf("hold %v", hosts), res, err)
	case n < 60:
		hosts := s.someHosts()
		res, err := s.svc.Unhold(ctx, simActor, HoldRequest{Hosts: hosts})
		s.noteHolds(hosts)

		return s.act(fmt.Sprintf("unhold %v", hosts), res, err)
	case n < 80:
		return s.manualAction()
	case n < 94:
		return s.settingsAction()
	case n < 97:
		return s.endLease()
	default:
		hosts := s.someHosts()[:1]
		res, err := s.svc.Forget(ctx, simActor, HoldRequest{Hosts: hosts})

		// The stored liveness rows of a forgotten host are gone, and so is
		// what the person noted about it.
		if s.store.host(hosts[0]) == nil {
			s.world.dropped[hosts[0]] = true

			for _, byHost := range s.skipped {
				delete(byHost, hosts[0])
			}
		}

		return s.act(fmt.Sprintf("forget %v", hosts), res, err)
	}
}

// endLease is the person who ends an open lease: any one, whatever it does.
// The agent finishes what it has started; the host is broken from then on. A
// lease that the agent has not started as far as the controller knows is
// refused.
func (s *sim) endLease() error {
	ctx := context.Background()

	open, err := s.store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return err
	}

	id := "nosuchlease"
	if len(open) > 0 {
		id = open[s.rng.IntN(len(open))].ID
	}

	res, err := s.svc.EndLease(ctx, simActor, EndLeaseRequest{Lease: id, Reason: "sim"})

	return s.act("end lease "+id, res, err)
}

// rolloutAction pauses, resumes (approves), aborts or skips in one rollout.
func (s *sim) rolloutAction(n int) error {
	ctx := context.Background()

	r := s.pickRollout(nil)
	if r == nil {
		return nil
	}

	switch n % 7 {
	case 0:
		res, err := s.svc.Pause(ctx, simActor, ScopeRequest{Rollout: r.ID})

		return s.act("pause "+r.ID, res, err)
	case 1, 2, 3:
		res, err := s.svc.Resume(ctx, simActor, ScopeRequest{Rollout: r.ID})

		return s.act("resume "+r.ID, res, err)
	case 4:
		res, err := s.svc.Abort(ctx, simActor, ScopeRequest{Rollout: r.ID, Reason: "sim"})

		return s.act("abort "+r.ID, res, err)
	default:
		hosts := s.someHosts()
		res, err := s.svc.Skip(ctx, simActor, RolloutHostsRequest{Rollout: r.ID, Hosts: hosts})
		if err == nil && r.Kind != fleet.RolloutManual {
			for _, name := range res.Hosts {
				if s.skipped[r.Key] == nil {
					s.skipped[r.Key] = make(map[string]bool, 2)
				}

				s.skipped[r.Key][name] = true
				s.stats.Skips++
			}
		}

		return s.act(fmt.Sprintf("skip %s %v", r.ID, hosts), res, err)
	}
}

// manualAction requests a manual roll or reboot.
func (s *sim) manualAction() error {
	var (
		ctx    = context.Background()
		hosts  = s.someHosts()
		all    = s.rng.IntN(6) == 0
		stag   = s.rng.IntN(2) == 0
		force  = s.rng.IntN(4) == 0
		ignore = s.rng.IntN(6) == 0
	)

	if all {
		hosts = nil
	}

	what := fmt.Sprintf("%v all=%v staggered=%v force=%v ignoreBudget=%v", hosts, all, stag, force, ignore)

	if s.rng.IntN(2) == 0 {
		res, err := s.svc.Roll(ctx, simActor, RollRequest{
			Hosts: hosts, All: all, Staggered: stag, Force: force, IgnoreBudget: ignore, Reason: "sim",
		})

		return s.act("roll "+what, res, err)
	}

	res, err := s.svc.Reboot(ctx, simActor, RebootRequest{
		Hosts: hosts, All: all, Staggered: stag, Force: force, IgnoreBudget: ignore, Reason: "sim",
	})

	return s.act("reboot "+what, res, err)
}

// settingsAction changes one setting.
func (s *sim) settingsAction() error {
	ctx := context.Background()

	set, err := s.svc.Settings(ctx)
	if err != nil {
		return err
	}

	policies := []fleet.Policy{fleet.PolicyAuto, fleet.PolicyAuto, fleet.PolicyManual, fleet.PolicyPaused}
	what := ""

	switch s.rng.IntN(9) {
	case 0, 1, 2:
		set.MaxUnavailablePercent = []float64{0, 5, 10, 20, 34, 50, 100}[s.rng.IntN(7)]
		what = fmt.Sprintf("maxUnavailablePercent=%v", set.MaxUnavailablePercent)
	case 3:
		set.MinParticipationPercent = []float64{50, 66, 80, 90, 95}[s.rng.IntN(5)]
		what = fmt.Sprintf("minParticipationPercent=%v", set.MinParticipationPercent)
	case 4:
		if s.rng.IntN(2) == 0 {
			set.MinPeers = []int{1, 2, 3, 5}[s.rng.IntN(4)]
			what = fmt.Sprintf("minPeers=%d", set.MinPeers)

			break
		}

		set.ReadyEpochs = uint64(s.rng.IntN(3))
		what = fmt.Sprintf("readyEpochs=%d", set.ReadyEpochs)
	case 5:
		set.PolicyImage = policies[s.rng.IntN(len(policies))]
		what = "policyImage=" + string(set.PolicyImage)
	case 6:
		set.PolicyPackages = policies[s.rng.IntN(len(policies))]
		what = "policyPackages=" + string(set.PolicyPackages)
	case 7:
		set.PolicyReboot = policies[s.rng.IntN(len(policies))]
		what = "policyReboot=" + string(set.PolicyReboot)
	default:
		set.MaxUnavailablePercent = 250
		what = "maxUnavailablePercent=250 (invalid)"
	}

	err = s.svc.UpdateSettings(ctx, simActor, set)

	return s.act("settings "+what, &ActionResult{Message: "saved"}, err)
}

// converge heals the world, lets a person lift every block and retry what
// halted, and then expects every host on its target and no open rollout.
func (s *sim) converge() error {
	w := s.world
	ctx := context.Background()

	w.calm = true
	w.beaconDownUntil, w.finalityStallUntil, w.dropUntil = time.Time{}, time.Time{}, time.Time{}
	w.blackoutUntil = time.Time{}
	w.livenessStallUntil, w.livenessUnknownUntil = time.Time{}, time.Time{}
	s.store.setFailing(false, 0)

	for d := range w.digestKind {
		w.digestKind[d] = digestGood
	}

	for _, a := range w.agents {
		a.silentUntil, a.outageUntil, a.localHold, a.rebootSticks = time.Time{}, time.Time{}, false, false
		a.syncingUntil, a.elOfflineUntil, a.elDownUntil = time.Time{}, time.Time{}, time.Time{}
		a.peerCapUntil, a.elLag = time.Time{}, 0
		a.dead, a.sloppy, a.blind = false, false, false
	}

	set, err := s.svc.Settings(ctx)
	if err != nil {
		return err
	}

	set.Paused = false
	set.PolicyImage, set.PolicyPackages, set.PolicyReboot = fleet.PolicyAuto, fleet.PolicyAuto, fleet.PolicyAuto
	set.LeaseTTL = fleet.Duration(5 * time.Minute)

	if set.MaxUnavailablePercent == 0 {
		set.MaxUnavailablePercent = 10
	}

	if err := s.svc.UpdateSettings(ctx, simActor, set); err != nil {
		return fmt.Errorf("converge: settings: %w", err)
	}

	s.note("heal the world")

	if err := s.boot(s.store); err != nil {
		return err
	}

	problem := errors.New("not started")

	for range simConvergeTicks {
		s.step++
		s.note("tick +24s")

		if err := s.tick(24 * time.Second); err != nil {
			return err
		}

		if err := s.operate(); err != nil {
			return err
		}

		s.stats.ConvergeTicks++
		s.stats.MaxConvergeTicks = s.stats.ConvergeTicks

		if problem = s.converged(); problem == nil {
			return nil
		}
	}

	raw, _ := json.Marshal(s.svc.Plan().Queue)

	return fmt.Errorf("no convergence after %d ticks: %w\nqueue: %s\nopen rollouts:\n%s",
		simConvergeTicks, problem, raw, s.openRollouts())
}

func (s *sim) openRollouts() string {
	var b strings.Builder

	rollouts, _, _ := s.store.view()
	for _, r := range rollouts {
		if !r.State.Terminal() {
			raw, _ := json.Marshal(r)
			b.Write(raw)
			b.WriteByte('\n')
		}
	}

	return b.String()
}

// operate is the person of the convergence phase: lift holds and pauses,
// retry what halted, and reopen aborted causes that hosts still report.
func (s *sim) operate() error {
	ctx := context.Background()
	rollouts, _, _ := s.store.view()
	open := make(map[string]bool, len(rollouts))

	for _, a := range s.world.agents {
		if h := s.store.host(a.name); h != nil && h.HoldWanted {
			res, err := s.svc.Unhold(ctx, simActor, HoldRequest{Hosts: []string{a.name}})
			s.noteHolds([]string{a.name})

			if actErr := s.act("unhold "+a.name, res, err); actErr != nil {
				return actErr
			}
		}
	}

	for _, r := range rollouts {
		if r.State.Terminal() {
			continue
		}

		open[r.Key] = true

		if r.Paused {
			res, err := s.svc.Resume(ctx, simActor, ScopeRequest{Rollout: r.ID})
			if actErr := s.act("resume "+r.ID, res, err); actErr != nil {
				return actErr
			}
		}

		failed := slices.ContainsFunc(r.Hosts, func(m fleet.RolloutHost) bool { return m.State == fleet.HostFailed })
		if r.State == fleet.RolloutHalted || failed {
			if err := s.retry(r, nil); err != nil {
				return err
			}
		}

		// A skip is taken back by naming the host.
		if hosts := skippedMembers(r); len(hosts) > 0 {
			if err := s.retry(r, hosts); err != nil {
				return err
			}
		}
	}

	reported := s.reportedKeys()
	latest := make(map[string]bool, len(rollouts))

	for i := len(rollouts) - 1; i >= 0; i-- {
		r := rollouts[i]
		if latest[r.Key] {
			continue
		}

		latest[r.Key] = true

		// An aborted cause is reopened; the skips of a closed rollout whose
		// hosts still report the need are lifted.
		if !open[r.Key] && reported[r.Key] &&
			(r.State == fleet.RolloutAborted || len(skippedMembers(r)) > 0) {
			if err := s.retry(r, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

// noteHolds takes down what a hold or unhold did to the named hosts, as the
// person sees it afterwards (an action may fail half way). A host the
// controller does not know keeps what was noted before.
func (s *sim) noteHolds(hosts []string) {
	for _, name := range hosts {
		if h := s.store.host(name); h != nil {
			s.holds[name] = h.HoldWanted
		}
	}
}

// skippedMembers names the members of a rollout that a person skipped.
func skippedMembers(r *fleet.Rollout) []string {
	var hosts []string

	for i := range r.Hosts {
		if operatorSkipped(&r.Hosts[i]) {
			hosts = append(hosts, r.Hosts[i].Host)
		}
	}

	return hosts
}

// retry retries a rollout as a person would, for all hosts or the named
// ones. Every host the action names as retried is no longer skipped for the
// rollout's key.
func (s *sim) retry(r *fleet.Rollout, hosts []string) error {
	res, err := s.svc.Retry(context.Background(), simActor, RolloutHostsRequest{Rollout: r.ID, Hosts: hosts})
	if err == nil {
		s.retried = append(s.retried, r.ID)

		for _, name := range res.Hosts {
			if s.skipped[r.Key][name] {
				delete(s.skipped[r.Key], name)
				s.stats.SkipsLifted++
			}
		}
	}

	return s.act(fmt.Sprintf("retry %s %v", r.ID, hosts), res, err)
}

// needsGone forgets the skips of a host for the needs it no longer reports:
// a skip holds for as long as the host keeps reporting the need. A held host
// reports nothing and keeps its skips.
func (s *sim) needsGone(a *simAgent, req *protocol.SyncRequest) {
	if h := s.store.host(a.name); h == nil || h.Held() {
		return
	}

	for key, byHost := range s.skipped {
		if byHost[a.name] && !slices.ContainsFunc(req.Needs, func(n protocol.Need) bool {
			return needKey(&n) == key
		}) {
			delete(byHost, a.name)
		}
	}
}

// reportedKeys are the rollout keys of the observed needs the agents report.
func (s *sim) reportedKeys() map[string]bool {
	keys := make(map[string]bool, 8)

	for _, a := range s.world.agents {
		if a.lastSent == nil {
			continue
		}

		for i := range a.lastSent.Needs {
			n := &a.lastSent.Needs[i]

			switch {
			case n.DirectiveID != "":
			case n.Kind == protocol.NeedImage:
				keys[imageKey(n.Image.Image, n.Image.ToDigest)] = true
			case n.Kind == protocol.NeedPackages:
				keys[prefixPackages+protocol.PackagesKey(n.Packages.Packages)] = true
			case n.Kind == protocol.NeedReboot:
				keys[keyReboot] = true
			}
		}
	}

	return keys
}

// converged reports what is still missing: an open rollout or lease, or a
// host that is not on its target.
func (s *sim) converged() error {
	rollouts, leases, _ := s.store.view()

	for _, r := range rollouts {
		if !r.State.Terminal() {
			return fmt.Errorf("rollout %s (%s) is still %s", r.ID, r.Key, r.State)
		}
	}

	for _, l := range leases {
		if !l.State.Terminal() {
			return fmt.Errorf("lease %s of %s is still %s", l.ID, l.Host, l.State)
		}
	}

	for _, a := range s.world.agents {
		if err := s.world.onTarget(a); err != nil {
			return err
		}
	}

	return nil
}

// run performs the random steps and the convergence of one seed. It returns
// a failure report, or "".
func runSim(seed uint64, steps int) (string, *simStats) {
	s, err := newSim(seed)
	if err != nil {
		if s == nil {
			return fmt.Sprintf("seed %d: %v", seed, err), &simStats{}
		}

		return s.report(err), &s.stats
	}

	for s.step = 1; s.step <= steps; s.step++ {
		if err := s.randomStep(); err != nil {
			return s.report(err), &s.stats
		}
	}

	if err := s.converge(); err != nil {
		return s.report(err), &s.stats
	}

	s.count()

	return "", &s.stats
}

// simSeeds is which seeds to run: one from UPGRADOOR_SIM_SEED, a count from
// UPGRADOOR_SIM_SEEDS, else 300, or 60 in short mode.
func simSeeds(t *testing.T) (first, n uint64) {
	t.Helper()

	if v := os.Getenv("UPGRADOOR_SIM_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("UPGRADOOR_SIM_SEED: %v", err)
		}

		return seed, 1
	}

	if v := os.Getenv("UPGRADOOR_SIM_SEEDS"); v != "" {
		count, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("UPGRADOOR_SIM_SEEDS: %v", err)
		}

		return 0, count
	}

	if testing.Short() {
		return 0, 60
	}

	return 0, 300
}

// TestSimulation drives random fleets through random histories and checks
// the invariants of docs/scheduler.md after every step, then convergence. A
// failure names the seed, the fleet, the settings and the last steps; the
// seed alone reproduces it.
func TestSimulation(t *testing.T) {
	const maxFailures = 3

	steps := 300
	if testing.Short() {
		steps = 150
	}

	first, n := simSeeds(t)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures = make(map[uint64]string, maxFailures)
		seeds    = make(chan uint64)
		total    simStats
	)

	for range min(uint64(runtime.GOMAXPROCS(0)), n) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for seed := range seeds {
				report, stats := runSim(seed, steps)

				mu.Lock()
				total.add(stats)

				if report != "" {
					failures[seed] = report
				}

				mu.Unlock()
			}
		}()
	}

	for seed := first; seed < first+n; seed++ {
		mu.Lock()
		stop := len(failures) >= maxFailures
		mu.Unlock()

		if stop {
			break
		}

		seeds <- seed
	}

	close(seeds)
	wg.Wait()

	failed := make([]uint64, 0, len(failures))
	for seed := range failures {
		failed = append(failed, seed)
	}

	slices.Sort(failed)

	for _, seed := range failed[:min(len(failed), maxFailures)] {
		t.Errorf("%s", failures[seed])
	}

	if len(failed) > maxFailures {
		t.Errorf("%d more seeds failed: %v", len(failed)-maxFailures, failed[maxFailures:])
	}

	raw, _ := json.Marshal(total)
	t.Logf("simulated %d steps per seed: %s", steps, raw)

	// A simulation that never grants, fails or waits proves nothing.
	if len(failed) == 0 && n >= 50 {
		for what, count := range map[string]int{
			"grants": total.Grants, "released leases": total.Released, "failed leases": total.Failed,
			"expired leases": total.Expired, "completed rollouts": total.Completed,
			"superseded rollouts": total.Superseded, "halts": total.Halts, "forced grants": total.ForcedGrants,
			"budget-exempt grants": total.ExemptGrants, "bundled leases": total.Bundled,
			"restart comparisons": total.Shadows, "write failures": total.WriteFailures,
			"adopted leases":                             total.Adopted,
			"holds adopted from the agents":              total.HoldsAdopted,
			"outages longer than leaseTtl":               total.Outages,
			"open leases carried over an outage":         total.OutageLeases,
			"runs without liveness":                      total.Unsupported,
			"budgeted validator grants without liveness": total.ProbeGrants,
			"skipped hosts":                              total.Skips,
			"skips taken back":                           total.SkipsLifted,
			"skips carried over":                         total.SkipsCarried,
			"repairs through the closed gate":            total.Repairs,
			"members in the repair stage":                total.BrokenMembers,
			"broken hosts granted first":                 total.BrokenFirst,
			"broken hosts repaired":                      total.Repaired,
			"hosts held back by a lease in flight":       total.FlightHeld,
			"leases ended by a person":                   total.EndedByOperator,
			"runs with a slot phase out of bounds":       total.FuturePhases,
			"failed leases with a step done":             total.MixedLeases,
			"slot-duration forks":                        total.Forks,
			"expired leases taken up again":              total.Reopened,
			"leases that ended after their rollout":      total.SettledClosed,
			"waits for a verb the agent lacks":           total.Waits["verb"],
			"waits for the budget":                       total.Waits["budget"], "waits for the host limit": total.Waits["host"],
			"waits for the gate": total.Waits["network"], "waits for the order": total.Waits["after"],
		} {
			if count == 0 {
				t.Errorf("the simulation never produced any %s", what)
			}
		}
	}

	// What is rare shows in a long run only.
	if len(failed) == 0 && n >= 1000 {
		for what, count := range map[string]int{
			"runs that begin before genesis":                                   total.BeforeGenesis,
			"restarts on rows of an earlier build":                             total.PreviousBuilds,
			"hosts held back by the lease of an aborted rollout":               total.FlightAborted,
			"hosts held back by a lease that was taken up again":               total.FlightReopened,
			"hosts held back in a rollout that a retry opened":                 total.FlightRetried,
			"hosts held back in a rollout that was open when the other closed": total.FlightNextToOpen,
			"hosts broken by a lease that a person ended":                      total.BrokenByOperator,
			"hosts broken by a lease of a rollout that had been aborted":       total.BrokenAfterAbort,
		} {
			if count == 0 {
				t.Errorf("the simulation never produced any %s", what)
			}
		}
	}
}
