package scheduler

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// These tests pin the checker itself: hand-built histories that break one
// rule each must be rejected, and the same histories without the breach
// accepted. Without them a simulation that passes could mean an oracle that
// checks nothing.

const (
	pinRollout = "r1"
	pinEpoch   = 99
)

// pinned is a hand-built history: a fleet of one host without validators
// (n1), four validator hosts (v1-v3 of weight 10, v4 of weight 70) and a
// bootnode (b1), all reporting the same image need, and one image rollout.
// With 40% the budget allows 40 of 100 validators and two leases at once.
// Every host runs a node, so each client needs minPeers (3) peers.
type pinned struct {
	t    *testing.T
	o    *oracle
	f    *simFacts
	snap *simSnapshot
	ids  int
	// digest is the digest the open rollout rolls to.
	digest string
}

func newPinned(t *testing.T) *pinned {
	t.Helper()

	now := testGenesis.Add((pinEpoch+1)*simEpoch + 5*time.Second)
	settings := fleet.DefaultSettings()
	settings.MaxUnavailablePercent = 40

	p := &pinned{
		t:      t,
		digest: digestA,
		o:      newOracle(),
		f: &simFacts{
			now:      now,
			settings: settings,
			health: fleet.NetworkHealth{
				UpdatedAt: now, GenesisTime: testGenesis,
				SecondsPerSlot: simSecondsPerSlot, SlotsPerEpoch: simSlotsPerEpoch,
				CurrentEpoch: pinEpoch + 1, FinalizedEpoch: pinEpoch - 1, FinalityLagEpochs: 2,
				LivenessKnown: true, LivenessEpoch: pinEpoch,
			},
			hosts:          make(map[string]*simHostFact, 6),
			rows:           map[uint64]map[string]uint64{pinEpoch - 1: {}, pinEpoch: {}},
			publishedEpoch: pinEpoch,
			havePublished:  true,
		},
		snap: &simSnapshot{},
	}

	r := &fleet.Rollout{
		ID: pinRollout, Kind: fleet.RolloutImage, Key: imageKey(imgGeth, digestA),
		Image: imgGeth, Digest: digestA, State: fleet.RolloutActive,
		CreatedAt: now.Add(-time.Hour),
	}

	for _, h := range []struct {
		name   string
		weight uint64
	}{{"n1", 0}, {"v1", 10}, {"v2", 10}, {"v3", 10}, {"v4", 70}, {"b1", 0}} {
		p.f.hosts[h.name] = &simHostFact{
			weight: h.weight, live: h.weight, lastSync: now,
			status: &protocol.SyncRequest{
				Verbs:      slices.Clone(allVerbs),
				Containers: []protocol.Container{container("c", imgGeth, digestOld, digestA, now)},
				Needs:      []protocol.Need{imageNeed("c", imgGeth, digestA)},
				Node:       nodeWith(9, 9),
			},
			disrupted: map[string]bool{},
			doneAt:    map[string]time.Time{},
		}

		if h.weight > 0 {
			p.f.rows[pinEpoch-1][h.name] = h.weight
			p.f.rows[pinEpoch][h.name] = h.weight
		}

		r.Hosts = append(r.Hosts, fleet.RolloutHost{
			Host: h.name, State: fleet.HostPending, Stage: fleet.StageRest,
		})
	}

	p.snap.rollouts = []*fleet.Rollout{r}

	require.NoError(t, p.observe(), "the base history must be accepted")

	return p
}

// rollout is the open rollout of the history: the newest one.
func (p *pinned) rollout() *fleet.Rollout { return p.snap.rollouts[len(p.snap.rollouts)-1] }

// offline makes none of the host's validators attest in the two newest
// epochs.
func (p *pinned) offline(host string) {
	p.f.hosts[host].live = 0
	p.f.rows[pinEpoch-1][host] = 0
	p.f.rows[pinEpoch][host] = 0
}

