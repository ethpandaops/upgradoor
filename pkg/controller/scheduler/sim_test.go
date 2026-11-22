package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
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
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
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
// After every reconcile and every action the oracle (further down)
// holds the committed state against its invariants; now and then the same
// reconcile is run on a freshly started scheduler, which must decide the
// same; at the end of a run the world is healed and everything must
// converge.
//
// Reproduce a failure with UPGRADOOR_SIM_SEED=<seed>; run more seeds with
// UPGRADOOR_SIM_SEEDS=<n>.
//
// The simulation is part of package scheduler, not a package of its own: the
// oracle checks the committed state with the scheduler's unexported helpers
// (cloneLease, needGroup, budgetEpsilon, the gate reasons) and the shared
// fake store and world builder of helpers_test.go.

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
	`verb not allowed by the agent|waiting to pull its image again after a failed pull|network gate: .+|after .+|` +
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

		if r.Kind == fleet.RolloutGroup && r.ContainerKind == "" && r.State == fleet.RolloutCompleted {
			s.stats["completed unlabelled rollouts"]++
		}
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
		LookupInterval:          fleet.Duration(5 * time.Minute),
	}

	// From the seed, not from the random stream, so that the other seeds keep
	// their histories.
	s.defaults.MaxIndependentPercent = []float64{0, 10, 25, 100}[(seed/7)%4]
	s.defaults.MaxLeasedPercent = []float64{10, 25, 50, 100}[(seed/11)%4]

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
		withheld := (cancelled && open[0].State == fleet.LeaseGranted && !reported) || open[0].Recheck

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
			unproven:   registered.Unproven,
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
	f := s.facts()
	err := s.oracle.observe(f, &simSnapshot{rollouts: rollouts, leases: leases})
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
	byLease := make(map[string]protocol.Impact, len(open))

	for _, l := range open {
		leased[l.Host] = true
		byLease[l.Host] = l.Impact

		if l.Impact == protocol.ImpactIndependent {
			independent++
		}
	}

	// A host that works without a lease takes a slot like one that holds one,
	// and so does a charged host, whatever lighter lease it holds; an
	// independent lease takes a place in its own limit.
	f := s.facts()
	slots := len(leased) - independent

	for name, h := range f.hosts {
		switch {
		case leased[name] && byLease[name] != protocol.ImpactIndependent:
		case f.charged(h), !leased[name] && f.working(h):
			slots++
		}
	}

	if plan.Budget.LeasedHosts != slots || plan.Budget.IndependentHosts != independent {
		return fmt.Errorf("plan counts %d leased and %d independent hosts, %d leases are open "+
			"(%d independent) and %d hosts work without one or are charged",
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

	// An aborted target is not rolled again until it changes: every tag
	// and the packages are released once more.
	for _, image := range w.images {
		w.registry[image] = w.newDigest(digestGood)
	}

	w.pkgVersion++

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

	// A retried host is done only through a readiness check (a recheck), so
	// the check must be able to pass.
	set.ReadinessTimeout = max(set.ReadinessTimeout, fleet.Duration(time.Duration(set.ReadyEpochs+3)*simEpoch))

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
			"waits: network", "waits: after", "completed unlabelled rollouts",
		} {
			if total[what] == 0 {
				t.Errorf("the simulation never produced any %s", what)
			}
		}
	}
}

// The simulated chain runs short epochs so that a run of a few hundred steps
// covers dozens of them.
const (
	simSecondsPerSlot = 6
	simSlotsPerEpoch  = 8
	simEpoch          = simSecondsPerSlot * simSlotsPerEpoch * time.Second
)

// Kinds of digests in the simulated registry.
const (
	digestGood = iota
	// digestFails: the container does not start; the agent reports failed.
	digestFails
	// digestMute: the container starts and the agent reports done, but the
	// validators never attest again.
	digestMute
)

const simPackage = "docker-ce"

// simLatencySeconds bounds how far an agent's start deadline lies after the
// lease's expiry. It must stay below leaseStartGrace.
const simLatencySeconds = 20

// simContainer is one container of a simulated host.
type simContainer struct {
	name    string
	image   string
	managed bool
	custom  bool
	// kind is the kind label of a managed container (empty: unlabelled): a
	// kind may hold containers of several images.
	kind protocol.ContainerKind
	// impact is the class of a managed container. Only work on node
	// containers takes the node down.
	impact protocol.Impact
	// running is the running digest, seen what the agent's last registry
	// poll saw.
	running   string
	seen      string
	checkedAt time.Time
}

// simManual is a need created by a roll or reboot directive.
type simManual struct {
	directive string
	kind      protocol.NeedKind
	container string
}

// simAgent is one simulated host with its agent. It speaks the protocol the
// way pkg/protocol describes it.
type simAgent struct {
	name   string
	val    []identity.ValidatorRange
	weight uint64
	probes bool
	// peerMax is how many peers the host's clients have once they found them
	// all again. It follows from the host's number, so that no seed draws a
	// different history for it.
	peerMax int
	verbs   []protocol.Verb

	containers []*simContainer
	// pkgInstalled and pkgSeen are versions of the blocked package.
	pkgInstalled   int
	pkgSeen        int
	rebootRequired bool
	// rebootSticks: the host comes back from a reboot still requiring one
	// (its boot configuration picks the old kernel).
	rebootSticks bool
	boot         int

	localHold bool
	ctrlHold  bool
	manual    []simManual
	// handled remembers the answer to every directive ever applied; acks are
	// the answers to the directives of the last response.
	handled map[string]protocol.DirectiveAck
	acks    []protocol.DirectiveAck

	nextPoll    time.Time
	silentUntil time.Time
	outageUntil time.Time

	// lease is the persisted lease; deadline and startAt say until when and
	// when the agent starts it.
	lease    *protocol.Lease
	deadline time.Time
	startAt  time.Time
	// op is the operation on lease; it ends at opEnd with opFail deciding
	// the outcome. down is true while the operation has things stopped.
	op     *protocol.Operation
	opEnd  time.Time
	opFail bool
	reboot bool
	down   bool
	// upAt is when the last operation ended: the node finds its peers again
	// from then on.
	upAt time.Time

	// What the node goes through besides being up or down. syncingUntil: a
	// client reports syncing (the execution client when syncingEL, else the
	// consensus client). elOfflineUntil: the consensus client reports its
	// execution client offline. elDownUntil: the execution client does not
	// answer. peerCapUntil: the node has at most peerCap peers. elLag: the
	// execution client finds its peers that much later than the consensus
	// client.
	// dead: the host's validators never attest, while its node and its agent
	// are fine (a broken validator client, missing keys).
	dead bool
	// sloppy: the agent reports its needs also while it is held and for verbs
	// its configuration does not allow. It refuses the steps all the same.
	sloppy bool
	// blind: the agent's registry polls see nothing new, so it never reports
	// the need the controller expects of it.
	blind bool
	// overrun: the agent's clock is slow, so that it may start its lease
	// after the controller has expired it.
	overrun bool
	// stepFail is the index of the step of the running operation that fails
	// while the others succeed; -1 for none.
	stepFail int
	// beginAt is, per step of the running operation, when the step begins;
	// the zero time for a step that is not executed. The steps of an operation
	// begin one after the other.
	beginAt []time.Time
	// cancelled: the agent has taken in that its lease was cancelled. It
	// begins no further step.
	cancelled bool

	syncingUntil   time.Time
	syncingEL      bool
	elOfflineUntil time.Time
	elDownUntil    time.Time
	peerCapUntil   time.Time
	peerCap        int
	elLag          time.Duration

	// late names the leases the agent started with a slow clock: possibly
	// after the controller had expired them.
	late map[string]bool
	// begins is every step the agent ever began, in order; cancelSeen is, per
	// lease, when the agent took in that the lease was cancelled or withdrawn.
	// An old build takes in neither.
	begins     []simBegin
	cancelSeen map[string]time.Time

	lastSync time.Time
	lastSent *protocol.SyncRequest
	// reported is, per lease ID, the operation the agent sent to the
	// controller for the lease, up to the first report that it is over: its
	// own record of what it said.
	reported map[string]*protocol.Operation
}

// simBegin is one step that an agent began: the first disruptive thing it
// did for the need.
type simBegin struct {
	lease, need string
	at          time.Time
}

func (a *simAgent) allows(v protocol.Verb) bool { return slices.Contains(a.verbs, v) }

