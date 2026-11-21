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
// leaseTtl, some on an empty database) and store write failures. Nodes come
// back from an operation syncing, without their EL or slow to find peers,
// lose peers they had, and agents go silent. Every fourth run begins before
// the chain's genesis, every third pauses rollouts after their canary and
// every other compares a host that comes back with the peers of the fleet.
//
// After every reconcile and every action the oracle (sim_oracle_test.go)
// holds the committed state against its invariants; now and then the same
// reconcile is run on a freshly started scheduler, which must decide the
// same; at the end of a run the world is healed and everything must
// converge.
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
var simReasons = regexp.MustCompile(`^(held|known offline|agent silent|working on an operation without a lease|paused|rollout paused|` +
	`paused after the canary; resume to continue|aborted(: .+)?; resume to continue|` +
	`waiting for approval \(policy manual\)|rollout halted: .+|` +
	`verb not allowed by the agent|network gate: .+|after .+|` +
	`budget [0-9.]+% of [0-9.]+%|host limit \d+ of \d+|independent limit \d+ of \d+)$`)

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
	// retried are the rollouts retried since the last observation.
	retried []string
	// cancelled names the leases a person ended. It is lost with the
	// database.
	cancelled map[string]bool
	// checked is, per agent, how many of the steps it began were looked at;
	// started names the leases whose operation was seen to begin.
	checked map[string]int
	started map[string]bool
	// stats counts what happened, so that the test can tell a simulation that
	// exercised the rules from one that idled.
	stats simStats
}

// simStats counts events by name.
type simStats map[string]int

func (a simStats) add(b simStats) {
	for k, v := range b {
		if k == statMaxConverge {
			a[k] = max(a[k], v)

			continue
		}

		a[k] += v
	}
}

// statMaxConverge is the longest convergence of any seed, in ticks.
const statMaxConverge = "longest convergence (ticks)"

// auditStats names the audit actions that are counted.
var auditStats = map[string]string{
	"lease.granted":      "grants",
	"lease.released":     "released leases",
	"lease.failed":       "failed leases",
	"lease.expired":      "expired leases",
	"rollout.completed":  "completed rollouts",
	"rollout.superseded": "superseded rollouts",
	"rollout.halted":     "halts",
	"action.abort":       "aborted rollouts",
}

