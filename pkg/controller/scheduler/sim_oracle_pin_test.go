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
type pinned struct {
	t    *testing.T
	o    *oracle
	f    *simFacts
	snap *simSnapshot
	ids  int
}

func newPinned(t *testing.T) *pinned {
	t.Helper()

	now := testGenesis.Add((pinEpoch+1)*simEpoch + 5*time.Second)
	settings := fleet.DefaultSettings()
	settings.MaxUnavailablePercent = 40

	p := &pinned{
		t: t,
		o: newOracle(),
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
			rows:           map[uint64]map[string]uint64{pinEpoch: {}},
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
		name     string
		weight   uint64
		bootnode bool
	}{{"n1", 0, false}, {"v1", 10, false}, {"v2", 10, false}, {"v3", 10, false}, {"v4", 70, false}, {"b1", 0, true}} {
		p.f.hosts[h.name] = &simHostFact{
			weight: h.weight, bootnode: h.bootnode, live: h.weight, lastSync: now,
			status: &protocol.SyncRequest{
				Verbs:      slices.Clone(allVerbs),
				Containers: []protocol.Container{container("c", imgGeth, digestOld, digestA, now)},
				Needs:      []protocol.Need{imageNeed("c", imgGeth, digestA)},
			},
			disrupted: map[string]bool{},
			doneAt:    map[string]time.Time{},
		}

		if h.weight > 0 {
			p.f.rows[pinEpoch][h.name] = h.weight
		}

		r.Hosts = append(r.Hosts, fleet.RolloutHost{
			Host: h.name, State: fleet.HostPending, Stage: p.f.hosts[h.name].class(),
		})
	}

	p.snap.rollouts = []*fleet.Rollout{r}

	require.NoError(t, p.observe(), "the base history must be accepted")

	return p
}

func (p *pinned) rollout() *fleet.Rollout { return p.snap.rollouts[0] }

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
	need := imageNeed("c", imgGeth, digestA)
	l := &fleet.Lease{
		ID: fmt.Sprintf("l%d", p.ids), Host: host, State: fleet.LeaseGranted,
		Needs: []protocol.Need{need}, Rollouts: map[string]string{need.ID: pinRollout},
		GrantedBy: actorScheduler, GrantedAt: p.f.now,
	}
	p.snap.leases = append(p.snap.leases, l)

	r := p.rollout()
	m := r.Host(host)
	m.State, m.LeaseID = fleet.HostLeased, l.ID

	if p.f.hosts[host].class() == fleet.StageBatches && r.Canary == "" {
		r.Canary = host
	}

	return l
}

// done lets the agent finish the host's lease one second before the newest
// liveness epoch began.
func (p *pinned) done(host string) *fleet.Lease {
	l := p.lease(host)
	h := p.f.hosts[host]
	h.disrupted[l.ID] = true
	h.doneAt[l.ID] = testGenesis.Add(pinEpoch*simEpoch - time.Second)
	h.status.Needs = nil
	h.status.Containers = []protocol.Container{container("c", imgGeth, digestA, digestA, p.f.now)}

	return l
}

// serve walks the host through a lease to released and done.
func (p *pinned) serve(host string) {
	p.t.Helper()
	p.grant(host)
	p.ok()
	p.release(host)
	p.ok()
}

func (p *pinned) release(host string) {
	l := p.done(host)
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

	p.grant("n1")
	p.ok()
	p.release("n1")
	p.ok()

	// The canary alone, then two validator hosts within 40%, then the heavy
	// host alone, then the bootnode.
	p.grant("v1")
	p.ok()
	p.release("v1")
	p.grant("v2")
	p.grant("v3")
	p.ok()
	p.release("v2")
	p.release("v3")
	p.ok()
	p.grant("v4")
	p.ok()
	p.release("v4")
	p.grant("b1")
	p.ok()
	p.release("b1")
	p.rollout().State = fleet.RolloutCompleted
	p.ok()
}

func TestOracleAcceptsAGrantAfterARetry(t *testing.T) {
	p := newPinned(t)

	p.serve("n1")
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
	l := p.grant("n1")
	p.ok()

	l.State = fleet.LeaseExpired
	p.rollout().Host("n1").State = fleet.HostPending
	p.ok()

	p.f.hosts["n1"].disrupted[l.ID] = true
	l.State = fleet.LeaseRunning
	p.rollout().Host("n1").State = fleet.HostLeased
	p.ok()

	p.release("n1")
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
	// hosts are released on their probes.
	p := newPinned(t)
	p.withoutLiveness()

	p.serve("n1")
	p.serve("v1")
	p.grant("v2")
	p.grant("v3")
	p.ok()
	p.release("v2")
	p.release("v3")
	p.ok()
}