func (a *simAgent) held() bool { return a.localHold || a.ctrlHold }

func (a *simAgent) container(name string) *simContainer {
	for _, c := range a.containers {
		if c.name == name {
			return c
		}
	}

	return nil
}

// simWorld is the simulated fleet, registry and network. Nothing in it comes
// from the scheduler.
type simWorld struct {
	rng *rand.Rand
	now time.Time
	// start is when the world began.
	start  time.Time
	agents []*simAgent
	byName map[string]*simAgent

	images     []string
	registry   map[string]string
	digestKind map[string]int
	digests    int
	pkgVersion int

	// calm switches the random failures off (convergence).
	calm               bool
	beaconDownUntil    time.Time
	finalityStallUntil time.Time
	finalityStallSince time.Time
	// blackoutUntil: no validator of any host is seen attesting.
	blackoutUntil        time.Time
	livenessStallUntil   time.Time
	livenessUnknownUntil time.Time
	dropUntil            time.Time
	dropPercent          uint64
	healthAt             time.Time

	// phases are the chain's slot durations: the first from genesis on, one
	// more for every fork that changes the duration.
	phases []simPhase

	// epoch is the epoch of now; epochMin the fewest validators each host
	// attested with at any moment of it.
	epoch    uint64
	epochMin map[string]uint64
	// published holds the liveness rows the beacon service stored, per
	// epoch; dropped names hosts whose stored rows were deleted since.
	published      map[uint64]map[string]uint64
	publishedEpoch uint64
	havePublished  bool
	dropped        map[string]bool
}

// simImpacts are the classes a managed container is drawn from: mostly the
// node's own containers.
var simImpacts = []protocol.Impact{
	protocol.ImpactNode, protocol.ImpactNode, protocol.ImpactNode, protocol.ImpactNode, protocol.ImpactIndependent,
}

// simLeaseImpact is the world's own class of the work of a lease: node when
// any need is about the node or a node container.
func simLeaseImpact(a *simAgent, needs []protocol.Need) protocol.Impact {
	impact := protocol.ImpactIndependent

	for i := range needs {
		c := a.container(needs[i].Container())
		if c == nil || !c.managed {
			return protocol.ImpactNode
		}

		impact = impact.Heavier(c.impact)
	}

	return impact
}

func simDigest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// simStart is when a simulated world begins, unless its chain has not begun
// yet (newSim).
var simStart = testGenesis.Add(50*simEpoch + 7*time.Second)

// newSimWorld builds a random fleet at a time, which may lie before the
// chain's genesis.
func newSimWorld(rng *rand.Rand, start time.Time) *simWorld {
	w := &simWorld{
		rng:        rng,
		now:        start,
		start:      start,
		byName:     make(map[string]*simAgent, 32),
		registry:   make(map[string]string, 4),
		digestKind: make(map[string]int, 16),
		epochMin:   make(map[string]uint64, 32),
		published:  make(map[uint64]map[string]uint64, 64),
		dropped:    make(map[string]bool, 2),
		pkgVersion: 1,
		phases:     []simPhase{{start: testGenesis, slot: simSecondsPerSlot * time.Second}},
	}
	w.healthAt = w.now
	w.epoch = w.epochOf(w.now)

	for i := range 2 + rng.IntN(3) {
		image := fmt.Sprintf("org/client%d:devnet", i)
		w.images = append(w.images, image)
		w.registry[image] = w.newDigest(digestGood)
	}

	hosts := 3 + rng.IntN(10)
	if rng.IntN(3) == 0 {
		hosts = 3 + rng.IntN(28)
	}

	next := uint64(0)

	for i := range hosts {
		a := &simAgent{
			name:         fmt.Sprintf("host-%02d", i),
			probes:       rng.IntN(10) < 7,
			peerMax:      simPeers + i%simPeerSpread,
			verbs:        slices.Clone(allVerbs),
			pkgInstalled: 1,
			pkgSeen:      1,
			handled:      make(map[string]protocol.DirectiveAck, 8),
			late:         make(map[string]bool, 2),
			reported:     make(map[string]*protocol.Operation, 8),
			cancelSeen:   make(map[string]time.Time, 2),
			localHold:    rng.IntN(25) == 0,
			dead:         rng.IntN(20) == 0,
			sloppy:       rng.IntN(5) == 0,
			blind:        rng.IntN(12) == 0,
			stepFail:     -1,
		}

		if rng.IntN(7) == 0 {
			a.verbs = slices.DeleteFunc(a.verbs, func(protocol.Verb) bool { return rng.IntN(2) == 0 })
		}

		if rng.IntN(10) < 7 {
			a.weight = uint64(1 + rng.IntN(60))
			if rng.IntN(8) == 0 {
				a.weight = uint64(100 + rng.IntN(400))
			}

			a.val = []identity.ValidatorRange{{Start: next, End: next + a.weight}}
			next += a.weight
		}

		for c := range 1 + rng.IntN(3) {
			image := w.images[rng.IntN(len(w.images))]
			a.containers = append(a.containers, &simContainer{
				name:    fmt.Sprintf("c%d", c),
				image:   image,
				managed: true,
				kind:    protocol.ContainerKind(fmt.Sprintf("g%d", c%2)),
				custom:  rng.IntN(12) == 0,
				// From the host and the container, not from the random
				// stream, so that the seeds keep their histories.
				impact:    simImpacts[(i+2*c)%len(simImpacts)],
				running:   w.registry[image],
				seen:      w.registry[image],
				checkedAt: w.now,
			})
		}

		// From the host, not from the random stream, so that the seeds keep
		// their histories: every third host runs an unlabelled container on
		// one of the devnet's images, which moves in a rollout of that image
		// of its own, next to the rollouts of the kinds.
		if i%3 == 0 {
			image := w.images[i%len(w.images)]
			a.containers = append(a.containers, &simContainer{
				name: "tools", image: image, managed: true,
				impact: protocol.ImpactIndependent, running: w.registry[image],
				seen: w.registry[image], checkedAt: w.now,
			})
		}

		if rng.IntN(4) == 0 {
			a.containers = append(a.containers, &simContainer{
				name: "sidecar", image: "org/sidecar:latest", running: simDigest(1 << 20),
			})
		}

		a.nextPoll = w.now.Add(time.Duration(rng.IntN(60)) * time.Second)
		w.agents = append(w.agents, a)
		w.byName[a.name] = a
		w.epochMin[a.name] = w.attesting(a)
	}

	return w
}

func (w *simWorld) newDigest(kind int) string {
	w.digests++
	d := simDigest(w.digests)
	w.digestKind[d] = kind

	return d
}

// simPhase is a stretch of the simulated chain with one slot duration.
type simPhase struct {
	epoch uint64
	start time.Time
	slot  time.Duration
}

// phaseOf returns the phase an epoch is in.
func (w *simWorld) phaseOf(e uint64) simPhase {
	phase := w.phases[0]

	for _, next := range w.phases[1:] {
		if next.epoch <= e {
			phase = next
		}
	}

	return phase
}

// epochOf is the epoch a time is in. This is the world's own clock: it does
// not share code with the scheduler's. Before genesis no epoch has begun:
// the time counts to the first one, which is not complete before it has run.
func (w *simWorld) epochOf(t time.Time) uint64 {
	phase := w.phases[0]
	if t.Before(phase.start) {
		return phase.epoch
	}

	for _, next := range w.phases[1:] {
		if !t.Before(next.start) {
			phase = next
		}
	}

	return phase.epoch + uint64(t.Sub(phase.start)/(simSlotsPerEpoch*phase.slot))
}

// epochStart is when an epoch begins.
func (w *simWorld) epochStart(e uint64) time.Time {
	phase := w.phaseOf(e)

	return phase.start.Add(time.Duration(e-phase.epoch) * simSlotsPerEpoch * phase.slot)
}

// fork schedules a change of the slot duration at an epoch that has not
// begun. The chain has at most two of them.
func (w *simWorld) fork(at uint64, slot time.Duration) bool {
	last := w.phases[len(w.phases)-1]
	if len(w.phases) > 2 || at <= w.epochOf(w.now) || at <= last.epoch || slot == last.slot {
		return false
	}

	w.phases = append(w.phases, simPhase{epoch: at, start: w.epochStart(at), slot: slot})

	return true
}