// successor moves the tag to digest B: the rollout so far is superseded with
// the named hosts failed in it, and the scheduler opens the next one, in
// which it marks them as carried.
func (p *pinned) successor(failed ...string) *fleet.Rollout {
	p.t.Helper()

	old := p.rollout()
	old.State = fleet.RolloutSuperseded

	next := &fleet.Rollout{
		ID: "r2", Kind: fleet.RolloutImage, Key: imageKey(imgGeth, digestB),
		Image: imgGeth, Digest: digestB, State: fleet.RolloutActive,
		CreatedAt: p.f.now.Add(-time.Minute),
	}

	for i := range old.Hosts {
		m := &old.Hosts[i]
		member := fleet.RolloutHost{Host: m.Host, State: fleet.HostPending, Stage: fleet.StageRest}

		if slices.Contains(failed, m.Host) {
			m.State = fleet.HostFailed
			member.CarriedFrom = old.ID
		}

		next.Hosts = append(next.Hosts, member)

		st := p.f.hosts[m.Host].status
		st.Containers = []protocol.Container{container("c", imgGeth, digestOld, digestB, p.f.now)}
		st.Needs = []protocol.Need{imageNeed("c", imgGeth, digestB)}
	}

	p.snap.rollouts = append(p.snap.rollouts, next)
	p.digest = digestB
	p.reconciled()

	return next
}

// observe hands a copy of the current state to the oracle.
func (p *pinned) observe() error {
	snap := &simSnapshot{directives: p.snap.directives}

	for _, r := range p.snap.rollouts {
		snap.rollouts = append(snap.rollouts, cloneRollout(r))
	}

	for _, l := range p.snap.leases {
		snap.leases = append(snap.leases, cloneLease(l))
	}

	err := p.o.observe(p.f, snap)
	p.f.retried = nil

	return err
}

// ok observes and requires the history so far to be accepted.
func (p *pinned) ok() {
	p.t.Helper()
	require.NoError(p.t, p.observe())
}

func (p *pinned) lease(host string) *fleet.Lease {
	for _, l := range p.snap.leases {
		if l.Host == host && !l.State.Terminal() {
			return l
		}
	}

	p.t.Fatalf("host %s holds no lease", host)

	return nil
}

// grant writes a lease for the host's image need, as the scheduler would.
func (p *pinned) grant(host string) *fleet.Lease {
	p.ids++
	need := imageNeed("c", imgGeth, p.digest)
	l := &fleet.Lease{
		ID: fmt.Sprintf("l%d", p.ids), Host: host, State: fleet.LeaseGranted,
		Needs: []protocol.Need{need}, Rollouts: map[string]string{need.ID: p.rollout().ID},
		GrantedBy: actorScheduler, GrantedAt: p.f.now,
	}
	p.snap.leases = append(p.snap.leases, l)

	m := p.rollout().Host(host)
	m.State, m.LeaseID = fleet.HostLeased, l.ID

	return l
}

// done lets the agent finish the host's lease one second before the newest
// liveness epoch began, and sync once right after.
func (p *pinned) done(host string) *fleet.Lease {
	l := p.lease(host)
	h := p.f.hosts[host]
	h.disrupted[l.ID] = true
	h.doneAt[l.ID] = testGenesis.Add(pinEpoch*simEpoch - time.Second)
	h.lastSync = h.doneAt[l.ID].Add(time.Second)
	h.status.Needs = nil
	h.status.Containers = []protocol.Container{container("c", imgGeth, p.digest, p.digest, p.f.now)}

	return l
}

// verifying lets the agent finish the host's lease and the scheduler see the
// first sync after it in a committed reconcile.
func (p *pinned) verifying(host string) *fleet.Lease {
	p.t.Helper()

	l := p.done(host)
	l.State = fleet.LeaseVerifying
	p.reconciled()

	return l
}

// reconciled observes the state as one that a reconcile has just committed
// and requires the history so far to be accepted.
func (p *pinned) reconciled() {
	p.t.Helper()

	p.f.fresh = true
	err := p.observe()
	p.f.fresh = false

	require.NoError(p.t, err)
}

// serve walks the host through a lease to released and done.
func (p *pinned) serve(host string) {
	p.t.Helper()
	p.grant(host)
	p.ok()
	p.release(host)
	p.ok()
}

// release ends the host's lease as released, on a sync an epoch after the
// first one that showed its node back.
func (p *pinned) release(host string) {
	p.t.Helper()

	l := p.verifying(host)
	p.f.hosts[host].lastSync = p.f.now
	l.State = fleet.LeaseReleased
	p.rollout().Host(host).State = fleet.HostDone
}