func TestOracleAcceptsASilentLeaseLongAfterTheStart(t *testing.T) {
	p := newPinned(t)
	p.grant("n1")
	p.ok()

	// The controller started an hour ago and has not heard from the agent
	// since: longer than leaseTtl.
	p.f.since = p.f.now.Add(-time.Hour)
	p.f.hosts["n1"].lastSync = p.f.now.Add(-2 * time.Hour)
	p.fail("n1", true)
	p.lease0().Error = reasonAgentSilent
	p.ok()
}

// lease0 is the first lease of the history, whatever its state.
func (p *pinned) lease0() *fleet.Lease { return p.snap.leases[0] }

func TestOracleRejectsBadHistories(t *testing.T) {
	// canaryDone is a history up to the point where the batches may start.
	canaryDone := func(p *pinned) {
		p.serve("n1")
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
			name: "ordering violation: validator host before the hosts without validators",
			bad:  func(p *pinned) { p.grant("v1") },
			want: "stage 2 host granted while n1 of stage 0 is pending",
		},
		{
			name: "ordering violation: second validator host while the canary is out",
			bad: func(p *pinned) {
				p.serve("n1")
				p.grant("v1")
				p.ok()
				p.grant("v2")
			},
			want: "validator host granted before canary v1 is done",
		},
		{
			name: "ordering violation: bootnode before the validator hosts",
			bad: func(p *pinned) {
				canaryDone(p)
				p.grant("b1")
			},
			want: "stage 3 host granted while v2 of stage 2 is pending",
		},
		{
			name: "grant during a halt",
			bad: func(p *pinned) {
				p.serve("n1")
				p.grant("v1")
				p.ok()
				p.fail("v1", true)
				p.ok()
				// Nobody retried, yet the rollout runs again and serves v2.
				p.rollout().State = fleet.RolloutActive
				p.rollout().Host("v1").State = fleet.HostDone
				p.grant("v2")
			},
			want: "although lease l2 failed in it and nobody retried it",
		},
		{
			name: "failed lease that does not halt its rollout",
			bad: func(p *pinned) {
				p.serve("n1")
				p.grant("v1")
				p.ok()
				p.fail("v1", false)
			},
			want: "rollout r1 is active although lease l2 failed in it",
		},
		{
			name: "grant to a held host",
			bad: func(p *pinned) {
				p.f.hosts["n1"].holdWanted = true
				p.grant("n1")
			},
			want: "the host is held",
		},
		{
			name: "grant to a stale host",
			bad: func(p *pinned) {
				p.f.hosts["n1"].lastSync = p.f.now.Add(-time.Hour)
				p.grant("n1")
			},
			want: "the host is stale",
		},
		{
			name: "grant for a verb the agent does not allow",
			bad: func(p *pinned) {
				p.f.hosts["n1"].status.Verbs = []protocol.Verb{protocol.VerbReboot}
				p.grant("n1")
			},
			want: "wants verb roll, which the agent does not allow",
		},
		{
			name: "grant of a need the agent does not report",
			bad: func(p *pinned) {
				p.f.hosts["n1"].status.Needs = nil
				p.grant("n1")
			},
			want: "is not one the agent reports",
		},
		{
			name: "grant in a paused rollout",
			bad: func(p *pinned) {
				p.rollout().Paused = true
				p.grant("n1")
			},
			want: "the rollout is paused",
		},
		{
			name: "grant under the global pause",
			bad: func(p *pinned) {
				p.f.settings.Paused = true
				p.grant("n1")
			},
			want: "the global pause is in effect",
		},
		{
			name: "grant in an unapproved rollout",
			bad: func(p *pinned) {
				p.f.settings.PolicyImage = fleet.PolicyManual
				p.grant("n1")
			},
			want: "nobody approved the rollout",
		},
		{
			name: "grant while the gate fails",
			bad: func(p *pinned) {
				p.f.health.FinalityLagEpochs = 9
				p.grant("n1")
			},
			want: "the network gate fails: [finality lag]",
		},
		{
			name: "grant in a closed rollout",
			bad: func(p *pinned) {
				p.rollout().State = fleet.RolloutAborted
				p.grant("n1")
			},
			want: "the rollout is aborted",
		},
		{
			name: "second lease for a host",
			bad: func(p *pinned) {
				p.grant("n1")
				p.ok()
				p.grant("n1")
			},
			want: "host n1 holds two leases",
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
				p.serve("n1")
				p.grant("v1")
				p.ok()
				p.release("v1")
				p.f.hosts["v1"].doneAt["l2"] = p.f.now
			},
			want: "released after 0 of 1 complete epochs since the agent was done",
		},
		{
			name: "release although the validators stay down",
			bad: func(p *pinned) {
				p.serve("n1")
				p.grant("v1")
				p.ok()
				p.release("v1")
				p.f.rows[pinEpoch]["v1"] = 3
			},
			want: "released although 3 of 10 validators attested in epoch 99 (need 9)",
		},
		{
			name: "expired lease that runs again although the agent never started it",
			bad: func(p *pinned) {
				l := p.grant("n1")
				p.ok()
				l.State = fleet.LeaseExpired
				p.rollout().Host("n1").State = fleet.HostPending
				p.ok()
				l.State = fleet.LeaseRunning
				p.rollout().Host("n1").State = fleet.HostLeased
			},
			want: "left terminal state expired for running",
		},
		{
			name: "released lease that runs again",
			bad: func(p *pinned) {
				p.serve("n1")
				p.snap.leases[0].State = fleet.LeaseRunning
			},
			want: "left terminal state released for running",
		},
		{
			name: "grant to a host a person skipped for the cause",
			bad: func(p *pinned) {
				p.f.skipped = map[string]map[string]bool{imageKey(imgGeth, digestA): {"n1": true}}
				p.grant("n1")
			},
			want: "a person skipped the host for image:" + imgGeth + "@" + digestA + " and nobody retried it",
		},
		{
			name: "release although the agent still reports the need it served",
			bad: func(p *pinned) {
				l := p.grant("n1")
				p.ok()
				p.release("n1")

				need := imageNeed("c", imgGeth, digestA)
				l.Operation = &protocol.Operation{
					LeaseID: l.ID, Phase: protocol.PhaseDone, Disrupted: true,
					Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepDone}},
				}
				p.f.hosts["n1"].status.Needs = []protocol.Need{need}
			},
			want: "released although the agent still reports need " + imageNeed("c", imgGeth, digestA).ID,
		},
		{
			name: "lease failed as silent although the controller has just started",
			bad: func(p *pinned) {
				p.grant("n1")
				p.ok()

				// The agent's last sync is old, but the controller was down
				// until ten minutes ago: less than leaseTtl to be heard.
				p.f.since = p.f.now.Add(-10 * time.Minute)
				p.f.hosts["n1"].lastSync = p.f.now.Add(-2 * time.Hour)
				p.fail("n1", true)
				p.lease0().Error = reasonAgentSilent
			},
			want: `failed as "agent silent" after 10m0s of silence`,
		},
		{
			name: "grant while liveness is merely unknown",
			bad: func(p *pinned) {
				p.f.health.LivenessKnown = false
				p.grant("n1")
			},
			want: "liveness data stale",
		},
		{
			name: "release of a validator host on bad probes when liveness is not served",
			bad: func(p *pinned) {
				p.withoutLiveness()
				p.serve("n1")
				p.grant("v1")
				p.ok()
				p.f.hosts["v1"].status.Node = protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Syncing: true}}
				p.release("v1")
			},
			want: "released although its probes do not report synced",
		},
		{
			name: "controller hold lifted although a person holds the host",
			bad: func(p *pinned) {
				p.f.holds = map[string]bool{"n1": true}
				p.snap.directives = append(p.snap.directives, &fleet.Directive{
					Directive: protocol.Directive{ID: "d1", Kind: protocol.DirectiveReleaseHold},
					Host:      "n1", State: fleet.DirectivePending,
				})
			},
			want: "lifts the controller hold of n1",
		},
		{
			name: "lease expired although the agent stopped containers",
			bad: func(p *pinned) {
				l := p.grant("n1")
				p.ok()
				p.f.hosts["n1"].disrupted[l.ID] = true
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