// describe renders the fleet for a failure report.
func (w *simWorld) describe() string {
	var b strings.Builder

	for _, a := range w.agents {
		fmt.Fprintf(&b, "  %s weight=%d probes=%v peers=%d verbs=%v localHold=%v dead=%v sloppy=%v blind=%v:",
			a.name, a.weight, a.probes, a.peerMax, a.verbs, a.localHold, a.dead, a.sloppy, a.blind)

		for _, c := range a.containers {
			fmt.Fprintf(&b, " %s=%s(managed=%v custom=%v)", c.name, c.image, c.managed, c.custom)
		}

		b.WriteByte('\n')
	}

	return b.String()
}

// attesting is how many of the host's validators attest right now. A node
// that is syncing, has lost its execution client or has no peer attests with
// none.
func (w *simWorld) attesting(a *simAgent) uint64 {
	if a.weight == 0 || a.dead || w.now.Before(w.blackoutUntil) || !w.synced(a) || w.behind(a) ||
		w.clPeers(a) == 0 {
		return 0
	}

	if w.now.Before(w.dropUntil) {
		return a.weight - a.weight*w.dropPercent/100
	}

	return a.weight
}

// simPeers is how many peers a node has at least once it found them all
// again, and simPeerSpread how many values that number takes over the hosts
// (12 to 16: the fewest are three quarters of the most, so that a host at its
// full number is never below the largest readyPeerShare of a run);
// simPeerEvery is how long each of them takes after the node came back.
const (
	simPeers      = 12
	simPeerSpread = 5
	simPeerEvery  = 3 * time.Second
)

// clPeers is how many peers the consensus client has: none while the node is
// down, then one more every simPeerEvery since it came back from its last
// operation or outage, and no more than peerCap while the node has trouble
// keeping them.
func (w *simWorld) clPeers(a *simAgent) int {
	return w.peersAfter(a, 0)
}

// elPeers is the same for the execution client, which finds its peers elLag
// later and has none while it does not answer.
func (w *simWorld) elPeers(a *simAgent) int {
	if w.now.Before(a.elDownUntil) {
		return 0
	}

	return w.peersAfter(a, a.elLag)
}

func (w *simWorld) peersAfter(a *simAgent, lag time.Duration) int {
	if !w.synced(a) {
		return 0
	}

	peers := int(min(time.Duration(a.peerMax), max(0, w.now.Sub(later(a.upAt, a.outageUntil))-lag)/simPeerEvery))
	if w.now.Before(a.peerCapUntil) {
		peers = min(peers, a.peerCap)
	}

	return peers
}

// synced reports whether the host's node is up: no operation has it stopped,
// it is in no outage and its containers run.
func (w *simWorld) synced(a *simAgent) bool {
	if a.down || w.now.Before(a.outageUntil) {
		return false
	}

	for _, c := range a.containers {
		if c.managed && !c.custom && c.impact == protocol.ImpactNode && w.digestKind[c.running] != digestGood {
			return false
		}
	}

	return true
}

// behind reports whether a node that is up is not at head: a client is
// syncing, or the consensus client has lost its execution client.
func (w *simWorld) behind(a *simAgent) bool {
	return w.now.Before(a.syncingUntil) || w.now.Before(a.elOfflineUntil) || w.now.Before(a.elDownUntil)
}

// node is what the host's probes say.
func (w *simWorld) node(a *simAgent) protocol.NodeHealth {
	var (
		up      = w.synced(a)
		syncing = up && w.now.Before(a.syncingUntil)
		elUp    = up && !w.now.Before(a.elDownUntil)
	)

	return protocol.NodeHealth{
		EL: &protocol.ELHealth{Reachable: elUp, Syncing: syncing && a.syncingEL && elUp, Peers: w.elPeers(a)},
		CL: &protocol.CLHealth{
			Reachable: up,
			Syncing:   syncing && !a.syncingEL,
			ELOffline: up && (!elUp || w.now.Before(a.elOfflineUntil)),
			Peers:     w.clPeers(a),
		},
	}
}

// comeBack decides what the node goes through after an operation: now and
// then it has to catch up first, its execution client takes longer than its
// consensus client, or it keeps losing its peers.
func (w *simWorld) comeBack(a *simAgent) {
	a.upAt = w.now
	a.elLag = 0

	if w.calm {
		return
	}

	if w.rng.IntN(3) == 0 {
		a.syncingUntil = w.now.Add(time.Duration(2+w.rng.IntN(40)) * time.Second)
		a.syncingEL = w.rng.IntN(2) == 0
	}

	if w.rng.IntN(5) == 0 {
		a.elOfflineUntil = w.now.Add(time.Duration(2+w.rng.IntN(30)) * time.Second)
	}

	if w.rng.IntN(3) == 0 {
		a.elLag = time.Duration(3+w.rng.IntN(40)) * time.Second
	}
}

// advance moves the clock, publishes the liveness of the epochs that ended
// and lets every agent work.
func (w *simWorld) advance(d time.Duration, st *fakeStore) {
	w.now = w.now.Add(d)

	for now := w.epochOf(w.now); w.epoch < now; w.epoch++ {
		w.publish(w.epoch, st)

		// Nothing changed since the last advance, so the next epoch starts
		// in the state the hosts are in.
		for _, a := range w.agents {
			w.epochMin[a.name] = w.attesting(a)
		}
	}

	for _, a := range w.agents {
		w.work(a)
	}

	w.sample()

	if !w.now.Before(w.beaconDownUntil) {
		w.healthAt = w.now
	}
}

// sample records the hosts' attesting validators for the running epoch.
func (w *simWorld) sample() {
	for _, a := range w.agents {
		w.epochMin[a.name] = min(w.epochMin[a.name], w.attesting(a))
	}
}

// publish stores the liveness rows of a complete epoch, unless the beacon
// service cannot get them.
func (w *simWorld) publish(epoch uint64, st *fakeStore) {
	end := w.epochStart(epoch + 1)

	if end.Before(w.beaconDownUntil) || end.Before(w.livenessStallUntil) || end.Before(w.livenessUnknownUntil) {
		return
	}

	rows := make(map[string]uint64, len(w.agents))
	stored := make([]fleet.Liveness, 0, len(w.agents))

	for _, a := range w.agents {
		// The beacon service tracks the validators of the hosts it knows.
		if a.weight == 0 || st.host(a.name) == nil {
			continue
		}

		rows[a.name] = w.epochMin[a.name]
		stored = append(stored, fleet.Liveness{
			Host: a.name, Epoch: epoch, Live: w.epochMin[a.name], Total: a.weight,
		})

		delete(w.dropped, a.name)
	}

	if len(stored) == 0 {
		return
	}

	w.published[epoch] = rows
	w.publishedEpoch = epoch
	w.havePublished = true

	st.putLiveness(stored...)
}

// publishedLive is the host's attesting validators in the newest published
// epoch as the store holds it; the full weight when there is no row.
func (w *simWorld) publishedLive(a *simAgent) uint64 {
	if !w.havePublished || w.dropped[a.name] {
		return a.weight
	}

	live, ok := w.published[w.publishedEpoch][a.name]
	if !ok {
		return a.weight
	}

	return min(live, a.weight)
}

// storedLiveness reports whether the store still holds a row of the newest
// published epoch: the rows of a forgotten host and of a lost database are
// gone until the next epoch is published.
func (w *simWorld) storedLiveness() bool {
	if !w.havePublished {
		return false
	}

	for name := range w.published[w.publishedEpoch] {
		if !w.dropped[name] {
			return true
		}
	}

	return false
}

// health is the beacon service's view.
func (w *simWorld) health() fleet.NetworkHealth {
	lag := uint64(2)
	if w.now.Before(w.finalityStallUntil) {
		lag += uint64(w.now.Sub(w.finalityStallSince) / simEpoch)
	}

	epoch := w.epochOf(w.healthAt)
	phase := w.phaseOf(epoch)

	// A chain that has just begun (or has not yet) has finalized nothing, and
	// lags by no more than it is old.
	lag = min(lag, epoch)

	slot := uint64(0)
	if since := w.healthAt.Sub(w.epochStart(epoch)); since > 0 {
		slot = uint64(since / phase.slot)
	}
	phases := make([]fleet.SlotPhase, 0, len(w.phases))

	// The beacon service publishes the whole schedule, forks to come too.
	for _, p := range w.phases {
		phases = append(phases, fleet.SlotPhase{Epoch: p.epoch, Start: p.start, SlotMs: uint64(p.slot / time.Millisecond)})
	}

	n := fleet.NetworkHealth{
		UpdatedAt:         w.healthAt,
		GenesisTime:       testGenesis,
		SecondsPerSlot:    uint64(phase.slot / time.Second),
		Phases:            phases,
		SlotsPerEpoch:     simSlotsPerEpoch,
		HeadSlot:          epoch*simSlotsPerEpoch + slot,
		CurrentEpoch:      epoch,
		FinalizedEpoch:    epoch - lag,
		FinalityLagEpochs: lag,
		LivenessKnown:     w.havePublished && !w.now.Before(w.livenessUnknownUntil),
		LivenessEpoch:     w.publishedEpoch,
	}

	if w.now.Before(w.beaconDownUntil) {
		n.Error = "connection refused"
	}

	for _, a := range w.agents {
		if live, ok := w.published[w.publishedEpoch][a.name]; ok && w.havePublished {
			n.Tracked += a.weight
			n.Live += live
		}
	}

	return n
}