// fail ends the host's lease as failed; halt says whether the scheduler
// halts the rollout as it must.
func (p *pinned) fail(host string, halt bool) {
	l := p.lease(host)
	l.State = fleet.LeaseFailed
	l.Operation = &protocol.Operation{LeaseID: l.ID, Phase: protocol.PhaseFailed, Disrupted: true}
	p.f.hosts[host].disrupted[l.ID] = true
	p.rollout().Host(host).State = fleet.HostFailed

	if halt {
		p.rollout().State = fleet.RolloutHalted
	}
}

func TestOracleAcceptsAGoodHistory(t *testing.T) {
	p := newPinned(t)

	// The canary alone: a host with validators. Then the rest, two at a time
	// within 40%: hosts with and without validators and the bootnode alike.
	p.grant("v1")
	p.ok()
	p.release("v1")
	p.grant("v2")
	p.grant("n1")
	p.ok()
	p.release("v2")
	p.release("n1")
	p.ok()
	p.grant("b1")
	p.grant("v3")
	p.ok()
	p.release("b1")
	p.release("v3")
	p.ok()
	p.grant("v4")
	p.ok()
	p.release("v4")
	p.rollout().State = fleet.RolloutCompleted
	p.ok()
}

func TestOracleAcceptsACanaryWithoutValidators(t *testing.T) {
	// No host with validators can be served: one is held, the others were
	// skipped. The canary is then a host without.
	p := newPinned(t)
	p.f.hosts["v1"].holdWanted = true

	for _, name := range []string{"v2", "v3", "v4"} {
		p.rollout().Host(name).State = fleet.HostSkipped
	}

	p.serve("b1")
	p.serve("n1")
}

func TestOracleAcceptsACanaryThatFitsTheBudget(t *testing.T) {
	// No validator may go down: the canary is a host that costs nothing, and
	// the other one follows.
	p := newPinned(t)
	p.f.settings.MaxUnavailablePercent = 0

	p.serve("b1")
	p.serve("n1")
}

func TestOracleAcceptsASingleMemberWithoutCanary(t *testing.T) {
	p := newPinned(t)

	for _, name := range []string{"n1", "v2", "v3", "v4", "b1"} {
		p.rollout().Host(name).State = fleet.HostSkipped
	}

	p.grant("v1")
	p.ok()
}

func TestOracleAcceptsCarriedHostsFirst(t *testing.T) {
	// n1 and v2 failed in the rollout that the new one supersedes: both are
	// granted before anybody else and without a canary. Their repair is the
	// first success, so the rest follows without one too.
	p := newPinned(t)
	p.successor("n1", "v2")

	p.grant("n1")
	p.grant("v2")
	p.ok()
	p.release("n1")
	p.release("v2")
	p.ok()
	p.grant("v3")
	p.grant("b1")
	p.ok()
}

func TestOracleAcceptsARepairThroughTheClosedGate(t *testing.T) {
	// v2 has been offline for two epochs while the canary is out: the gate is
	// closed (80 of the 90 validators without a lease attest) and lets its
	// repair pass, next to the canary and whatever the order.
	p := newPinned(t)
	p.grant("v1")
	p.ok()
	p.offline("v2")
	p.grant("v2")
	p.ok()

	// Nobody else is stuck meanwhile: they wait for the gate.
	for range 2 * simStuckPasses {
		p.reconciled()
	}
}

func TestOracleAcceptsHostsThatWaitForTheOrder(t *testing.T) {
	// The canary is out: everybody else waits, for as long as it takes.
	p := newPinned(t)
	p.grant("v1")
	p.ok()

	for range 2 * simStuckPasses {
		p.reconciled()
	}

	// A carried host that is out holds the others back as well.
	q := newPinned(t)
	q.successor("v2")
	q.grant("v2")
	q.ok()

	for range 2 * simStuckPasses {
		q.reconciled()
	}
}

func TestOracleAcceptsAReadinessFailureAfterTheTimeout(t *testing.T) {
	p := newPinned(t)
	p.grant("v1")
	p.ok()

	l := p.done("v1")
	p.f.hosts["v1"].doneAt[l.ID] = p.f.now.Add(-31 * time.Minute)
	p.f.since = p.f.now.Add(-2 * time.Hour)
	p.fail("v1", true)
	l.Error = "not ready after 30m0s: validators attesting: 0 of 1 epochs with at least 9 of 10"
	p.ok()
}

