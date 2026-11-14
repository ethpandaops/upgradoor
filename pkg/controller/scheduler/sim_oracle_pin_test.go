package scheduler

import (
	"cmp"
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
				Phases:       fleet.SinglePhase(testGenesis, simSecondsPerSlot*time.Second),
				CurrentEpoch: pinEpoch + 1, FinalizedEpoch: pinEpoch - 1, FinalityLagEpochs: 2,
				LivenessKnown: true, LivenessEpoch: pinEpoch,
			},
			hosts:          make(map[string]*simHostFact, 6),
			epochStart:     func(e uint64) time.Time { return testGenesis.Add(time.Duration(e) * simEpoch) },
			epochOf:        func(t time.Time) uint64 { return uint64(t.Sub(testGenesis) / simEpoch) },
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

// breakIn lets the named hosts fail in the open rollout, one lease after the
// other, each as the canary of the hosts that are left: everybody else is
// skipped first, and a person reopens the rollout after every failure but
// the last without retrying the host.
func (p *pinned) breakIn(hosts ...string) {
	p.t.Helper()

	r := p.rollout()

	for i := range r.Hosts {
		if m := &r.Hosts[i]; !slices.Contains(hosts, m.Host) && m.State == fleet.HostPending {
			m.State = fleet.HostSkipped
		}
	}

	// The canary is a host with validators before one without.
	order := slices.Clone(hosts)
	slices.SortStableFunc(order, func(a, b string) int {
		return cmp.Compare(min(p.f.hosts[b].weight, 1), min(p.f.hosts[a].weight, 1))
	})

	for i, host := range order {
		if i > 0 {
			r.State = fleet.RolloutActive
			p.f.retried = []string{r.ID}
			p.ok()
		}

		p.grant(host)
		p.ok()
		p.fail(host, true)
		p.ok()
	}
}

// broken gives a waiting member the stage and the note of a host that is
// broken, as the scheduler does: by the last lease of the host that failed.
func (p *pinned) broken(r *fleet.Rollout, host string) {
	p.t.Helper()

	for _, l := range slices.Backward(p.snap.leases) {
		if l.Host != host || l.State != fleet.LeaseFailed {
			continue
		}

		m := r.Host(host)
		m.Stage = fleet.StageRepair
		m.Error = oracleBrokenNote + l.ID + " (rollout " + l.Rollouts[l.Needs[0].ID] + ")"

		return
	}

	p.t.Fatalf("no lease of %s has failed", host)
}

// successor moves the tag to digest B: the rollout so far is superseded with
// the named hosts failed in it (by a lease each), and the scheduler opens the
// next one, in which they are in the repair stage.
func (p *pinned) successor(failed ...string) *fleet.Rollout {
	p.t.Helper()

	next := p.follow(failed...)
	p.reconciled()

	return next
}

// follow is successor without the look at the result: the reconcile that
// opens the next rollout has written its rows. A rollout that a person closed
// before stays as it is.
func (p *pinned) follow(failed ...string) *fleet.Rollout {
	p.t.Helper()

	old := p.rollout()

	unbroken := slices.DeleteFunc(slices.Clone(failed), func(host string) bool {
		return old.Host(host).State == fleet.HostFailed
	})
	if len(unbroken) > 0 {
		p.breakIn(unbroken...)
	}

	if !old.State.Terminal() {
		old.State = fleet.RolloutSuperseded
	}

	next := &fleet.Rollout{
		ID: fmt.Sprintf("r%d", len(p.snap.rollouts)+1), Kind: fleet.RolloutImage, Key: imageKey(imgGeth, digestB),
		Image: imgGeth, Digest: digestB, State: fleet.RolloutActive,
		CreatedAt: p.f.now.Add(-time.Minute),
	}

	for i := range old.Hosts {
		m := &old.Hosts[i]
		member := fleet.RolloutHost{Host: m.Host, State: fleet.HostPending, Stage: fleet.StageRest}

		next.Hosts = append(next.Hosts, member)

		st := p.f.hosts[m.Host].status
		st.Containers = []protocol.Container{container("c", imgGeth, digestOld, digestB, p.f.now)}
		st.Needs = []protocol.Need{imageNeed("c", imgGeth, digestB)}
	}

	p.snap.rollouts = append(p.snap.rollouts, next)
	p.digest = digestB

	for _, host := range failed {
		p.broken(next, host)
	}

	return next
}