// work lets one agent act at the current time: poll its registries, start
// the lease it holds, move its operation on.
func (w *simWorld) work(a *simAgent) {
	if !w.now.Before(a.nextPoll) {
		w.poll(a)
	}

	if a.lease != nil && a.op == nil {
		switch {
		case !w.now.Before(a.deadline):
			// An agent that has not started when the lease expires must not
			// start.
			a.lease = nil
		case !w.now.Before(a.startAt):
			w.startOperation(a)
		}
	}

	if a.op == nil || a.op.Phase.Terminal() {
		return
	}

	w.beginSteps(a)

	if !w.now.Before(a.opEnd) {
		w.finishOperation(a)

		return
	}

	left := a.opEnd.Sub(w.now)
	total := a.opEnd.Sub(a.op.StartedAt)
	phase := protocol.PhaseVerifying

	switch {
	case left*10 > total*7 && a.reboot:
		phase = protocol.PhaseDraining
	case left*10 > total*5:
		phase = protocol.PhaseApplying
	case left*10 > total*4 && a.reboot:
		phase = protocol.PhaseRebooting
	case left*10 > total*2:
		phase = protocol.PhaseStarting
	}

	if a.op.Phase != phase {
		a.op.Phase = phase
		a.op.UpdatedAt = w.now
	}
}

// poll looks at the registries and the package candidates.
func (w *simWorld) poll(a *simAgent) {
	for _, c := range a.containers {
		if c.managed && !c.custom && !a.blind {
			c.seen = w.registry[c.image]
			c.checkedAt = w.now
		}
	}

	a.pkgSeen = w.pkgVersion
	a.nextPoll = w.now.Add(time.Duration(15+w.rng.IntN(90)) * time.Second)
}

// stepFor validates one granted need locally, as the agent must.
func (w *simWorld) stepFor(a *simAgent, n *protocol.Need) protocol.StepStatus {
	if a.held() || !a.allows(n.Kind.Verb()) {
		return protocol.StepRefused
	}

	switch n.Kind {
	case protocol.NeedImage:
		c := a.container(n.Image.Container)

		switch {
		case c == nil || !c.managed || c.custom:
			return protocol.StepRefused
		case c.image != n.Image.Image || w.registry[c.image] != n.Image.ToDigest:
			// The digest the tag points at must equal the lease's.
			return protocol.StepSuperseded
		case n.DirectiveID == "" && c.running == n.Image.ToDigest:
			return protocol.StepSuperseded
		}
	case protocol.NeedPackages:
		want := simPackages(a.pkgInstalled, w.pkgVersion)
		if a.pkgInstalled >= w.pkgVersion || protocol.PackagesNeedID(want) != n.ID {
			return protocol.StepSuperseded
		}
	case protocol.NeedReboot, protocol.NeedOverride:
		if n.DirectiveID == "" && !a.rebootRequired {
			return protocol.StepSuperseded
		}
	case protocol.NeedSpec, protocol.NeedAction:
		// The simulated agents have no agent.yaml that changes, and no actions.
		return protocol.StepRefused
	}

	if n.DirectiveID != "" && !slices.ContainsFunc(a.manual, func(m simManual) bool {
		return m.directive == n.DirectiveID
	}) {
		return protocol.StepSuperseded
	}

	return protocol.StepRunning
}

// startOperation begins the work on the persisted lease. Its steps begin one
// after the other in the first half of the operation; the first begins now.
func (w *simWorld) startOperation(a *simAgent) {
	op := &protocol.Operation{
		LeaseID:   a.lease.ID,
		Phase:     protocol.PhaseApplying,
		StartedAt: w.now,
		UpdatedAt: w.now,
		Steps:     make([]protocol.Step, 0, len(a.lease.Needs)),
	}

	a.reboot = false
	a.stepFail = -1
	a.beginAt = make([]time.Time, len(a.lease.Needs))

	runs := make([]int, 0, len(a.lease.Needs))

	for i := range a.lease.Needs {
		n := &a.lease.Needs[i]
		status := w.stepFor(a, n)

		if status == protocol.StepRunning {
			status = protocol.StepPending
			op.Disrupted = true
			a.reboot = a.reboot || n.Kind == protocol.NeedReboot

			runs = append(runs, i)
		}

		op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: status})
	}

	a.op = op

	if !op.Disrupted {
		op.Phase = protocol.PhaseDone

		return
	}

	if a.reboot {
		op.Phase = protocol.PhaseDraining
	}

	// Work on slot and independent containers leaves the node up.
	a.down = simLeaseImpact(a, a.lease.Needs) == protocol.ImpactNode

	if a.overrun {
		a.late[op.LeaseID] = true
	}

	a.opEnd = w.now.Add(time.Duration(4+w.rng.IntN(80)) * time.Second)
	a.opFail = !w.calm && w.rng.IntN(25) == 0

	// Now and then an operation takes long: longer than any leaseTtl. Its
	// lease stays open for as long as the agent reports.
	if !w.calm && w.rng.IntN(12) == 0 {
		a.opEnd = w.now.Add(time.Duration(330+w.rng.IntN(600)) * time.Second)
	}

	for k, i := range runs {
		a.beginAt[i] = w.now.Add(a.opEnd.Sub(w.now) / 2 * time.Duration(k) / time.Duration(len(runs)))
	}

	// Now and then one step of several fails while the others succeed.
	if !w.calm && len(runs) > 1 && w.rng.IntN(4) == 0 {
		a.stepFail = runs[w.rng.IntN(len(runs))]
	}

	w.beginSteps(a)
}

// beginSteps begins the steps of the running operation whose time has come.
// An agent that has taken in the cancel of its lease begins none.
func (w *simWorld) beginSteps(a *simAgent) {
	for i := range a.op.Steps {
		step := &a.op.Steps[i]
		if step.Status != protocol.StepPending || a.beginAt[i].IsZero() || w.now.Before(a.beginAt[i]) {
			continue
		}

		if a.cancelled {
			continue
		}

		step.Status = protocol.StepRunning
		a.op.UpdatedAt = w.now
		a.begins = append(a.begins, simBegin{lease: a.op.LeaseID, need: step.NeedID, at: w.now})
	}
}

// decline ends a lease that the agent has not started and never will: it was
// cancelled, or the controller no longer offers it. Nothing is touched, and
// the agent says so.
func (w *simWorld) decline(a *simAgent) {
	op := &protocol.Operation{
		LeaseID:   a.lease.ID,
		Phase:     protocol.PhaseDone,
		StartedAt: w.now,
		UpdatedAt: w.now,
		Steps:     make([]protocol.Step, 0, len(a.lease.Needs)),
	}

	for i := range a.lease.Needs {
		op.Steps = append(op.Steps, protocol.Step{
			NeedID: a.lease.Needs[i].ID, Status: protocol.StepRefused, Error: "the lease is not to be started",
		})
	}

	a.op = op
	a.cancelSeen[op.LeaseID] = w.now
}

// cancel lets an agent take in that its lease was cancelled: one that has not
// started declines it, one that has begins no further step.
func (w *simWorld) cancel(a *simAgent) {
	if a.cancelled || (a.op != nil && a.op.Phase.Terminal()) {
		return
	}

	a.cancelled = true

	if a.op == nil {
		w.decline(a)

		return
	}

	a.cancelSeen[a.op.LeaseID] = w.now
}

