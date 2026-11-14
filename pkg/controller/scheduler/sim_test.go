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
// operator actions, controller restarts and store write failures. After
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
	seed     uint64
	rng      *rand.Rand
	world    *simWorld
	store    *fakeStore
	svc      *service
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
	stats    simStats
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
	ConvergeTicks, MaxConvergeTicks       int
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
	a.Adopted += b.Adopted
	a.ConvergeTicks += b.ConvergeTicks
	a.MaxConvergeTicks = max(a.MaxConvergeTicks, b.MaxConvergeTicks)

	if a.Waits == nil {
		a.Waits = make(map[string]int, len(b.Waits))
	}

	for k, v := range b.Waits {
		a.Waits[k] += v
	}
}

// count reads the run's totals off the audit trail and the store.
func (s *sim) count() {
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

		if exempt {
			s.stats.ExemptGrants++
		}

		if len(ids) > 1 {
			s.stats.Bundled++
		}
	}

	s.stats.Hosts = len(s.world.agents)
	s.stats.Seeds = 1
}

func newSim(seed uint64) (*sim, error) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	s := &sim{
		seed:   seed,
		rng:    rng,
		world:  newSimWorld(rng),
		store:  newFakeStore(),
		oracle: newOracle(),
		log:    log,
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
		SoakEpochs:              uint64(pick(0, 1, 1, 2)),
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

// boot (re)starts the controller on the store.
func (s *sim) boot(st *fakeStore) error {
	svc, err := s.service(st, s.newID)
	s.svc = svc

	return err
}

func (s *sim) note(format string, args ...any) {
	at := s.world.now.Sub(testGenesis.Add(50 * simEpoch)).Truncate(time.Second)
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
// that moment. A lease the store has never seen (the database was lost) is
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

		s.store.recordSync(host)

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
		settings:       settings,
		health:         w.health(),
		hosts:          make(map[string]*simHostFact, len(w.agents)),
		rows:           w.published,
		publishedEpoch: w.publishedEpoch,
		havePublished:  w.havePublished,
		retried:        s.retried,
		fresh:          fresh,
	}

	for _, a := range w.agents {
		registered := s.store.host(a.name)
		if registered == nil {
			continue
		}

		f.hosts[a.name] = &simHostFact{
			weight:     a.weight,
			bootnode:   a.bootnode,
			live:       w.publishedLive(a),
			holdWanted: registered.HoldWanted,
			lastSync:   a.lastSync,
			status:     a.lastSent,
			disrupted:  a.disrupted,
			doneAt:     a.doneAt,
		}
	}

	return f
}

// observe hands the committed state to the oracle, unless nothing was
// written since the last observation.
func (s *sim) observe(fresh bool) error {
	writes := s.store.writeCount()
	if writes == s.observed && len(s.retried) == 0 {
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
		errors.Is(err, ErrUnknownRollout), errors.Is(err, errWrite):
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
		s.note("%s: reboot required", a.name)
	case n < 600:
		s.worldEvent()
	case n < 650:
		s.agentEvent()
	case n < 690:
		s.note("controller restart")
		s.stats.Restarts++

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

// loseDatabase restarts the controller on an empty database: rollouts,
// leases, settings and liveness are gone. Agents that are in the middle of an
// operation keep reporting it, and the scheduler must adopt those leases.
func (s *sim) loseDatabase() error {
	s.note("database lost")
	s.stats.LostDatabases++

	s.store = newFakeStore()
	s.oracle = newOracle()
	s.observed = 0
	s.retried = nil

	for _, a := range s.world.agents {
		s.world.dropped[a.name] = true
	}

	return s.boot(s.store)
}

// worldEvent starts or ends trouble that has nothing to do with rollouts.
func (s *sim) worldEvent() {
	w := s.world

	switch s.rng.IntN(10) {
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

// agentEvent changes something on one host.
func (s *sim) agentEvent() {
	w := s.world
	a := s.agent()

	switch s.rng.IntN(10) {
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
			return r.State == fleet.RolloutHalted || r.State == fleet.RolloutAborted
		})
		if r == nil {
			return nil
		}

		req := RolloutHostsRequest{Rollout: r.ID}
		if s.rng.IntN(5) == 0 {
			req.Hosts = s.someHosts()
		}

		res, err := s.svc.Retry(ctx, simActor, req)
		if err == nil {
			s.retried = append(s.retried, r.ID)
		}

		return s.act(fmt.Sprintf("retry %s %v", r.ID, req.Hosts), res, err)
	case n < 50:
		hosts := s.someHosts()
		res, err := s.svc.Hold(ctx, simActor, HoldRequest{Hosts: hosts, Reason: "sim"})

		return s.act(fmt.Sprintf("hold %v", hosts), res, err)
	case n < 60:
		hosts := s.someHosts()
		res, err := s.svc.Unhold(ctx, simActor, HoldRequest{Hosts: hosts})

		return s.act(fmt.Sprintf("unhold %v", hosts), res, err)
	case n < 80:
		return s.manualAction()
	case n < 97:
		return s.settingsAction()
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
		set.SoakEpochs = uint64(s.rng.IntN(3))
		what = fmt.Sprintf("soakEpochs=%d", set.SoakEpochs)
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
	w.livenessStallUntil, w.livenessUnknownUntil = time.Time{}, time.Time{}
	s.store.setFailing(false, 0)

	for d := range w.digestKind {
		w.digestKind[d] = digestGood
	}

	for _, a := range w.agents {
		a.silentUntil, a.outageUntil, a.localHold = time.Time{}, time.Time{}, false
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
			if err := s.retry(r.ID); err != nil {
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

		if r.State == fleet.RolloutAborted && !open[r.Key] && reported[r.Key] {
			if err := s.retry(r.ID); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *sim) retry(id string) error {
	res, err := s.svc.Retry(context.Background(), simActor, RolloutHostsRequest{Rollout: id})
	if err == nil {
		s.retried = append(s.retried, id)
	}

	return s.act("retry "+id, res, err)
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
			"adopted leases":       total.Adopted,
			"waits for the budget": total.Waits["budget"], "waits for the host limit": total.Waits["host"],
			"waits for the gate": total.Waits["network"], "waits for the order": total.Waits["after"],
		} {
			if count == 0 {
				t.Errorf("the simulation never produced any %s", what)
			}
		}
	}
}