func TestOracleAcceptsAGrantAfterARetry(t *testing.T) {
	p := newPinned(t)

	p.grant("v1")
	p.ok()
	p.fail("v1", true)
	p.ok()

	// A person retries: the rollout is active again and v1 may be granted.
	p.rollout().State = fleet.RolloutActive
	p.rollout().Host("v1").State = fleet.HostPending
	p.f.retried = []string{pinRollout}
	p.ok()
	p.grant("v1")
	p.ok()
}

func TestOracleAcceptsAReopenedLease(t *testing.T) {
	// The agent started a lease after the scheduler had expired it: the
	// lease is taken up again, and nobody else may be granted on top.
	p := newPinned(t)
	l := p.grant("v1")
	p.ok()

	l.State = fleet.LeaseExpired
	p.rollout().Host("v1").State = fleet.HostPending
	p.ok()

	p.f.hosts["v1"].disrupted[l.ID] = true
	l.State = fleet.LeaseRunning
	p.rollout().Host("v1").State = fleet.HostLeased
	p.ok()

	p.release("v1")
	p.ok()
}

// withoutLiveness makes the pinned beacon node one that does not serve the
// liveness endpoint: nothing was ever published.
func (p *pinned) withoutLiveness() {
	p.f.health.LivenessKnown = false
	p.f.health.LivenessUnsupported = true
	p.f.rows = map[uint64]map[string]uint64{}
	p.f.havePublished = false
}

func TestOracleAcceptsAHistoryWithoutLiveness(t *testing.T) {
	// No liveness to be had: the gate does not ask for it, and validator
	// hosts are released on their probes alone.
	p := newPinned(t)
	p.withoutLiveness()

	p.serve("v1")
	p.grant("v2")
	p.grant("n1")
	p.ok()
	p.release("v2")
	p.release("n1")
	p.ok()
}

func TestOracleAcceptsASilentLeaseLongAfterTheStart(t *testing.T) {
	p := newPinned(t)
	p.grant("v1")
	p.ok()

	// The controller started an hour ago and has not heard from the agent
	// since: longer than leaseTtl.
	p.f.since = p.f.now.Add(-time.Hour)
	p.f.hosts["v1"].lastSync = p.f.now.Add(-2 * time.Hour)
	p.fail("v1", true)
	p.lease0().Error = reasonAgentSilent
	p.ok()
}

// lease0 is the first lease of the history, whatever its state.
func (p *pinned) lease0() *fleet.Lease { return p.snap.leases[0] }