// finishOperation ends the running operation and applies what it did.
func (w *simWorld) finishOperation(a *simAgent) {
	op := a.op
	op.UpdatedAt = w.now

	if a.down {
		a.down = false
		w.comeBack(a)
	}

	// A step that had not begun when the agent took in the cancel never
	// began. How it is reported is settled when the operation's end is known.
	defer w.reportUnbegun(a, op)

	if a.opFail {
		// Nothing was changed; the containers are back on what they ran.
		op.Phase = protocol.PhaseFailed
		op.Error = "apt-get exited with status 100"

		for i := range op.Steps {
			if op.Steps[i].Status == protocol.StepRunning {
				op.Steps[i].Status = protocol.StepFailed
				op.Steps[i].Error = op.Error
			}
		}

		return
	}

	op.Phase = protocol.PhaseDone
	rebooted := false

	for i := range op.Steps {
		step := &op.Steps[i]
		if step.Status != protocol.StepRunning {
			continue
		}

		if i == a.stepFail {
			step.Status = protocol.StepFailed
			step.Error = "step exited with status 1"
			op.Phase = protocol.PhaseFailed
			op.Error = step.Error

			continue
		}

		step.Status = protocol.StepDone
		n := &a.lease.Needs[i]

		switch n.Kind {
		case protocol.NeedImage:
			c := a.container(n.Image.Container)
			c.running = n.Image.ToDigest

			if w.digestKind[c.running] == digestFails {
				step.Status = protocol.StepFailed
				step.Error = "container " + c.name + " exited"
				op.Phase = protocol.PhaseFailed
				op.Error = step.Error
			}
		case protocol.NeedPackages:
			a.pkgInstalled = w.pkgVersion
			a.pkgSeen = w.pkgVersion
			a.rebootRequired = a.rebootRequired || w.rng.IntN(4) == 0
		case protocol.NeedOverride, protocol.NeedSpec, protocol.NeedAction:
			// The simulated agents have no overrides file, no agent.yaml
			// that changes and no actions.
		case protocol.NeedReboot:
			rebooted = true
			a.boot++
		}

		if n.DirectiveID != "" && step.Status == protocol.StepDone {
			a.manual = slices.DeleteFunc(a.manual, func(m simManual) bool {
				return m.directive == n.DirectiveID
			})
		}
	}

	// The reboot is the last thing an operation does, so it also covers
	// what the package upgrade of the same operation asked for.
	if rebooted {
		a.rebootRequired = a.rebootSticks
	}
}

// reportUnbegun gives the steps that never began (the agent had taken in the
// cancel of its lease) their final status: cancelled when the cancel is what
// ended the operation; after a failure, as after any failure (pending, not
// attempted).
func (w *simWorld) reportUnbegun(a *simAgent, op *protocol.Operation) {
	failed := op.Phase == protocol.PhaseFailed

	for i := range op.Steps {
		step := &op.Steps[i]
		if step.Status != protocol.StepPending || a.beginAt[i].IsZero() {
			continue
		}

		if failed {
			step.Error = "not attempted: " + op.Error

			continue
		}

		step.Status = protocol.StepCancelled
		step.Error = "cancelled before it had begun"
	}
}

func simPackages(from, to int) []protocol.PackageChange {
	return []protocol.PackageChange{{
		Name: simPackage, From: fmt.Sprintf("1.%d", from), To: fmt.Sprintf("1.%d", to),
	}}
}

// needs lists the agent's open needs. A held agent reports none, and a need
// is reported only for a verb the local configuration allows; a sloppy agent
// reports its observed needs regardless.
func (w *simWorld) needs(a *simAgent) []protocol.Need {
	if a.held() && !a.sloppy {
		return nil
	}

	needs := make([]protocol.Need, 0, 4)

	if a.allows(protocol.VerbRoll) || a.sloppy {
		for _, c := range a.containers {
			if !c.managed || c.custom || c.seen == c.running {
				continue
			}

			needs = append(needs, protocol.Need{
				ID:   protocol.ImageNeedID(c.name, c.seen),
				Kind: protocol.NeedImage,
				Image: &protocol.ImageNeed{
					Container: c.name, Kind: c.kind, Image: c.image, FromDigest: c.running, ToDigest: c.seen,
				},
			})
		}
	}

	if (a.allows(protocol.VerbPackages) || a.sloppy) && a.pkgSeen > a.pkgInstalled {
		pkgs := simPackages(a.pkgInstalled, a.pkgSeen)
		needs = append(needs, protocol.Need{
			ID:       protocol.PackagesNeedID(pkgs),
			Kind:     protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		})
	}

	if (a.allows(protocol.VerbReboot) || a.sloppy) && a.rebootRequired {
		needs = append(needs, rebootNeed())
	}

	if a.held() {
		// Held: the observed needs at most, no manual ones.
		return needs
	}

	// A manual need that local policy no longer permits is dropped.
	a.manual = slices.DeleteFunc(a.manual, func(m simManual) bool {
		if !a.allows(m.kind.Verb()) {
			return true
		}

		c := a.container(m.container)

		return m.kind == protocol.NeedImage && (c == nil || !c.managed || c.custom)
	})

	for _, m := range a.manual {
		n := protocol.Need{ID: protocol.ManualNeedID(m.directive), Kind: m.kind, DirectiveID: m.directive}

		if m.kind == protocol.NeedImage {
			c := a.container(m.container)
			n.Image = &protocol.ImageNeed{
				Container: c.name, Kind: c.kind, Image: c.image, FromDigest: c.running, ToDigest: c.seen,
			}
		} else {
			n.Reboot = &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}}
		}

		needs = append(needs, n)
	}

	return needs
}

// request builds the agent's sync request.
func (w *simWorld) request(a *simAgent) *protocol.SyncRequest {
	req := &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    "sim",
		SentAt:          w.now,
		BootID:          fmt.Sprintf("%s-boot-%d", a.name, a.boot),
		Verbs:           slices.Clone(a.verbs),
		Hold:            protocol.Hold{Local: a.localHold, Controller: a.ctrlHold},
		Containers:      make([]protocol.Container, 0, len(a.containers)),
		Needs:           w.needs(a),
		Operation:       cloneOperation(a.op),
		DirectiveAcks:   slices.Clone(a.acks),
	}

	for _, c := range a.containers {
		pc := protocol.Container{
			Name: c.name, Image: c.image, Digest: c.running, State: "running", Managed: c.managed,
		}

		if c.managed {
			pc.ConfiguredImage = c.image
			pc.Impact = c.impact
			pc.Kind = c.kind
		}

		if c.custom {
			pc.Image = "debug/" + c.image
			pc.Digest = ""
		}

		if c.managed && !c.custom {
			pc.Registry = &protocol.RegistryCheck{Digest: c.seen, CheckedAt: c.checkedAt, Pulled: c.running == c.seen}
		}

		req.Containers = append(req.Containers, pc)
	}

	if a.probes {
		req.Node = w.node(a)
	}

	return req
}

// respond lets the agent take in a sync response.
func (w *simWorld) respond(a *simAgent, resp *Assignment) {
	acks := make([]protocol.DirectiveAck, 0, len(resp.Directives))

	for _, d := range resp.Directives {
		ack, ok := a.handled[d.ID]
		if !ok {
			ack = w.apply(a, d)
			a.handled[d.ID] = ack
		}

		acks = append(acks, ack)
	}

	a.acks = acks
	busy := a.op != nil && !a.op.Phase.Terminal()

	switch {
	case resp.Lease == nil && busy:
		// Once started, the agent finishes.
	case resp.Lease == nil && a.lease != nil && a.op == nil:
		// The controller no longer offers the lease and nothing was done for
		// it: it is not started any more, and the agent says so.
		w.decline(a)
	case resp.Lease == nil:
		// The controller no longer offers the lease: a finished operation is
		// forgotten.
		a.op = nil
		a.lease = nil
		a.cancelled = false
	case a.lease != nil && a.lease.ID == resp.Lease.ID:
		if resp.Lease.Cancel {
			w.cancel(a)
		}
	case busy:
	case resp.Lease.Cancel:
		// A lease that is offered cancelled is not taken.
	default:
		l := *resp.Lease
		l.Needs = slices.Clone(resp.Lease.Needs)
		a.lease = &l
		a.op = nil
		a.cancelled = false
		a.deadline = w.now.Add(time.Duration(l.TTLSeconds) * time.Second)

		if l.ExpiresAt.Before(a.deadline) {
			a.deadline = l.ExpiresAt
		}

		// The response took a while to arrive and the agent's clock is not
		// the controller's: its deadline lies a little after the lease's.
		a.deadline = a.deadline.Add(time.Duration(w.rng.IntN(simLatencySeconds+1)) * time.Second)

		delay := time.Duration(w.rng.IntN(8)) * time.Second

		// A host whose validators are down is one that gets a repair lease;
		// its agent is late more often, so that such leases expire and are
		// taken up again too.
		late := 6
		if a.dead || w.now.Before(a.outageUntil) {
			late = 14
		}

		switch n := w.rng.IntN(20); {
		case w.calm:
		case n == 0:
			delay = time.Hour
		case n < 4:
			delay = time.Duration(10+w.rng.IntN(60)) * time.Second
		case n < late:
			// Start in the last moments before the deadline.
			delay = max(a.deadline.Sub(w.now)-time.Duration(1+w.rng.IntN(25))*time.Second, 0)
		}

		// Now and then the agent's clock is slow by more than the grace the
		// controller gives: it starts a lease that the controller has just
		// expired. The controller takes such a lease up again.
		a.overrun = !w.calm && w.rng.IntN(20) < late-4
		if a.overrun {
			a.deadline = a.deadline.Add(time.Duration(61+w.rng.IntN(90)) * time.Second)
			delay = max(a.deadline.Sub(w.now)-time.Duration(1+w.rng.IntN(40))*time.Second, 0)
		}

		a.startAt = w.now.Add(delay)
	}
}