// abortAndRetry lets a person abort the open rollout and retry it at once:
// the rollout is aborted, and the action opens a new one for the same cause.
func (p *pinned) abortAndRetry() *fleet.Rollout {
	p.t.Helper()

	old := p.rollout()
	old.State = fleet.RolloutAborted
	p.ok()

	next := &fleet.Rollout{
		ID: fmt.Sprintf("r%d", len(p.snap.rollouts)+1), Kind: old.Kind, Key: old.Key,
		Image: old.Image, Digest: old.Digest, State: fleet.RolloutActive,
		CreatedBy: simActor, CreatedAt: p.f.now.Add(-time.Second),
	}

	for i := range old.Hosts {
		next.Hosts = append(next.Hosts, fleet.RolloutHost{
			Host: old.Hosts[i].Host, State: fleet.HostPending, Stage: fleet.StageRest,
		})
	}

	p.snap.rollouts = append(p.snap.rollouts, next)
	p.ok()

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

// repair grants the host a lease through the closed gate, marked as the
// scheduler marks it.
func (p *pinned) repair(host string) *fleet.Lease {
	l := p.grant(host)
	l.Repair = true

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

	if l.Operation == nil {
		l.Operation = &protocol.Operation{
			LeaseID: l.ID, Phase: protocol.PhaseDone, Disrupted: true,
			Steps: []protocol.Step{{NeedID: l.Needs[0].ID, Status: protocol.StepDone}},
		}
	}

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

func TestOracleAcceptsBrokenHostsFirst(t *testing.T) {
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

	// Repaired, they are in order: for the next digest nobody goes first, and
	// the canary is the one it would be anyway.
	p.release("v3")
	p.release("b1")
	p.ok()
	p.successor()
	p.grant("v1")
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
	p.repair("v2")
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

	// A broken host that is out holds the others back as well.
	q := newPinned(t)
	q.successor("v2")
	q.grant("v2")
	q.ok()

	for range 2 * simStuckPasses {
		q.reconciled()
	}
}

func TestOracleAcceptsASuccessorOfARolloutWithAnOpenLease(t *testing.T) {
	// v1 is out for the rollout of digest A when the tag moves. The successor
	// grants nobody while that lease is open. It fails: v1 is broken and
	// goes first.
	p := newPinned(t)
	l := p.grant("v1")
	p.ok()
	next := p.successor()

	for range 2 * simStuckPasses {
		p.reconciled()
	}

	l.State = fleet.LeaseFailed
	p.f.hosts["v1"].disrupted[l.ID] = true
	p.snap.rollouts[0].Host("v1").State = fleet.HostFailed
	p.broken(next, "v1")
	p.reconciled()

	p.grant("v1")
	p.ok()

	// Released ready instead, the host is in order and nobody goes first.
	q := newPinned(t)
	q.grant("v1")
	q.ok()
	q.successor()
	q.snap.leases[0].State = fleet.LeaseReleased
	q.snap.leases[0].Operation = &protocol.Operation{
		LeaseID: "l1", Phase: protocol.PhaseDone, Disrupted: true,
		Steps: []protocol.Step{{NeedID: q.snap.leases[0].Needs[0].ID, Status: protocol.StepDone}},
	}
	q.f.hosts["v1"].disrupted["l1"] = true
	q.f.hosts["v1"].doneAt["l1"] = q.f.now.Add(-2 * time.Minute)
	q.snap.rollouts[0].Host("v1").State = fleet.HostDone
	q.o.runs["l1"] = oracleRun{
		first: q.f.now.Add(-time.Minute), newest: q.f.now,
		syncs: []time.Time{q.f.now.Add(-time.Minute), q.f.now},
	}
	q.reconciled()
	q.grant("v1")
	q.ok()
}

func TestOracleAcceptsARetryNextToALeaseInFlight(t *testing.T) {
	// v1 is out for the rollout of digest A when the tag moves. A person
	// aborts the successor and retries it: the rollout that the retry opens
	// waits for the lease like the one before it, and has v1 first when the
	// lease fails.
	p := newPinned(t)
	l := p.grant("v1")
	p.ok()
	p.successor()

	next := p.abortAndRetry()

	for range 2 * simStuckPasses {
		p.reconciled()
	}

	l.State = fleet.LeaseFailed
	p.f.hosts["v1"].disrupted[l.ID] = true
	p.snap.rollouts[0].Host("v1").State = fleet.HostFailed
	p.broken(next, "v1")
	p.reconciled()

	p.grant("v1")
	p.ok()

	// A host that failed in the aborted rollout itself goes first as well,
	// in the rollout of the retry.
	q := newPinned(t)
	q.grant("v1")
	q.ok()
	q.fail("v1", true)
	q.ok()

	reopened := q.abortAndRetry()
	q.broken(reopened, "v1")
	q.reconciled()
	q.grant("v1")
	q.ok()

	// And into the rollout of the next digest.
	r := newPinned(t)
	r.grant("v1")
	r.ok()
	r.fail("v1", true)
	r.ok()
	r.rollout().State = fleet.RolloutAborted
	r.ok()
	r.successor("v1")
	r.grant("v1")
	r.ok()
}

func TestOracleAcceptsALeaseReopenedAfterItsRolloutWasSuperseded(t *testing.T) {
	// The lease of v1 expires, the tag moves, and then the agent turns out to
	// have started: the successor grants nobody while that lease is open
	// again, and has v1 first when it fails.
	p := newPinned(t)
	l := p.grant("v1")
	p.ok()

	l.State = fleet.LeaseExpired
	p.rollout().Host("v1").State = fleet.HostPending
	p.rollout().Host("v1").LeaseID = ""
	p.ok()

	next := p.successor()

	p.f.hosts["v1"].disrupted[l.ID] = true
	l.State = fleet.LeaseRunning
	p.snap.rollouts[0].Host("v1").State = fleet.HostLeased
	p.snap.rollouts[0].Host("v1").LeaseID = l.ID

	for range 2 * simStuckPasses {
		p.reconciled()
	}

	l.State = fleet.LeaseFailed
	p.snap.rollouts[0].Host("v1").State = fleet.HostFailed
	p.broken(next, "v1")
	p.reconciled()

	p.grant("v1")
	p.ok()
}

func TestOracleAcceptsRowsOfAnEarlierBuild(t *testing.T) {
	// The rollout of digest A was aborted with v1 out; its lease failed after
	// that. In rows of an earlier build v1 is still leased there, by a lease
	// that is over. The row says nothing either way: the lease failed, so v1
	// goes first in the next rollout.
	p := newPinned(t)
	l := p.grant("v1")
	p.ok()
	p.rollout().State = fleet.RolloutAborted
	p.ok()

	l.State = fleet.LeaseFailed
	p.f.hosts["v1"].disrupted[l.ID] = true
	p.ok()

	next := p.follow()
	p.broken(next, "v1")
	p.reconciled()
	p.grant("v1")
	p.ok()
}

func TestOracleAcceptsALeaseEndedByAPerson(t *testing.T) {
	// v1 is out for the rollout of digest A, which a person aborts. The next
	// rollout waits for that lease, for as long as the agent takes. A person
	// ends the lease: v1 is broken and goes first, and nobody waits for a
	// lease any more.
	p := newPinned(t)
	l := p.grant("v1")
	p.ok()
	p.rollout().State = fleet.RolloutAborted
	p.ok()

	next := p.successor()

	for range 2 * simStuckPasses {
		p.reconciled()
	}

	l.State, l.Error = fleet.LeaseFailed, oracleEndedByOperator
	p.f.hosts["v1"].disrupted[l.ID] = true
	p.ok()

	p.broken(next, "v1")
	p.reconciled()
	p.grant("v1")
	p.ok()
	p.release("v1")
	p.ok()
	p.grant("v2")
	p.grant("v3")
	p.ok()
}

func TestOracleAcceptsLeasesThatSayNothing(t *testing.T) {
	// A lease that expired stopped nothing: its host is an ordinary member
	// of the next rollout, and the canary is chosen as ever.
	p := newPinned(t)
	l := p.grant("v1")
	p.ok()

	l.State = fleet.LeaseExpired
	p.rollout().Host("v1").State = fleet.HostPending
	p.rollout().Host("v1").LeaseID = ""
	p.ok()
	p.successor()
	p.grant("v1")
	p.ok()

	// A lease that failed on another step, with the step for the image done,
	// does not break the host for the image either.
	q := newPinned(t)
	// (Adopted, so that the lease may carry a step for something that is not
	// a rollout of this history.)
	l = q.grant("v1")
	other := rebootNeed()
	l.Needs = append(l.Needs, other)
	l.GrantedBy = grantedByAdoption
	q.ok()

	l.State = fleet.LeaseFailed
	l.Operation = &protocol.Operation{
		LeaseID: l.ID, Phase: protocol.PhaseFailed, Disrupted: true,
		Steps: []protocol.Step{
			{NeedID: l.Needs[0].ID, Status: protocol.StepDone},
			{NeedID: other.ID, Status: protocol.StepFailed},
		},
	}
	q.f.hosts["v1"].disrupted[l.ID] = true
	q.rollout().Host("v1").State = fleet.HostDone
	q.rollout().Host("v1").LeaseID = ""
	q.ok()
	q.successor()
	q.grant("v1")
	q.ok()
}

// genesis moves the start of the pinned chain.
func (p *pinned) genesis(at time.Time) {
	p.f.epochStart = func(e uint64) time.Time { return at.Add(time.Duration(e) * simEpoch) }
	p.f.epochOf = func(t time.Time) uint64 {
		if t.Before(at) {
			return 0
		}

		return uint64(t.Sub(at) / simEpoch)
	}
}

func TestOracleAcceptsARunFromBeforeGenesis(t *testing.T) {
	// The node came back a minute ago, ten seconds before the chain began:
	// 50 seconds of its first epoch are enough.
	p := newPinned(t)
	p.serve("v1")
	p.ok()
	p.grant("n1")
	p.ok()
	p.genesis(p.f.now.Add(-50 * time.Second))

	l := p.done("n1")
	p.f.hosts["n1"].doneAt[l.ID] = p.f.now.Add(-61 * time.Second)
	p.f.hosts["n1"].lastSync = p.f.now.Add(-time.Minute)
	l.State = fleet.LeaseVerifying
	p.reconciled()
	p.f.hosts["n1"].lastSync = p.f.now
	l.State = fleet.LeaseReleased
	p.rollout().Host("n1").State = fleet.HostDone
	p.ok()
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
			name: "ordering violation: a host while a broken host waits",
			bad: func(p *pinned) {
				p.successor("n1")
				p.grant("v1")
			},
			want: "granted although the broken host n1 (pending) is still to be served",
		},
		{
			name: "ordering violation: a host while a broken host is expected to report",
			bad: func(p *pinned) {
				r := p.successor("n1")
				r.CreatedAt = p.f.now
				r.Host("n1").State = fleet.HostExpected
				p.f.hosts["n1"].status.Needs = nil
				p.grant("v1")
			},
			want: "granted although the broken host n1 (expected) is still to be served",
		},
		{
			name: "ordering violation: a host while a broken host is being repaired",
			bad: func(p *pinned) {
				p.successor("v2")
				p.grant("v2")
				p.ok()
				p.grant("v1")
			},
			want: "granted although the broken host v2 (leased) is still to be served",
		},
		{
			name: "broken: a host whose last lease failed is not in the repair stage",
			bad: func(p *pinned) {
				r := p.successor("n1")
				r.Host("n1").Stage, r.Host("n1").Error = fleet.StageRest, ""
				p.f.fresh = true
			},
			want: "member n1 of rollout r2 (image:" + imgGeth + "@" + digestB + ") is in stage 3",
		},
		{
			name: "broken: a repair stage that no lease gives",
			bad: func(p *pinned) {
				p.rollout().Host("v2").Stage = fleet.StageRepair
				p.f.fresh = true
			},
			want: "is in stage 1 with the note \"\", but by the leases the oracle saw end its host is broken: false",
		},
		{
			name: "broken: a note that names another lease",
			bad: func(p *pinned) {
				r := p.successor("n1")
				r.Host("n1").Error = oracleBrokenNote + "l99"
				p.f.fresh = true
			},
			want: "carries the note \"broken since lease l99\"",
		},
		{
			name: "broken: a host that was repaired since is still in the repair stage",
			bad: func(p *pinned) {
				p.successor("v2")
				p.grant("v2")
				p.ok()
				p.release("v2")
				p.ok()

				// The tag moves again. The lease that broke v2 is still in the
				// table, and so is the one that repaired it.
				r := p.follow()
				p.broken(r, "v2")
				p.f.fresh = true
			},
			want: "member v2 of rollout r3 (image:" + imgGeth + "@" + digestB + ") is in stage 1",
		},
		{
			name: "broken: a host that was repaired since is granted before the canary",
			bad: func(p *pinned) {
				p.successor("v2")
				p.grant("v2")
				p.ok()
				p.release("v2")
				p.ok()
				p.successor()
				p.grant("v2")
			},
			want: "canary v2 although v1 comes first",
		},
		{
			name: "broken: a host whose lease expired goes first",
			bad: func(p *pinned) {
				l := p.grant("v2")
				l.State = fleet.LeaseExpired
				p.rollout().Host("v2").State = fleet.HostPending
				p.rollout().Host("v2").LeaseID = ""
				p.rollout().Host("v2").Stage = fleet.StageRepair
				p.f.fresh = true
			},
			want: "member v2 of rollout r1 (image:" + imgGeth + "@" + digestA + ") is in stage 1",
		},
		{
			name: "ended by a person: the host is not first afterwards",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()
				p.rollout().State = fleet.RolloutAborted
				p.ok()
				p.successor()

				l.State, l.Error = fleet.LeaseFailed, oracleEndedByOperator
				p.f.hosts["v1"].disrupted[l.ID] = true
				p.f.fresh = true
			},
			want: "member v1 of rollout r2 (image:" + imgGeth + "@" + digestB + ") is in stage 3",
		},
		{
			name: "in flight: a canary is chosen while a lease of the closed rollout is still open",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()

				r := p.follow()
				r.Canary = "v2"
				r.Host("v2").Stage = fleet.StageCanary
				p.f.fresh = true
			},
			want: "rollout r2 chose the canary v2 while lease l1 of a closed rollout is still open on v1",
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
				p.repair("v2")
				p.ok()
				p.repair("v3")
			},
			want: "and another repair is open",
		},
		{
			name: "repair: two in one reconcile",
			bad: func(p *pinned) {
				p.offline("v2")
				p.offline("v3")
				p.repair("v2")
				p.repair("v3")
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
			name: "gate: an ordinary grant next to a repair, as if the repaired host were not there",
			bad: func(p *pinned) {
				// 80 of 100 attest: closed at 85%. With the repaired host left
				// out it would read 80 of 90 and be open.
				p.f.settings.MinParticipationPercent = 85
				p.offline("v2")
				p.offline("v3")
				p.repair("v2")
				p.ok()
				p.grant("v1")
			},
			want: "the network gate fails ([participation])",
		},
		{
			name: "repair: a lease through the closed gate that is not marked",
			bad: func(p *pinned) {
				p.offline("v2")
				p.offline("v3")
				p.grant("v2")
			},
			want: "lease l1 of v2 is marked repair: false, but the oracle saw: true",
		},
		{
			name: "repair: a mark on a lease that passed no closed gate",
			bad:  func(p *pinned) { p.repair("v1") },
			want: "lease l1 of v1 is marked repair: true, but the oracle saw: false",
		},
		{
			name: "books: a member that waits and carries a lease ID",
			bad: func(p *pinned) {
				p.rollout().Host("v2").LeaseID = "l9"
				p.f.fresh = true
			},
			want: "member v2 of rollout r1 is pending and carries lease l9",
		},
		{
			name: "books: a member done through a lease whose step for the rollout was not done",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()
				l.Operation = &protocol.Operation{
					LeaseID: l.ID, Phase: protocol.PhaseDone, Disrupted: true,
					Steps: []protocol.Step{{NeedID: l.Needs[0].ID, Status: protocol.StepSuperseded}},
				}
				p.release("v1")
				p.f.fresh = true
			},
			want: "member v1 of rollout r1 is done through lease l1, which is released and did not finish every step",
		},
		{
			name: "books: a failed canary that is replaced while the rollout is halted",
			bad: func(p *pinned) {
				p.rollout().Canary = "v1"
				p.grant("v1")
				p.reconciled()
				p.fail("v1", true)
				p.rollout().Canary = "v2"
				p.f.fresh = true
			},
			want: "rollout r1 is halted by its canary v1, which is no longer its canary",
		},
		{
			name: "books: a host that becomes the canary while it holds a repair lease",
			bad: func(p *pinned) {
				p.rollout().Canary = "v1"
				p.grant("v1")
				p.reconciled()
				p.offline("v2")
				p.repair("v2")
				p.reconciled()
				p.rollout().Canary = "v2"
				p.f.fresh = true
			},
			want: "host v2 became the canary of rollout r1 while it holds the repair lease l2",
		},
		{
			name: "books: a lease that is still being checked after the readiness timeout",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.verifying("v1")
				p.f.now = p.f.now.Add(31 * time.Minute)
				p.f.health.UpdatedAt = p.f.now
				p.f.hosts["v1"].lastSync = p.f.now
				p.f.fresh = true
			},
			want: "lease l1 of v1 is still being checked for readiness 31m0s after the later of its first check",
		},
		{
			name: "supersede: a grant while a lease of the superseded rollout is still open",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.successor()
				p.grant("v2")
			},
			want: "granted although lease l1 of a closed rollout is still open on v1",
		},
		{
			name: "supersede: a host whose lease of the superseded rollout failed does not go first",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()
				p.successor()

				// The lease fails: by the oracle's record v1 is broken now.
				l.State = fleet.LeaseFailed
				p.f.hosts["v1"].disrupted[l.ID] = true
				p.f.fresh = true
			},
			want: "member v1 of rollout r2 (image:" + imgGeth + "@" + digestB + ") is in stage 3",
		},
		{
			name: "retry: a grant in the reopened rollout while a lease of the superseded rollout is still open",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.successor()
				p.abortAndRetry()
				p.grant("v2")
			},
			want: "rollout r3 (image:" + imgGeth + "@" + digestB + "): granted although lease l1 of a closed " +
				"rollout is still open on v1",
		},
		{
			name: "retry: a broken host that does not go first in the reopened rollout",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.fail("v1", true)
				p.ok()
				p.successor("v1")
				p.abortAndRetry()
				p.f.fresh = true
			},
			want: "member v1 of rollout r3 (image:" + imgGeth + "@" + digestB + ") is in stage 3",
		},
		{
			name: "abort: a host that failed in the aborted rollout does not go first after the retry",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.fail("v1", true)
				p.ok()
				p.abortAndRetry()
				p.f.fresh = true
			},
			want: "member v1 of rollout r2 (image:" + imgGeth + "@" + digestA + ") is in stage 3",
		},
		{
			name: "abort: a host that failed in the aborted rollout does not go first for the next digest",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.fail("v1", true)
				p.ok()
				p.rollout().State = fleet.RolloutAborted
				p.ok()
				p.follow()
				p.f.fresh = true
			},
			want: "member v1 of rollout r2 (image:" + imgGeth + "@" + digestB + ") is in stage 3",
		},
		{
			name: "abort: a grant for the next digest while a lease of the aborted rollout is still open",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.rollout().State = fleet.RolloutAborted
				p.ok()
				p.successor()
				p.grant("v2")
			},
			want: "granted although lease l1 of a closed rollout is still open on v1",
		},
		{
			name: "reopen: a grant next to a lease that was taken up again after its rollout was superseded",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()

				l.State = fleet.LeaseExpired
				p.rollout().Host("v1").State = fleet.HostPending
				p.rollout().Host("v1").LeaseID = ""
				p.ok()
				p.successor()

				// The agent did start: the lease runs again, and the
				// successor leases its canary next to it.
				p.f.hosts["v1"].disrupted[l.ID] = true
				l.State = fleet.LeaseRunning
				p.grant("v2")
			},
			want: "granted although lease l1 of a closed rollout is still open on v1",
		},
		{
			name: "reopen: a lease taken up again after its rollout was superseded fails and its host does not go first",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()

				l.State = fleet.LeaseExpired
				p.rollout().Host("v1").State = fleet.HostPending
				p.rollout().Host("v1").LeaseID = ""
				p.ok()
				p.successor()

				p.f.hosts["v1"].disrupted[l.ID] = true
				l.State = fleet.LeaseFailed
				p.f.fresh = true
			},
			want: "member v1 of rollout r2 (image:" + imgGeth + "@" + digestB + ") is in stage 3",
		},
		{
			name: "rows of an earlier build: a lease that failed after its rollout was closed, the row still says leased",
			bad: func(p *pinned) {
				l := p.grant("v1")
				p.ok()
				p.rollout().State = fleet.RolloutAborted
				p.ok()

				l.State = fleet.LeaseFailed
				p.f.hosts["v1"].disrupted[l.ID] = true
				p.ok()

				// The member of the aborted rollout is still leased. What counts
				// is how the lease ended.
				p.follow()
				p.f.fresh = true
			},
			want: "member v1 of rollout r2 (image:" + imgGeth + "@" + digestB + ") is in stage 3",
		},
		{
			name: "readiness: a lease whose epochs were not placed although the timing is known",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()
				p.verifying("v1").Readiness = "epoch timing unknown"
			},
			want: `is verifying with "epoch timing unknown" "", but the chain's timing is known`,
		},
		{
			name: "readiness: a lease failed for epochs that were not placed",
			bad: func(p *pinned) {
				p.grant("v1")
				p.ok()

				l := p.done("v1")
				p.f.hosts["v1"].doneAt[l.ID] = p.f.now.Add(-31 * time.Minute)
				p.f.since = p.f.now.Add(-2 * time.Hour)
				p.fail("v1", true)
				l.Error = "not ready after 30m0s: epoch timing unknown"
			},
			want: "but the chain's timing is known",
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
			name: "release on a run that began before genesis and has not lasted an epoch of the chain",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("n1")
				p.ok()

				// The chain began 20 seconds ago; the first sync after the
				// operation came a minute ago.
				p.genesis(p.f.now.Add(-20 * time.Second))

				l := p.done("n1")
				p.f.hosts["n1"].doneAt[l.ID] = p.f.now.Add(-61 * time.Second)
				p.f.hosts["n1"].lastSync = p.f.now.Add(-time.Minute)
				l.State = fleet.LeaseVerifying
				p.reconciled()
				p.f.hosts["n1"].lastSync = p.f.now
				l.State = fleet.LeaseReleased
				p.rollout().Host("n1").State = fleet.HostDone
			},
			want: "that spans 1m0s, it must span 48s",
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