func TestOracleRejectsBadHistories(t *testing.T) {
	// canaryDone is a history up to the point where the rest may start.
	canaryDone := func(p *pinned) {
		p.serve("v1")
		p.ok()
	}

	tests := []struct {
		name string
		bad  func(p *pinned)
		want string
	}{
		{
			name: "over-budget grant",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("v2")
				p.ok()
				// 10 unavailable plus 70 is far over the 40 allowed.
				p.grant("v4")
			},
			want: "over budget: cost 70 with 10 of 100 already unavailable",
		},
		{
			name: "over-budget grant because validators are down by themselves",
			bad: func(p *pinned) {
				canaryDone(p)
				p.f.hosts["v4"].live = 30
				p.f.settings.MinParticipationPercent = 0
				p.grant("v2")
			},
			want: "over budget: cost 10 with 40 of 100 already unavailable",
		},
		{
			name: "third lease over the host limit",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("v2")
				p.grant("v3")
				p.ok()
				// A host that is down already costs nothing, but the limit
				// of two leases still holds.
				p.f.hosts["v4"].live = 0
				p.f.settings.MinParticipationPercent = 0
				p.grant("v4")
			},
			want: "host limit: 2 hosts already hold a lease, at most 2 of 6 may",
		},
		{
			name: "ordering violation: a host of the rest while the canary is out",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.grant("n1")
			},
			want: "granted before the first success of the rollout while v1 holds a lease",
		},
		{
			name: "ordering violation: a host of the rest while the canary is being verified",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.verifying("v1")
				p.grant("v2")
			},
			want: "granted before the first success of the rollout while v1 holds a lease",
		},
		{
			name: "ordering violation: a canary without validators although a validator host waits",
			bad:  func(p *pinned) { p.grant("n1") },
			want: "canary n1 although v1 comes first",
		},
		{
			name: "ordering violation: a canary that is not the lightest host with validators",
			bad:  func(p *pinned) { p.grant("v4") },
			want: "canary v4 although v1 comes first",
		},
		{
			name: "ordering violation: a canary that does not fit the budget although a host that costs nothing waits",
			bad: func(p *pinned) {
				p.f.settings.MaxUnavailablePercent = 0
				p.grant("v1")
			},
			want: "canary v1 although n1 comes first",
		},
		{
			name: "ordering violation: a canary whose validators are all offline although a live one waits",
			bad: func(p *pinned) {
				p.f.settings.MinParticipationPercent = 0
				p.offline("v1")
				p.grant("v1")
			},
			want: "canary v1 although n1 comes first",
		},
		{
			name: "ordering violation: a host while a carried host waits",
			bad: func(p *pinned) {
				p.successor("n1")
				p.grant("v1")
			},
			want: "granted although the carried host n1 (pending) is still to be served",
		},
		{
			name: "ordering violation: a host while a carried host is expected to report",
			bad: func(p *pinned) {
				r := p.successor("n1")
				r.CreatedAt = p.f.now
				r.Host("n1").State = fleet.HostExpected
				p.f.hosts["n1"].status.Needs = nil
				p.grant("v1")
			},
			want: "granted although the carried host n1 (expected) is still to be served",
		},
		{
			name: "ordering violation: a host while a carried host is being repaired",
			bad: func(p *pinned) {
				p.successor("v2")
				p.grant("v2")
				p.ok()
				p.grant("v1")
			},
			want: "granted although the carried host v2 (leased) is still to be served",
		},
		{
			name: "a carried mark that is lost",
			bad: func(p *pinned) {
				r := p.successor("n1")
				r.Host("n1").CarriedFrom = ""
			},
			want: "member n1 of rollout r2 (image:" + imgGeth + "@" + digestB + ") is marked carried: false",
		},
		{
			name: "a carried mark that nothing in the history of the rollouts gives",
			bad:  func(p *pinned) { p.rollout().Host("v2").CarriedFrom = "r0" },
			want: "is marked carried: true",
		},
		{
			name: "first success: the rest after a canary whose lease failed on another step",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()

				// The image step is done, another step of the lease failed: the
				// rollout is not halted, v1 is done, but nothing proved it ready.
				l.Operation = &protocol.Operation{
					LeaseID: l.ID, Phase: protocol.PhaseFailed, Disrupted: true,
					Steps: []protocol.Step{
						{NeedID: l.Needs[0].ID, Status: protocol.StepDone},
						{NeedID: "reboot", Status: protocol.StepFailed},
					},
				}
				l.State = fleet.LeaseFailed
				p.f.hosts["v1"].disrupted[l.ID] = true
				p.rollout().Host("v1").State = fleet.HostDone
				p.ok()
				p.grant("v2")
				p.grant("v3")
			},
			want: "granted before the first success of the rollout while v3 holds a lease",
		},
		{
			name: "first success: the rest after a canary that was done without a lease",
			bad: func(p *pinned) {
				p.rollout().Host("v1").State = fleet.HostDone
				p.f.hosts["v1"].status.Needs = nil
				p.ok()
				p.grant("v2")
				p.grant("v3")
			},
			want: "granted before the first success of the rollout while v3 holds a lease",
		},
		{
			name: "repair: a second one while the first is open",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.offline("v2")
				p.offline("v3")
				p.grant("v2")
				p.ok()
				p.grant("v3")
			},
			want: "and another repair is open",
		},
		{
			name: "repair: two in one reconcile",
			bad: func(p *pinned) {
				p.offline("v2")
				p.offline("v3")
				p.grant("v2")
				p.grant("v3")
			},
			want: "and another repair is open",
		},
		{
			name: "repair: a host that missed one epoch",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.offline("v2")
				p.f.rows[pinEpoch-1]["v2"] = 10
				p.grant("v2")
			},
			want: "the host is not established as offline",
		},
		{
			name: "repair: a host whose lease has just ended",
			bad: func(p *pinned) {
				// v1's own lease expired unused a moment ago; the epochs that
				// show it offline began before that.
				l := p.grant("v1")
				p.ok()
				l.State = fleet.LeaseExpired
				p.rollout().Host("v1").State = fleet.HostPending
				p.ok()
				p.f.settings.MinParticipationPercent = 95
				p.offline("v1")
				p.grant("v1")
			},
			want: "the host is not established as offline",
		},
		{
			name: "repair: a host the database has only just seen",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.offline("v2")
				p.f.registered = map[string]time.Time{"v2": p.f.now.Add(-time.Second)}
				p.grant("v2")
			},
			want: "the host is not established as offline",
		},
		{
			name: "readiness failure before the timeout",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()

				l := p.done("v1")
				p.fail("v1", true)
				l.Error = "not ready after 30m0s: node is syncing"
			},
			want: `failed as "not ready after 30m0s: node is syncing" 54s after the later of the end of its operation`,
		},
		{
			name: "readiness failure on the first reconcile after a controller outage",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()

				// Done two hours ago; the controller came back a minute ago.
				l := p.done("v1")
				p.f.hosts["v1"].doneAt[l.ID] = p.f.now.Add(-2 * time.Hour)
				p.f.since = p.f.now.Add(-time.Minute)
				p.fail("v1", true)
				l.Error = "not ready after 30m0s: waiting for attestation data from the beacon node"
			},
			want: "1m0s after the later of the end of its operation and the controller's start",
		},
		{
			name: "no progress: nobody is granted although nothing holds the canary back",
			bad: func(p *pinned) {
				for range simStuckPasses - 1 {
					p.reconciled()
				}

				p.f.fresh = true
			},
			want: "is stuck in rollout r1",
		},
		{
			name: "no progress: a host that is established as offline waits for the canary behind the closed gate",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				// Offline for two epochs: the closed gate lets its repair pass.
				p.offline("v2")

				for range simStuckPasses - 1 {
					p.reconciled()
				}

				p.f.fresh = true
			},
			want: "host v2 is stuck in rollout r1",
		},
		{
			name: "no progress: hosts that cost nothing wait for a canary that does not fit the budget",
			bad: func(p *pinned) {
				p.f.settings.MaxUnavailablePercent = 0

				for range simStuckPasses - 1 {
					p.reconciled()
				}

				p.f.fresh = true
			},
			want: "host n1 is stuck in rollout r1",
		},
		{
			name: "grant during a halt",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.fail("v1", true)
				p.ok()
				// Nobody retried, yet the rollout runs again and serves v2.
				p.rollout().State = fleet.RolloutActive
				p.rollout().Host("v1").State = fleet.HostDone
				p.grant("v2")
			},
			want: "although lease l1 failed in it and nobody retried it",
		},
		{
			name: "failed lease that does not halt its rollout",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.fail("v1", false)
			},
			want: "rollout r1 is active although lease l1 failed in it",
		},
		{
			name: "grant to a held host",
			bad: func(p *pinned) {
				p.f.hosts["v1"].holdWanted = true
				p.grant("v1")
			},
			want: "the host is held",
		},
		{
			name: "grant to a stale host",
			bad: func(p *pinned) {
				p.f.hosts["v1"].lastSync = p.f.now.Add(-time.Hour)
				p.grant("v1")
			},
			want: "the host is stale",
		},
		{
			name: "grant for a verb the agent does not allow",
			bad: func(p *pinned) {
				p.f.hosts["v1"].status.Verbs = []protocol.Verb{protocol.VerbReboot}
				p.grant("v1")
			},
			want: "wants verb roll, which the agent does not allow",
		},
		{
			name: "grant of a need the agent does not report",
			bad: func(p *pinned) {
				p.f.hosts["v1"].status.Needs = nil
				p.grant("v1")
			},
			want: "is not one the agent reports",
		},
		{
			name: "grant in a paused rollout",
			bad: func(p *pinned) {
				p.rollout().Paused = true
				p.grant("v1")
			},
			want: "the rollout is paused",
		},
		{
			name: "grant under the global pause",
			bad: func(p *pinned) {
				p.f.settings.Paused = true
				p.grant("v1")
			},
			want: "the global pause is in effect",
		},
		{
			name: "grant in an unapproved rollout",
			bad: func(p *pinned) {
				p.f.settings.PolicyImage = fleet.PolicyManual
				p.grant("v1")
			},
			want: "nobody approved the rollout",
		},
		{
			name: "grant while the gate fails",
			bad: func(p *pinned) {
				p.f.health.FinalityLagEpochs = 9
				p.grant("v1")
			},
			want: "the network gate fails ([finality lag]) and the host is not established as offline",
		},
		{
			name: "grant in a closed rollout",
			bad: func(p *pinned) {
				p.rollout().State = fleet.RolloutAborted
				p.grant("v1")
			},
			want: "the rollout is aborted",
		},
		{
			name: "second lease for a host",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.grant("v1")
			},
			want: "host v1 holds two leases",
		},
		{
			name: "closed rollout that reopens",
			bad: func(p *pinned) {
				p.rollout().State = fleet.RolloutAborted
				p.ok()
				p.rollout().State = fleet.RolloutActive
			},
			want: "rollout r1 left terminal state aborted for active",
		},
		{
			name: "two open rollouts for one key",
			bad: func(p *pinned) {
				second := cloneRollout(p.rollout())
				second.ID = "r2"
				p.snap.rollouts = append(p.snap.rollouts, second)
			},
			want: "has two open rollouts: r1 and r2",
		},
		{
			name: "release before the validators attested for a complete epoch",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.release("v1")
				p.f.hosts["v1"].doneAt["l1"] = p.f.now
			},
			want: "released after 0 of 1 complete epochs since the agent was done",
		},
		{
			name: "release although the validators stay down",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.release("v1")
				p.f.rows[pinEpoch]["v1"] = 3
			},
			want: "released although 3 of 10 validators attested in epoch 99 (need 9)",
		},
		{
			name: "release with fewer validators attesting than the bar, rounded up",
			bad: func(p *pinned) {
				// 85% of 10 validators is 8.5: nine must attest.
				p.f.settings.MinParticipationPercent = 85
				p.grant("v1")
				p.ok()
				p.release("v1")
				p.f.rows[pinEpoch]["v1"] = 8
			},
			want: "released although 8 of 10 validators attested in epoch 99 (need 9)",
		},
		{
			name: "expired lease that runs again although the agent never started it",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()
				l.State = fleet.LeaseExpired
				p.rollout().Host("v1").State = fleet.HostPending
				p.ok()
				l.State = fleet.LeaseRunning
				p.rollout().Host("v1").State = fleet.HostLeased
			},
			want: "left terminal state expired for running",
		},
		{
			name: "released lease that runs again",
			bad: func(p *pinned) {
				p.serve("v1")
				p.snap.leases[0].State = fleet.LeaseRunning
			},
			want: "left terminal state released for running",
		},
		{
			name: "grant to a host a person skipped for the cause",
			bad: func(p *pinned) {
				p.f.skipped = map[string]map[string]bool{imageKey(imgGeth, digestA): {"v1": true}}
				p.grant("v1")
			},
			want: "a person skipped the host for image:" + imgGeth + "@" + digestA + " and nobody retried it",
		},
		{
			name: "release although the agent still reports the need it served",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()
				p.release("v1")

				need := imageNeed("c", imgGeth, digestA)
				l.Operation = &protocol.Operation{
					LeaseID: l.ID, Phase: protocol.PhaseDone, Disrupted: true,
					Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepDone}},
				}
				p.f.hosts["v1"].status.Needs = []protocol.Need{need}
			},
			want: "released although the agent still reports need " + imageNeed("c", imgGeth, digestA).ID,
		},
		{
			name: "lease failed as silent although the controller has just started",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()

				// The agent's last sync is old, but the controller was down
				// until ten minutes ago: less than leaseTtl to be heard.
				p.f.since = p.f.now.Add(-10 * time.Minute)
				p.f.hosts["v1"].lastSync = p.f.now.Add(-2 * time.Hour)
				p.fail("v1", true)
				p.lease0().Error = reasonAgentSilent
			},
			want: `failed as "agent silent" after 10m0s of silence`,
		},
		{
			name: "grant while liveness is merely unknown",
			bad: func(p *pinned) {
				p.f.health.LivenessKnown = false
				p.grant("v1")
			},
			want: "liveness data stale",
		},
		{
			name: "release of a validator host while liveness is merely unknown",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.release("v1")
				p.f.health.LivenessKnown = false
			},
			want: "released although the beacon node has not said whether its 10 validators attest",
		},
		{
			name: "release of a validator host on bad probes when liveness is not served",
			bad: func(p *pinned) {
				p.withoutLiveness()
				p.grant("v1")
				p.ok()
				p.f.hosts["v1"].status.Node = protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Syncing: true}}
				p.release("v1")
			},
			want: "released although its node is not seen at head with 3 peers",
		},
		{
			name: "release of a validator host on bad probes although its validators attest",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.f.hosts["v1"].status.Node = protocol.NodeHealth{
					EL: &protocol.ELHealth{Reachable: true, Peers: 9},
					CL: &protocol.CLHealth{Reachable: true, ELOffline: true, Peers: 9},
				}
				p.release("v1")
			},
			want: "released although its node is not seen at head with 3 peers",
		},
		{
			name: "release of a host whose node has too few peers",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("n1")
				p.ok()
				p.f.hosts["n1"].status.Node = protocol.NodeHealth{
					EL: &protocol.ELHealth{Reachable: true, Peers: 9},
					CL: &protocol.CLHealth{Reachable: true, Peers: 2},
				}
				p.release("n1")
			},
			want: "released although its node is not seen at head with 3 peers",
		},
		{
			name: "release on the sync that reported done",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("n1")
				p.ok()
				p.release("n1")
				p.f.hosts["n1"].lastSync = p.f.hosts["n1"].doneAt["l2"]
			},
			want: "released although its node is not seen at head with 3 peers after the operation",
		},
		{
			name: "release before the node was back for readyEpochs",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("n1")
				p.ok()

				// The first sync after the operation came ten seconds ago.
				l := p.done("n1")
				p.f.hosts["n1"].lastSync = p.f.now.Add(-10 * time.Second)
				l.State = fleet.LeaseVerifying
				p.reconciled()
				p.f.hosts["n1"].lastSync = p.f.now
				l.State = fleet.LeaseReleased
				p.rollout().Host("n1").State = fleet.HostDone
			},
			want: "that spans 10s, it must span 48s",
		},
		{
			name: "release although a sync in between showed the node without its peers",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("n1")
				p.ok()

				// Back with peers, then a sync with too few, then back again
				// five seconds ago: the run started over.
				l := p.verifying("n1")
				h := p.f.hosts["n1"]
				h.lastSync = p.f.now.Add(-20 * time.Second)
				h.status.Node = nodeWith(9, 1)
				p.reconciled()
				h.lastSync = p.f.now.Add(-5 * time.Second)
				h.status.Node = nodeWith(9, 9)
				p.reconciled()
				h.lastSync = p.f.now
				l.State = fleet.LeaseReleased
				p.rollout().Host("n1").State = fleet.HostDone
			},
			want: "that spans 5s, it must span 48s",
		},
		{
			name: "release on one healthy sync and an epoch without any",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("n1")
				p.ok()

				// The only sync after the operation came 53 seconds ago: more
				// than an epoch, less than hostStaleAfter.
				l := p.verifying("n1")
				l.State = fleet.LeaseReleased
				p.rollout().Host("n1").State = fleet.HostDone
			},
			want: "that spans 0s, it must span 48s",
		},
		{
			name: "release although the agent went silent",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("n1")
				p.ok()
				p.release("n1")

				// Its last sync was healthy, and three minutes ago.
				p.f.hosts["n1"].doneAt["l2"] = p.f.now.Add(-4 * time.Minute)
				p.f.hosts["n1"].lastSync = p.f.now.Add(-3 * time.Minute)
			},
			want: "released although its agent is silent: last sync 3m0s ago",
		},
		{
			name: "controller hold lifted although a person holds the host",
			bad: func(p *pinned) {
				p.f.holds = map[string]bool{"v1": true}
				p.snap.directives = append(p.snap.directives, &fleet.Directive{
					Directive: protocol.Directive{ID: "d1", Kind: protocol.DirectiveReleaseHold},
					Host:      "v1", State: fleet.DirectivePending,
				})
			},
			want: "lifts the controller hold of v1",
		},
		{
			name: "lease expired although the agent stopped containers",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()
				p.f.hosts["v1"].disrupted[l.ID] = true
				l.State = fleet.LeaseExpired
			},
			want: "expired as if it cost nothing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPinned(t)
			tt.bad(p)

			err := p.observe()
			require.Error(t, err, "the oracle accepted a history that breaks a rule")
			require.Contains(t, err.Error(), tt.want)
		})
	}
}