// apply executes one directive.
func (w *simWorld) apply(a *simAgent, d protocol.Directive) protocol.DirectiveAck {
	ack := protocol.DirectiveAck{ID: d.ID, Status: protocol.DirectiveAccepted}
	reject := func(reason string) protocol.DirectiveAck {
		ack.Status = protocol.DirectiveRejected
		ack.Error = reason

		return ack
	}

	switch d.Kind {
	case protocol.DirectiveHold:
		a.ctrlHold = true
	case protocol.DirectiveReleaseHold:
		a.ctrlHold = false
	case protocol.DirectiveOverride:
		return reject("the simulated agents have no overrides file")
	case protocol.DirectiveAction:
		return reject("the simulated agents have no actions")
	case protocol.DirectiveRoll:
		c := a.container(d.Container)

		switch {
		case !a.allows(protocol.VerbRoll):
			return reject("verb roll is not allowed")
		case a.held():
			return reject("host is on hold")
		case c == nil || !c.managed:
			return reject("container is not managed")
		case c.custom:
			return reject("container runs a custom image")
		}

		a.manual = append(a.manual, simManual{directive: d.ID, kind: protocol.NeedImage, container: c.name})
	case protocol.DirectiveReboot:
		switch {
		case !a.allows(protocol.VerbReboot):
			return reject("verb reboot is not allowed")
		case a.held():
			return reject("host is on hold")
		}

		a.manual = append(a.manual, simManual{directive: d.ID, kind: protocol.NeedReboot})
	case protocol.DirectiveWithdraw:
		a.manual = slices.DeleteFunc(a.manual, func(m simManual) bool { return m.directive == d.Withdraws })
	}

	return ack
}

// onTarget reports whether the host has nothing left to do that its
// configuration lets it do.
func (w *simWorld) onTarget(a *simAgent) error {
	if a.allows(protocol.VerbRoll) {
		for _, c := range a.containers {
			if c.managed && !c.custom && c.running != w.registry[c.image] {
				return fmt.Errorf("%s: container %s runs %s, the tag points at %s",
					a.name, c.name, shortDigest(c.running), shortDigest(w.registry[c.image]))
			}
		}
	}

	if a.allows(protocol.VerbPackages) && a.pkgInstalled != w.pkgVersion {
		return fmt.Errorf("%s: package version 1.%d installed, 1.%d available", a.name, a.pkgInstalled, w.pkgVersion)
	}

	if a.allows(protocol.VerbReboot) && a.rebootRequired {
		return fmt.Errorf("%s: reboot still required", a.name)
	}

	return nil
}

// The oracle holds the committed state of the scheduler against a small set
// of invariants, with the simulated world's own numbers. It is no second
// scheduler: it does not say who should have been granted, only what must
// never happen.
//
//  1. At every grant the availability budget holds by the world's count of
//     who attests, unless every rollout the lease serves ignores the budget.
//  2. At every such grant fewer hosts than the host limit hold a lease.
//  3. A host holds one open lease at most. No lease is granted to a host
//     that is held or stale or works on an operation without a lease, under
//     the global pause, or for a rollout that is closed, halted, paused or
//     not approved. Ended leases and closed rollouts stay as they ended.
//
// A host whose agent reports an operation that is not over while the host
// holds no lease counts as leased in 1, 2 and 4, and so does a host that the
// scheduler marked as not proven back after a lease (fleet.Host.Unproven),
// with its full weight, unless it is the host granted.
//  9. Such a mark is set, unless the host is stale, when a node lease fails
//     after its agent reported a failure of an operation that disrupted the
//     host.
// 10. While the gate is open, a lease granted in a rollout that has a canary
//     and no success yet goes to the canary or to a host in the repair stage.
//  4. No such grant while the world's gate is closed, with one exception:
//     the gate is closed for participation or finality (not for want of
//     data), none of the host's validators attests and nothing is leased.
//
// 1, 2 and 4 hold per impact class, by the class of the containers the
// lease's needs are about as the host reported them: a node lease is what
// they say; an independent lease
// counts for neither budget nor host limit and passes a closed gate.
//
//  7. At every grant of an independent lease fewer hosts than the
//     independent limit hold one.
//  8. A lease that serves an image need of a unit (kind and team as the
//     host reports them) serves every such need the host reported in its
//     last sync: one lease per host per unit target.
//  5. After a lease failed, nothing is granted in the rollouts it served
//     until a person retries them (or they are closed).
//  6. A lease is released only after its agent reported the operation done.
//     (That an agent begins a step only under an open lease is checked where
//     the steps are seen: sim.checkStarts.)
//
// Restart equivalence and convergence after healing are the driver's
// (sim.reconcile, sim.converge).

// simHostFact is what the simulated world knows about one registered host.
// None of it comes from the scheduler.
type simHostFact struct {
	weight uint64
	// live is how many validators attested in the newest liveness epoch the
	// beacon service settled (the full weight without a row).
	live uint64
	// holdWanted is the operator's hold switch; offline says that the
	// operator's known-offline mark is in force.
	holdWanted bool
	offline    bool
	// unproven is the scheduler's mark that the host is not proven back after
	// a lease (fleet.Host.Unproven).
	unproven *time.Time
	// lastSync and status are the agent's last sync as it sent it.
	lastSync time.Time
	status   *protocol.SyncRequest
	// reported is the agent's own record, per lease ID, of the last operation
	// it sent to the controller for the lease.
	reported map[string]*protocol.Operation
}

func (h *simHostFact) held() bool {
	return h.holdWanted || h.offline || (h.status != nil && h.status.Hold.Active())
}

// simFacts is the state of the simulated world at one observation.
type simFacts struct {
	now time.Time
	// since is when the controller last started. It cannot have heard from
	// an agent before that, so silence is counted from the later of the last
	// sync and since.
	since    time.Time
	settings fleet.Settings
	health   fleet.NetworkHealth
	hosts    map[string]*simHostFact
	// publishedEpoch is the newest epoch with liveness rows in the store.
	publishedEpoch uint64
	havePublished  bool
	// epoch is the length of the epoch the beacon view was taken in, by the
	// simulated chain's own clock.
	epoch time.Duration
	// retried names the rollouts a person retried since the last
	// observation.
	retried []string
}

// stale: no sync for hostStaleAfter, counted from the controller's start at
// the earliest.
func (f *simFacts) stale(h *simHostFact) bool {
	return f.now.Sub(later(h.lastSync, f.since)) > f.settings.HostStaleAfter.Std()
}

// working: by its last sync the agent is in the middle of an operation. A
// host that is stale says nothing.
func (f *simFacts) working(h *simHostFact) bool {
	return !f.stale(h) && h.status != nil && h.status.Operation != nil &&
		!h.status.Operation.Phase.Terminal()
}

// charged: the scheduler marked the host as not proven back after a lease,
// and it is not stale.
func (f *simFacts) charged(h *simHostFact) bool {
	return h.unproven != nil && !f.stale(h)
}