// count reads the run's totals off the audit trail and the store.
func (s *sim) count() {
	for _, action := range s.store.auditActions() {
		if name, ok := auditStats[action]; ok {
			s.stats[name]++
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
			s.stats["forced grants"]++
		}

		if exempt {
			s.stats["budget-exempt grants"]++
		}

		if len(ids) > 1 {
			s.stats["bundled leases"]++
		}
	}

	s.stats["seeds"]++
	s.stats["hosts"] += len(s.world.agents)
	s.stats["slot-duration forks"] += len(s.world.phases) - 1
	s.stats["grants through the closed gate"] += s.oracle.exceptions
	s.stats["hosts seen working without a lease"] += s.oracle.working
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
		seed:      seed,
		rng:       rng,
		world:     newSimWorld(rng, start),
		store:     newFakeStore(),
		oracle:    newOracle(),
		log:       log,
		cancelled: make(map[string]bool, 2),
		checked:   make(map[string]int, 16),
		started:   make(map[string]bool, 64),
		stats:     make(simStats, 32),
	}

	pick := func(values ...float64) float64 { return values[rng.IntN(len(values))] }
	policy := func() fleet.Policy {
		if rng.IntN(6) == 0 {
			return fleet.PolicyManual
		}

		return fleet.PolicyAuto
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

	// From the seed, not from the random stream, so that the other seeds keep
	// their histories.
	s.defaults.MaxIndependentPercent = []float64{0, 10, 25, 100}[(seed/7)%4]

	// Both from the seed, not from the random stream, so that the other seeds
	// keep their histories. Every third seed pauses rollouts after their
	// canary, for one to three of the kinds.
	if seed%3 == 1 {
		kinds := 1 + (seed/3)%7
		s.defaults.PauseAfterCanaryImage = kinds&1 != 0
		s.defaults.PauseAfterCanaryPackages = kinds&2 != 0
		s.defaults.PauseAfterCanaryReboot = kinds&4 != 0
	}

	// Every other seed compares a host that comes back with the fleet.
	switch seed % 4 {
	case 0:
		s.defaults.ReadyPeerShare = 0.5
	case 2:
		s.defaults.ReadyPeerShare = 0.75
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
		Liveness: st.listLiveness,
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

// checkStarts looks at every step an agent began since the last look. An
// agent begins an operation only under a lease that is open at that moment,
// with two exceptions, in each of which the host works without a lease from
// its next sync on: the agent's clock is slow and the lease expired on it, or
// the store has never seen the lease (the database was lost). An operation
// that has begun is finished, whatever becomes of its lease, except that an
// agent begins no step once it has taken in that its lease was cancelled or
// is no longer offered.
func (s *sim) checkStarts() error {
	for _, a := range s.world.agents {
		begins := a.begins[s.checked[a.name]:]
		s.checked[a.name] = len(a.begins)

		for _, b := range begins {
			if told, ok := a.cancelSeen[b.lease]; ok && b.at.After(told) {
				return fmt.Errorf("%s began step %s of lease %s at %s, after it was told at %s that the lease is cancelled",
					a.name, b.need, b.lease, b.at.Format(time.TimeOnly), told.Format(time.TimeOnly))
			}

			s.stats["steps that agents began"]++

			if s.started[b.lease] {
				continue
			}

			s.started[b.lease] = true

			l, err := s.store.GetLease(context.Background(), b.lease)

			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return err
			case (a.overrun || len(a.late) > 0) && l.State == fleet.LeaseExpired && l.Host == a.name:
				// Its clock is slow: it starts a lease the controller has
				// expired.
			case l.State.Terminal() || l.Host != a.name:
				return fmt.Errorf("%s began the operation of lease %s with step %s, and the lease is %s (host %s)",
					a.name, l.ID, b.need, l.State, l.Host)
			}
			s.stats["steps that agents began"]++
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

		if s.store.recordSync(host) {
			s.stats["holds adopted from the agents"]++
			s.note("%s: registered on hold, as its agent reports", a.name)
		}

		a.lastSync = w.now
		a.lastSent = req

		// The agent's record of what it said about a lease ends with the
		// first report that its operation is over. (A lease that was adopted
		// is restated without needs; an agent that had forgotten the operation
		// answers with an empty one, which says nothing about the first.)
		if op := req.Operation; op != nil {
			if prev := a.reported[op.LeaseID]; prev == nil || !prev.Phase.Terminal() {
				a.reported[op.LeaseID] = cloneOperation(op)
			}
		}

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

// checkAssignment: a sync response carries only committed state. A lease
// that a person ended is restated with the cancel mark for as long as it is
// open, except while the agent has not reported an operation for it: then it
// is not offered at all.
func (s *sim) checkAssignment(a *simAgent, resp *Assignment) error {
	open, err := s.store.ListLeases(context.Background(), store.LeaseFilter{OpenOnly: true, Host: a.name})
	if err != nil {
		return err
	}

	if len(open) > 0 {
		cancelled := s.cancelled[open[0].ID]
		// Withheld from an agent that has not said that it has it.
		reported := a.lastSent != nil && a.lastSent.Operation != nil && a.lastSent.Operation.LeaseID == open[0].ID
		withheld := cancelled && open[0].State == fleet.LeaseGranted && !reported

		switch {
		case withheld && resp.Lease != nil:
			return fmt.Errorf("assignment of %s offers lease %s, which a person ended before the agent started it",
				a.name, resp.Lease.ID)
		case withheld && resp.Queue != nil:
			return fmt.Errorf("assignment of %s queues the host, which holds lease %s", a.name, open[0].ID)
		case withheld:
			return nil
		case resp.Lease != nil && resp.Lease.Cancel != cancelled:
			return fmt.Errorf("assignment of %s: lease %s cancel mark %v, a person ended it: %v",
				a.name, resp.Lease.ID, resp.Lease.Cancel, cancelled)
		}
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
			s.stats["write failures"]++

			return s.observe()
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

		s.stats["restart comparisons"]++
	}

	if err := s.observe(); err != nil {
		return err
	}

	return s.checkPlan()
}

// facts collects the world's state for the oracle.
func (s *sim) facts() *simFacts {
	w := s.world
	settings, _ := s.store.LoadSettings(context.Background(), s.defaults)
	health := w.health()
	epoch := w.epochOf(health.UpdatedAt)

	f := &simFacts{
		now:            w.now,
		since:          s.since,
		settings:       settings,
		health:         health,
		hosts:          make(map[string]*simHostFact, len(w.agents)),
		publishedEpoch: w.publishedEpoch,
		havePublished:  w.storedLiveness(),
		epoch:          w.epochStart(epoch + 1).Sub(w.epochStart(epoch)),
		retried:        s.retried,
	}

	for _, a := range w.agents {
		registered := s.store.host(a.name)
		if registered == nil {
			continue
		}

		live := w.publishedLive(a)

		f.hosts[a.name] = &simHostFact{
			weight:     a.weight,
			live:       live,
			holdWanted: registered.HoldWanted,
			offline:    registered.KnownOffline(w.now),
			lastSync:   a.lastSync,
			status:     a.lastSent,
			reported:   a.reported,
		}
	}

	return f
}

// observe hands the committed state to the oracle, after every reconcile
// and every action.
func (s *sim) observe() error {
	rollouts, leases, _ := s.store.view()
	err := s.oracle.observe(s.facts(), &simSnapshot{rollouts: rollouts, leases: leases})
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
	independent := 0

	for _, l := range open {
		leased[l.Host] = true

		if l.Impact == protocol.ImpactIndependent {
			independent++
		}
	}

	// A host that works without a lease takes a slot like one that holds one;
	// an independent lease takes a place in its own limit.
	f := s.facts()
	slots := len(leased) - independent

	for name, h := range f.hosts {
		if !leased[name] && f.working(h) {
			slots++
		}
	}

	if plan.Budget.LeasedHosts != slots || plan.Budget.IndependentHosts != independent {
		return fmt.Errorf("plan counts %d leased and %d independent hosts, %d leases are open "+
			"(%d independent) and %d hosts work without one",
			plan.Budget.LeasedHosts, plan.Budget.IndependentHosts, len(leased), independent,
			slots+independent-len(leased))
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

		word, _, _ := strings.Cut(q.Reason, " ")
		s.stats["waits: "+word]++
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

	return s.observe()
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
		s.stats["restarts"]++

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

	s.stats["outages longer than leaseTtl"]++
	s.stats["open leases carried over an outage"] += len(carried)

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
// operation keep reporting it.
func (s *sim) loseDatabase() error {
	s.note("database lost")
	s.stats["lost databases"]++
	s.stats["grants through the closed gate"] += s.oracle.exceptions
	s.stats["hosts seen working without a lease"] += s.oracle.working

	s.store = newFakeStore()
	s.oracle = newOracle()
	s.retried = nil
	s.cancelled = make(map[string]bool, 2)

	for _, a := range s.world.agents {
		s.world.dropped[a.name] = true
	}

	return s.boot(s.store)
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
	case n < 47:
		hosts := s.someHosts()
		res, err := s.svc.Hold(ctx, simActor, HoldRequest{Hosts: hosts, Reason: "sim"})

		return s.act(fmt.Sprintf("hold %v", hosts), res, err)
	case n < 50:
		// A host known offline for a while, or until a person lifts it.
		hosts := s.someHosts()[:1]
		req := OfflineRequest{Hosts: hosts, Reason: "sim"}

		if s.rng.IntN(2) == 0 {
			until := s.world.now.Add(time.Duration(1+s.rng.IntN(30)) * time.Minute)
			req.Until = &until
		}

		res, err := s.svc.MarkOffline(ctx, simActor, req)

		return s.act(fmt.Sprintf("mark offline %v", hosts), res, err)
	case n < 57:
		return s.unhold(s.someHosts())
	case n < 60:
		return s.unmarkOffline(s.someHosts())
	case n < 80:
		return s.manualAction()
	case n < 94:
		return s.settingsAction()
	case n < 97:
		return s.endLease()
	default:
		hosts := s.someHosts()[:1]
		res, err := s.svc.Forget(ctx, simActor, HoldRequest{Hosts: hosts})

		// The stored liveness rows of a forgotten host are gone.
		if s.store.host(hosts[0]) == nil {
			s.world.dropped[hosts[0]] = true
		}

		return s.act(fmt.Sprintf("forget %v", hosts), res, err)
	}
}

// unhold is the person who lifts the hold of hosts, at any time.
func (s *sim) unhold(hosts []string) error {
	res, err := s.svc.Unhold(context.Background(), simActor, HoldRequest{Hosts: hosts})

	return s.act(fmt.Sprintf("unhold %v", hosts), res, err)
}

// unmarkOffline is the person who lifts the known-offline mark of hosts.
func (s *sim) unmarkOffline(hosts []string) error {
	res, err := s.svc.UnmarkOffline(context.Background(), simActor, OfflineRequest{Hosts: hosts})

	return s.act(fmt.Sprintf("unmark offline %v", hosts), res, err)
}

// endLease is the person who ends an open lease: any one, whatever it does,
// started or not, ended before or not. Every open lease can be ended, and
// ending it does not move it: only its agent's report, its expiry or a
// reconcile ends a lease.
func (s *sim) endLease() error {
	ctx := context.Background()

	open, err := s.store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return err
	}

	if len(open) == 0 {
		res, endErr := s.svc.EndLease(ctx, simActor, EndLeaseRequest{Lease: "nosuchlease", Reason: "sim"})

		return s.act("end lease nosuchlease", res, endErr)
	}

	was := open[s.rng.IntN(len(open))]
	res, err := s.svc.EndLease(ctx, simActor, EndLeaseRequest{Lease: was.ID, Reason: "sim"})

	switch {
	case errors.Is(err, errWrite):
	case err != nil:
		return fmt.Errorf("end lease %s of %s (%s) was refused: %w", was.ID, was.Host, was.State, err)
	default:
		s.cancelled[was.ID] = true
		s.stats["leases ended by a person: "+string(was.State)]++

		now, getErr := s.store.GetLease(ctx, was.ID)
		if getErr != nil {
			return getErr
		}

		if now.State != was.State {
			return fmt.Errorf("ending lease %s of %s moved it from %s to %s", was.ID, was.Host, was.State, now.State)
		}
	}

	return s.act(fmt.Sprintf("end lease %s of %s (%s)", was.ID, was.Host, was.State), res, err)
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
		return s.resume(r.ID)
	case 4:
		res, err := s.svc.Abort(ctx, simActor, ScopeRequest{Rollout: r.ID, Reason: "sim"})

		return s.act("abort "+r.ID, res, err)
	default:
		hosts := s.someHosts()
		res, err := s.svc.Skip(ctx, simActor, RolloutHostsRequest{Rollout: r.ID, Hosts: hosts})
		if err == nil {
			s.stats["skipped hosts"] += len(res.Hosts)
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

	policies := []fleet.Policy{fleet.PolicyAuto, fleet.PolicyAuto, fleet.PolicyManual}
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

	err = replaceSettings(ctx, s.svc, simActor, set)

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

	if err := replaceSettings(ctx, s.svc, simActor, set); err != nil {
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

		s.stats["convergence ticks"]++
		s.stats[statMaxConverge]++

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

// operate is the person of the convergence phase: lift holds and pauses
// (those of an abort among them), retry what halted and take back the skips.
func (s *sim) operate() error {
	rollouts, _, _ := s.store.view()

	for _, a := range s.world.agents {
		if h := s.store.host(a.name); h != nil && h.HoldWanted {
			if err := s.unhold([]string{a.name}); err != nil {
				return err
			}
		}

		if h := s.store.host(a.name); h != nil && h.Offline != nil {
			if err := s.unmarkOffline([]string{a.name}); err != nil {
				return err
			}
		}
	}

	for _, r := range rollouts {
		if r.State.Terminal() {
			continue
		}

		if r.Paused {
			if err := s.resume(r.ID); err != nil {
				return err
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

	return nil
}

// resume is the person who resumes a rollout.
func (s *sim) resume(id string) error {
	res, err := s.svc.Resume(context.Background(), simActor, ScopeRequest{Rollout: id})

	return s.act("resume "+id, res, err)
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
// ones.
//
// A retry of the rollout lifts its halt. A retry of hosts lifts it only when
// one of the hosts it retried had failed and no member is left failed; the
// simulation does not take the scheduler's word for that: it looks at the
// members itself, and tells the oracle that the rollout was retried only when
// none is failed. A rollout that left its halt with a failed member in it
// then fails the oracle's halt rule.
func (s *sim) retry(r *fleet.Rollout, hosts []string) error {
	res, err := s.svc.Retry(context.Background(), simActor, RolloutHostsRequest{Rollout: r.ID, Hosts: hosts})
	if err == nil {
		after, getErr := s.store.GetRollout(context.Background(), r.ID)
		stillFailed := getErr == nil && slices.ContainsFunc(after.Hosts, func(m fleet.RolloutHost) bool {
			return m.State == fleet.HostFailed
		})

		// What was retried is read off the rollout as it was before.
		wasFailed := slices.ContainsFunc(res.Hosts, func(name string) bool {
			m := r.Host(name)

			return m != nil && m.State == fleet.HostFailed
		})

		if len(hosts) == 0 || (wasFailed && !stillFailed) {
			s.retried = append(s.retried, r.ID)
		}
	}

	return s.act(fmt.Sprintf("retry %s %v", r.ID, hosts), res, err)
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
func runSim(seed uint64, steps int) (string, simStats) {
	s, err := newSim(seed)
	if err != nil {
		if s == nil {
			return fmt.Sprintf("seed %d: %v", seed, err), simStats{}
		}

		return s.report(err), s.stats
	}

	for s.step = 1; s.step <= steps; s.step++ {
		if err := s.randomStep(); err != nil {
			return s.report(err), s.stats
		}
	}

	if err := s.converge(); err != nil {
		return s.report(err), s.stats
	}

	s.count()

	return "", s.stats
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
// the oracle's invariants after every step, then convergence. A
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
		total    = make(simStats, 64)
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
		for _, what := range []string{
			"grants", "released leases", "failed leases", "expired leases", "completed rollouts",
			"superseded rollouts", "halts", "forced grants", "budget-exempt grants", "bundled leases",
			"restart comparisons", "write failures", "lost databases", "holds adopted from the agents",
			"outages longer than leaseTtl", "open leases carried over an outage",
			"skipped hosts", "grants through the closed gate", "steps that agents began",
			"hosts seen working without a lease",
			"slot-duration forks", "leases ended by a person: granted", "leases ended by a person: running",
			"leases ended by a person: verifying", "waits: verb", "waits: budget", "waits: host",
			"waits: network", "waits: after",
		} {
			if total[what] == 0 {
				t.Errorf("the simulation never produced any %s", what)
			}
		}
	}
}