// chargedOthers counts the charged hosts other than host that do not hold a
// node lease (those count by their lease already).
func (f *simFacts) chargedOthers(leased map[string]protocol.Impact, host string) int {
	n := 0

	for name, h := range f.hosts {
		if name != host && f.charged(h) && leased[name] != protocol.ImpactNode {
			n++
		}
	}

	return n
}

// simSnapshot is the committed scheduler state at one observation, in
// creation order.
type simSnapshot struct {
	rollouts []*fleet.Rollout
	leases   []*fleet.Lease
}

func (s *simSnapshot) rollout(id string) *fleet.Rollout {
	for _, r := range s.rollouts {
		if r.ID == id {
			return r
		}
	}

	return nil
}

// oracle is what the invariants need to remember between two observations.
type oracle struct {
	rollouts map[string]fleet.RolloutState
	leases   map[string]fleet.LeaseState
	// serves maps, per lease, each need to the rollout it is served for, as
	// the oracle first saw it.
	serves map[string]map[string]string
	// halted maps a rollout to the lease whose failure halted it, until a
	// person retries it.
	halted map[string]string
	// exceptions counts the grants that passed a closed gate; working the
	// observations of a host that works without a lease.
	exceptions, working int
}

func newOracle() *oracle {
	return &oracle{
		rollouts: make(map[string]fleet.RolloutState, 16),
		leases:   make(map[string]fleet.LeaseState, 64),
		serves:   make(map[string]map[string]string, 64),
		halted:   make(map[string]string, 4),
	}
}

// observe checks one observation against the history and returns the first
// violated invariant.
func (o *oracle) observe(f *simFacts, snap *simSnapshot) error {
	for _, r := range snap.rollouts {
		if was, known := o.rollouts[r.ID]; known && was.Terminal() && was != r.State {
			return fmt.Errorf("rollout %s was %s and is %s", r.ID, was, r.State)
		}

		o.rollouts[r.ID] = r.State

		if r.State.Terminal() {
			delete(o.halted, r.ID)
		}
	}

	for _, id := range f.retried {
		delete(o.halted, id)
	}

	var (
		granted []*fleet.Lease
		// leased holds the class of every host that holds an open lease.
		leased = make(map[string]protocol.Impact, len(snap.leases))
	)

	for _, l := range snap.leases {
		was, known := o.leases[l.ID]
		o.leases[l.ID] = l.State

		switch {
		case !known && !l.Recheck:
			o.serves[l.ID] = maps.Clone(l.Rollouts)
			granted = append(granted, l)

			continue
		case !known:
			// A recheck comes from a person's retry, not from a grant.
			o.serves[l.ID] = maps.Clone(l.Rollouts)
		case was.Terminal() && was != l.State:
			return fmt.Errorf("lease %s of %s was %s and is %s", l.ID, l.Host, was, l.State)
		}

		if !l.State.Terminal() {
			if leased[l.Host] != "" {
				return fmt.Errorf("host %s holds two open leases", l.Host)
			}

			leased[l.Host] = oracleImpact(f.hosts[l.Host], l)
		}

		if l.State == was {
			continue
		}

		if err := o.checkEnd(f, snap, l); err != nil {
			return fmt.Errorf("lease %s of %s: %w", l.ID, l.Host, err)
		}
	}

	// Hosts that work without a lease take a slot like the leased ones.
	for name, h := range f.hosts {
		if leased[name] == "" && f.working(h) {
			leased[name] = protocol.ImpactNode
			o.working++
		}
	}

	// The gate is evaluated once per reconcile, before the first grant.
	gate := oracleGate(f, leased)

	for _, l := range granted {
		if err := o.checkGrant(f, snap, l, leased, gate); err != nil {
			return fmt.Errorf("grant of lease %s to %s: %w", l.ID, l.Host, err)
		}

		leased[l.Host] = oracleImpact(f.hosts[l.Host], l)
	}

	return nil
}

// oracleImpact is the class of a lease by the containers its needs are about,
// as the host last reported them: node for a need about no container or one
// the host does not report.
func oracleImpact(h *simHostFact, l *fleet.Lease) protocol.Impact {
	impact := protocol.ImpactIndependent

	for i := range l.Needs {
		impact = impact.Heavier(oracleNeedImpact(h, &l.Needs[i]))
	}

	return impact
}

// oracleNeedImpact is the class of one need by the container it is about.
func oracleNeedImpact(h *simHostFact, n *protocol.Need) protocol.Impact {
	name := n.Container()
	if h == nil || h.status == nil || name == "" {
		return protocol.ImpactNode
	}

	for _, rc := range h.status.Containers {
		if rc.Name == name && rc.Impact != "" {
			return rc.Impact
		}
	}

	return protocol.ImpactNode
}

// oracleCount counts the leased hosts of the classes.
func oracleCount(leased map[string]protocol.Impact, classes ...protocol.Impact) int {
	n := 0

	for _, impact := range leased {
		if slices.Contains(classes, impact) {
			n++
		}
	}

	return n
}

// checkEnd looks at a lease that changed its state: one that was released
// must have been reported done by its agent, and one that failed halts the
// rollouts it served.
func (o *oracle) checkEnd(f *simFacts, snap *simSnapshot, l *fleet.Lease) error {
	var report *protocol.Operation
	if host := f.hosts[l.Host]; host != nil {
		report = host.reported[l.ID]
	}

	// Invariant 9: a node lease that failed after its agent disrupted the
	// host leaves the host charged.
	if host := f.hosts[l.Host]; host != nil && !f.stale(host) && host.unproven == nil &&
		leaseImpact(l) == protocol.ImpactNode && l.State == fleet.LeaseFailed &&
		report != nil && report.Disrupted && report.Phase == protocol.PhaseFailed {
		return errors.New("failed after its agent disrupted the host, and the host is not charged")
	}

	switch l.State {
	case fleet.LeaseReleased:
		// A recheck repeats the readiness check of a lease whose agent did.
		if !l.Recheck && (report == nil || report.Phase != protocol.PhaseDone) {
			return errors.New("released, and its agent never reported the operation done")
		}
	case fleet.LeaseFailed:
		for _, id := range o.serves[l.ID] {
			r := snap.rollout(id)
			if r == nil || r.State.Terminal() || oracleSpared(report, o.serves[l.ID], id) {
				continue
			}

			if r.State != fleet.RolloutHalted {
				return fmt.Errorf("failed, and rollout %s (%s) it served is %s", r.ID, r.Key, r.State)
			}

			if o.halted[id] == "" {
				o.halted[id] = l.ID
			}
		}
	default:
	}

	return nil
}

// oracleSpared reports whether, by the agent's own report, every step the
// lease served for the rollout was done while another step failed: such a
// rollout is not stopped by the failure.
func oracleSpared(report *protocol.Operation, serves map[string]string, rollout string) bool {
	if report == nil || !slices.ContainsFunc(report.Steps, func(s protocol.Step) bool {
		return s.Status == protocol.StepFailed
	}) {
		return false
	}

	found := false

	for need, id := range serves {
		if id != rollout {
			continue
		}

		if !slices.ContainsFunc(report.Steps, func(s protocol.Step) bool {
			return s.NeedID == need && s.Status == protocol.StepDone
		}) {
			return false
		}

		found = true
	}

	return found
}

// oracleGate evaluates the network gate from the world. leased are the hosts
// that hold a lease or work without one; they are left out of the
// participation.
func oracleGate(f *simFacts, leased map[string]protocol.Impact) []string {
	var (
		reasons              []string
		n                    = &f.health
		tracked, total, live uint64
	)

	if n.UpdatedAt.IsZero() || f.now.Sub(n.UpdatedAt) > 2*f.epoch {
		reasons = append(reasons, gateBeacon)
	}

	if n.FinalityLagEpochs > f.settings.MaxFinalityLagEpochs {
		reasons = append(reasons, "finality lag")
	}

	for name, h := range f.hosts {
		tracked += h.weight

		// A host marked known offline is left out, like one at work, and so
		// is a charged one.
		if leased[name] == "" && !h.offline && !f.charged(h) {
			total += h.weight
			live += h.live
		}
	}

	switch {
	case tracked == 0:
	case !n.LivenessKnown || !f.havePublished || n.CurrentEpoch > f.publishedEpoch+2:
		reasons = append(reasons, gateLiveness)
	case total > 0 && float64(live)*100/float64(total) < f.settings.MinParticipationPercent:
		reasons = append(reasons, "participation")
	}

	return reasons
}

// oracleBudget checks the three grant conditions (the host costs nothing, it
// stays inside the budget, or it goes alone) and the host limit for one host,
// against the world's own count of unavailable weight.
func oracleBudget(f *simFacts, leased map[string]protocol.Impact, host string, impact protocol.Impact) error {
	if impact == protocol.ImpactIndependent {
		limit := max(1, int(math.Floor(f.settings.MaxIndependentPercent*float64(len(f.hosts))/100+budgetEpsilon)))
		if n := oracleCount(leased, protocol.ImpactIndependent); n >= limit {
			return fmt.Errorf("independent limit: %d hosts already hold an independent lease, at most %d of %d may",
				n, limit, len(f.hosts))
		}

		return nil
	}

	var total, unavailable uint64

	for name, h := range f.hosts {
		// The host granted is not counted by its charge: its lease charges it.
		charged := name != host && f.charged(h)

		// A host marked known offline that holds no lease is not counted.
		if h.offline && leased[name] == "" && !charged {
			continue
		}

		total += h.weight

		if leased[name] == protocol.ImpactNode || charged {
			unavailable += h.weight
		} else {
			unavailable += h.weight - h.live
		}
	}

	percent := f.settings.MaxUnavailablePercent
	allowed := percent * float64(total) / 100
	others := oracleCount(leased, protocol.ImpactNode) + f.chargedOthers(leased, host)

	cost := f.hosts[host].live
	if impact != protocol.ImpactNode {
		cost = 0
	}

	fits := cost == 0 ||
		float64(unavailable+cost) <= allowed+budgetEpsilon ||
		(unavailable == 0 && allowed > 0 && others == 0)
	if !fits {
		return fmt.Errorf("over budget: cost %d with %d of %d already unavailable, %v allowed and %d other leases",
			cost, unavailable, total, allowed, others)
	}

	limit := max(1, int(math.Floor(f.settings.MaxLeasedPercent*float64(len(f.hosts))/100+budgetEpsilon)))
	if others >= limit {
		return fmt.Errorf("host limit: %d hosts already hold a node lease, at most %d of %d may",
			others, limit, len(f.hosts))
	}

	return nil
}

// checkGrant checks one new lease. leased are the hosts that held a lease
// before it; gate are the reasons the world's gate is closed for.
func (o *oracle) checkGrant(
	f *simFacts, snap *simSnapshot, l *fleet.Lease, leased map[string]protocol.Impact, gate []string,
) error {
	host := f.hosts[l.Host]
	impact := oracleImpact(host, l)

	switch {
	case l.State != fleet.LeaseGranted:
		return fmt.Errorf("new lease is %s, not granted", l.State)
	case host == nil || host.status == nil:
		return errors.New("the host is unknown or has never synced")
	case leased[l.Host] != "":
		return errors.New("the host already holds a lease, or works on an operation without one")
	case host.held():
		return errors.New("the host is held")
	case f.stale(host):
		return fmt.Errorf("the host is stale: last sync %s ago, controller started %s ago",
			f.now.Sub(host.lastSync), f.now.Sub(f.since))
	case f.settings.Paused:
		return errors.New("the global pause is in effect")
	case len(l.Needs) == 0:
		return errors.New("the lease carries no need")
	}

	if l.Impact != impact {
		return fmt.Errorf("the lease is of class %q, its needs are %q", l.Impact, impact)
	}

	if err := oracleWholeGroups(host, l); err != nil {
		return err
	}

	// bound is the class of the needs whose rollouts do not ignore the
	// budget: they alone answer to the gate and the limits.
	exempt, anyExempt := true, false
	bound := protocol.ImpactIndependent

	for i := range l.Needs {
		r := snap.rollout(l.Rollouts[l.Needs[i].ID])
		if r == nil {
			return fmt.Errorf("need %s is served for no rollout", l.Needs[i].ID)
		}

		if err := o.checkRollout(f, r, l, len(gate) == 0); err != nil {
			return fmt.Errorf("rollout %s (%s): %w", r.ID, r.Key, err)
		}

		anyExempt = anyExempt || r.IgnoreBudget

		if !r.IgnoreBudget {
			exempt = false
			bound = bound.Heavier(oracleNeedImpact(host, &l.Needs[i]))
		}
	}

	// Independent work rides along with other work that takes the host
	// anyway; it needs a place of its own only in a lease of its own.
	if exempt || (bound == protocol.ImpactIndependent && (anyExempt || impact != protocol.ImpactIndependent)) {
		return nil
	}

	if len(gate) > 0 && bound != protocol.ImpactIndependent {
		// The one exception: the gate is closed for what the network does, not
		// for want of data, none of the host's validators attests, and nothing
		// at all is leased.
		if slices.Contains(gate, gateBeacon) || slices.Contains(gate, gateLiveness) {
			return fmt.Errorf("the network gate is closed for want of data: %v", gate)
		}

		if host.weight == 0 || host.live != 0 {
			return fmt.Errorf("the network gate is closed (%v) and %d of the host's %d validators attest",
				gate, host.live, host.weight)
		}

		if n := oracleCount(leased, protocol.ImpactNode) + f.chargedOthers(leased, l.Host); n > 0 {
			return fmt.Errorf("the network gate is closed (%v) and %d hosts are leased already", gate, n)
		}

		o.exceptions++
	}

	return oracleBudget(f, leased, l.Host, bound)
}

// oracleSucceeded reports whether a member of the rollout is done through a
// lease that was released.
func oracleSucceeded(r *fleet.Rollout) bool {
	return slices.ContainsFunc(r.Hosts, func(m fleet.RolloutHost) bool {
		return m.State == fleet.HostDone && m.LeaseID != ""
	})
}

// oracleWholeGroups is invariant 8: a lease that serves a need of a roll
// unit serves every observed image need of that unit the host reported in
// its last sync, so that a unit goes down once and is checked once. The
// unit is what the host reports of the need (its kind, its team), not what
// the scheduler made of it.
func oracleWholeGroups(h *simHostFact, l *fleet.Lease) error {
	in := make(map[string]bool, len(l.Needs))
	groups := make(map[[2]string]bool, len(l.Needs))

	// Unlabelled containers are a unit per image.
	key := func(n *protocol.ImageNeed) [2]string {
		if n.Kind == "" {
			return [2]string{"", imageref.Normalize(n.Image)}
		}

		return [2]string{string(n.Kind), n.Team}
	}

	for i := range l.Needs {
		n := &l.Needs[i]
		in[n.ID] = true

		if n.DirectiveID == "" && n.Image != nil {
			groups[key(n.Image)] = true
		}
	}

	for i := range h.status.Needs {
		n := &h.status.Needs[i]
		if n.DirectiveID != "" || n.Image == nil || !groups[key(n.Image)] {
			continue
		}

		if !in[n.ID] {
			return fmt.Errorf("the lease serves kind %q and leaves out the host's need %s of it",
				n.Image.Kind, n.ID)
		}
	}

	return nil
}

// checkRollout checks that a rollout may be granted in: it is open, not
// halted, not paused and approved where its policy asks for that, and the
// host is its member under this lease. While the gate is open (its one
// exception skips the order), the host does not wait for the canary
// (invariant 10).
func (o *oracle) checkRollout(f *simFacts, r *fleet.Rollout, l *fleet.Lease, gateOpen bool) error {
	policy := fleet.PolicyAuto

	switch r.Kind {
	case fleet.RolloutGroup:
		policy = f.settings.ImagePolicy(r.Team)
	case fleet.RolloutPackages:
		policy = f.settings.PolicyPackages
	case fleet.RolloutReboot:
		policy = f.settings.PolicyReboot
	default:
	}

	m := r.Host(l.Host)

	switch {
	case r.State != fleet.RolloutActive:
		return fmt.Errorf("the rollout is %s", r.State)
	case o.halted[r.ID] != "":
		return fmt.Errorf("lease %s failed in it and nobody retried it", o.halted[r.ID])
	case r.Paused:
		return errors.New("the rollout is paused")
	case policy == fleet.PolicyManual && !r.Approved:
		return errors.New("the policy is manual and nobody approved the rollout")
	case m == nil || m.State != fleet.HostLeased || m.LeaseID != l.ID:
		return errors.New("the host is not a member leased by this lease")
	case gateOpen && r.Canary != "" && r.Canary != l.Host && !oracleSucceeded(r) && m.Stage != fleet.StageRepair:
		return fmt.Errorf("the host waits for the canary %s", r.Canary)
	default:
		return nil
	}
}
