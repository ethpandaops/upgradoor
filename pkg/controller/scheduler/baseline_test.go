package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func healthyNode() protocol.NodeHealth {
	return protocol.NodeHealth{
		EL: &protocol.ELHealth{Reachable: true, Peers: 10, Head: 1000},
		CL: &protocol.CLHealth{Reachable: true, Peers: 10, HeadSlot: 3200},
	}
}

// stuckNode is a node whose EL is stuck on a block: syncing at head 100.
func stuckNode() protocol.NodeHealth {
	n := healthyNode()
	n.EL.Syncing = true
	n.EL.Head = 100

	return n
}

// stuckHosts adds hosts without validators whose EL is stuck, each with a
// need of its CL image.
func stuckHosts(b *worldBuilder, names ...string) {
	for _, name := range names {
		b.host(name, 0).Status.Node = stuckNode()
		b.wantsKind(name, protocol.KindCL, imgLighthouse, digestA)
	}
}

// verifyFor walks the host's lease to verifying and syncs every minute for d.
func (b *worldBuilder) verifyFor(host string, d time.Duration) {
	b.t.Helper()
	b.finish(host)
	b.step()

	for left := d; left > 0; left -= time.Minute {
		b.advance(min(left, time.Minute))
		b.step()
	}
}

var clKey = kindKey(protocol.KindCL, imgLighthouse, digestA)

func TestBaselineCapture(t *testing.T) {
	t.Run("a fresh sync", func(t *testing.T) {
		b := newWorldBuilder(t)
		stuckHosts(b, "s")

		b.step()

		l := b.lease("s")
		require.NotNil(t, l)
		require.NotNil(t, l.Baseline)
		assert.Equal(t, b.w.Now, l.Baseline.At)
		assert.Equal(t, "node is syncing", l.Baseline.Unready)
		assert.Equal(t, protocol.KindEL, l.Baseline.Root)
		assert.Equal(t, uint64(100), l.Baseline.Node.EL.Head)
		assert.Equal(t, []fleet.BaselineContainer{{Name: "c-cl", Kind: protocol.KindCL, State: "running"}},
			l.Baseline.Containers)
	})

	t.Run("a healthy host", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("h", 0).Status.Node = healthyNode()
		b.wantsKind("h", protocol.KindCL, imgLighthouse, digestA)

		b.step()

		require.NotNil(t, b.lease("h").Baseline)
		assert.Empty(t, b.lease("h").Baseline.Unready)
		assert.Empty(t, b.lease("h").Baseline.Root)
	})

	t.Run("a sync older than half of hostStaleAfter", func(t *testing.T) {
		b := newWorldBuilder(t)
		stuckHosts(b, "s")
		b.get("s").LastSeen = b.w.Now.Add(-b.w.Settings.HostStaleAfter.Std()/2 - time.Second)

		b.step()

		require.NotNil(t, b.lease("s"), "not stale")
		assert.Nil(t, b.lease("s").Baseline, "too old to judge by: the full check applies")
	})
}

func TestReadinessRoot(t *testing.T) {
	for name, tt := range map[string]struct {
		node       func(n *protocol.NodeHealth)
		live       uint64
		root       protocol.ContainerKind
		unready    string
		probedKind protocol.ContainerKind
	}{
		"ready":            {node: func(*protocol.NodeHealth) {}, live: 10},
		"EL syncing":       {node: func(n *protocol.NodeHealth) { n.EL.Syncing = true }, live: 10, root: "el", unready: "node is syncing"},
		"CL syncing":       {node: func(n *protocol.NodeHealth) { n.CL.Syncing = true }, live: 10, root: "cl", unready: "node is syncing"},
		"CL unreachable":   {node: func(n *protocol.NodeHealth) { n.CL.Reachable = false }, live: 10, root: "cl", unready: "CL is not reachable"},
		"EL short of peer": {node: func(n *protocol.NodeHealth) { n.EL.Peers = 0 }, live: 10, root: "el", unready: "waiting for peers: EL has 0 of 1"},
		"probe": {
			node: func(n *protocol.NodeHealth) {
				n.Probes = []protocol.KindHealth{{Kind: "mev", Error: "refused"}}
			},
			live: 10, root: "mev", unready: "probe mev: refused",
		},
		"validators":   {node: func(*protocol.NodeHealth) {}, live: 4, root: "vc", unready: "validators: 4 of 10 attesting"},
		"a few misses": {node: func(*protocol.NodeHealth) {}, live: 5},
	} {
		t.Run(name, func(t *testing.T) {
			b := newWorldBuilder(t)
			node := healthyNode()
			tt.node(&node)
			b.host("h", 10).Status.Node = node
			b.liveOverride["h"] = tt.live
			b.fillLiveness()

			p := newPass(b.w)
			assert.Equal(t, tt.root, p.hosts["h"].root)
			assert.Equal(t, tt.unready, p.hosts["h"].unready)
		})
	}
}

// compareWorld is a host whose lease is verifying since a minute, with a
// baseline of then, and a pass that sees it now.
func compareWorld(t *testing.T, weight uint64, then, now protocol.NodeHealth) (*pass, *fleet.Lease) {
	t.Helper()

	b := newWorldBuilder(t)
	h := b.host("h", weight)
	h.Status.Node = now
	b.wantsKind("h", protocol.KindCL, imgLighthouse, digestA)

	mev := container("c-mev", "org/mev:latest", digestOld, digestOld, b.w.Now)
	mev.Kind = "mev"
	h.Status.Containers = append(h.Status.Containers, mev)
	b.fillLiveness()

	done := b.w.Now.Add(-time.Minute)
	l := &fleet.Lease{
		ID: "l", Host: "h", State: fleet.LeaseVerifying, AgentDoneAt: &done,
		Needs: []protocol.Need{
			h.Status.Needs[0],
			{ID: "mev", Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Container: "c-mev"}},
		},
		Baseline: &fleet.LeaseBaseline{Node: then, Unready: "node is syncing", Duty: weight, Live: weight, LiveKnown: true},
	}

	return newPass(b.w), l
}

func TestCompareBaseline(t *testing.T) {
	with := func(base protocol.NodeHealth, change func(n *protocol.NodeHealth)) protocol.NodeHealth {
		n := cloneNode(base)
		change(&n)

		return n
	}

	noPeers := func() string { return "" }

	for name, tt := range map[string]struct {
		then, now protocol.NodeHealth
		worse     string
	}{
		"still stuck":       {then: stuckNode(), now: stuckNode()},
		"better":            {then: stuckNode(), now: healthyNode()},
		"head backwards":    {then: stuckNode(), now: with(stuckNode(), func(n *protocol.NodeHealth) { n.EL.Head = 90 }), worse: "EL head 90, was 100"},
		"EL unreachable":    {then: stuckNode(), now: with(stuckNode(), func(n *protocol.NodeHealth) { n.EL.Reachable = false }), worse: "EL not reachable, was syncing"},
		"CL was ok":         {then: stuckNode(), now: with(stuckNode(), func(n *protocol.NodeHealth) { n.CL.Syncing = true }), worse: "CL syncing, was ok"},
		"CL EL offline":     {then: stuckNode(), now: with(stuckNode(), func(n *protocol.NodeHealth) { n.CL.ELOffline = true }), worse: "CL reports its EL offline, was ok"},
		"CL head backwards": {then: stuckNode(), now: with(stuckNode(), func(n *protocol.NodeHealth) { n.CL.HeadSlot = 3100 }), worse: "CL head slot 3100, was 3200"},
		"CL probe gone":     {then: stuckNode(), now: with(stuckNode(), func(n *protocol.NodeHealth) { n.CL = nil }), worse: "the CL probe is gone"},
		"probe still unhealthy": {
			then: with(stuckNode(), func(n *protocol.NodeHealth) { n.Probes = []protocol.KindHealth{{Kind: "mev"}} }),
			now:  with(stuckNode(), func(n *protocol.NodeHealth) { n.Probes = []protocol.KindHealth{{Kind: "mev"}} }),
		},
		"probe healthy before": {
			then:  with(stuckNode(), func(n *protocol.NodeHealth) { n.Probes = []protocol.KindHealth{{Kind: "mev", Healthy: true}} }),
			now:   with(stuckNode(), func(n *protocol.NodeHealth) { n.Probes = []protocol.KindHealth{{Kind: "mev"}} }),
			worse: "probe mev is not healthy, healthy before",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, l := compareWorld(t, 0, tt.then, tt.now)

			worse, pending := p.compareBaseline(l, p.hosts["h"], noPeers)
			assert.Equal(t, tt.worse, worse)
			assert.Empty(t, pending)
		})
	}

	t.Run("peers below the fleet", func(t *testing.T) {
		p, l := compareWorld(t, 0, stuckNode(), stuckNode())
		below := func() string { return "EL peers 1, the fleet's median is 30 (needs 15)" }

		_, pending := p.compareBaseline(l, p.hosts["h"], below)
		assert.Equal(t, below(), pending, "fine before: as strict as the full check")

		l.Baseline.BelowFleet = "EL peers 2, the fleet's median is 30 (needs 15)"
		_, pending = p.compareBaseline(l, p.hosts["h"], below)
		assert.Empty(t, pending, "below the fleet before")
	})

	t.Run("validators", func(t *testing.T) {
		p, l := compareWorld(t, 10, stuckNode(), stuckNode())
		p.w.Attested = nil

		_, pending := p.compareBaseline(l, p.hosts["h"], noPeers)
		assert.Contains(t, pending, "waiting for the first attestation", "attested before: must again")

		l.Baseline.Live = 0
		_, pending = p.compareBaseline(l, p.hosts["h"], noPeers)
		assert.Empty(t, pending, "none attested before")
	})
}

// A host that was not ready before its lease and is no worse after it is
// released once baselineGrace has passed: its member is done, but nothing
// is proven: no success, no charge taken or set, no broken mark cleared.
func TestNoWorseRelease(t *testing.T) {
	b := newWorldBuilder(t)
	stuckHosts(b, "s")
	b.broke("s", packagesNeed("1.5"))

	charged := b.w.Now.Add(-time.Hour)
	b.get("s").Unproven = &charged

	b.step()
	l := b.lease("s")
	require.NotNil(t, l)

	b.verifyFor("s", 10*time.Minute)
	require.Equal(t, fleet.LeaseVerifying, b.lease("s").State, "within the grace")
	assert.Contains(t, b.lease("s").Readiness,
		"node is syncing; not ready before the lease (node is syncing): no worse, released as such from ")
	assert.NotNil(t, b.lease("s").NoWorseSince)

	for range 6 {
		b.advance(time.Minute)
		b.step()
	}

	ended := b.lastLease("s")
	assert.Equal(t, fleet.LeaseReleased, ended.State)
	assert.Equal(t, fleet.OutcomeNoWorse, ended.Outcome)
	assert.Equal(t, "not ready before the lease: node is syncing; no worse after", ended.Readiness)

	r := b.closed(clKey)
	require.NotNil(t, r, "the rollout completed: no halt")
	assert.Equal(t, fleet.RolloutCompleted, r.State)
	assert.Equal(t, fleet.HostDone, r.Host("s").State)
	assert.True(t, r.Host("s").NoWorse)
	assert.Equal(t, l.ID, r.Host("s").LeaseID)
	assert.False(t, doneByRelease(r.Host("s")), "no success")

	old := packagesNeed("1.5")
	assert.Equal(t, map[string]string{needGroup(&old): "broke-s"}, b.get("s").Broken)
	assert.Equal(t, &charged, b.get("s").Unproven, "the charge stays")
}

func TestNoWorseReleaseSetsNoCharge(t *testing.T) {
	b := newWorldBuilder(t)
	stuckHosts(b, "s")

	b.step()
	b.verifyFor("s", 17*time.Minute)

	assert.Equal(t, fleet.OutcomeNoWorse, b.lastLease("s").Outcome)
	assert.Nil(t, b.get("s").Unproven)
}

// A host released as no worse is no canary: the next candidate is, and once
// only hosts that are not ready for something else are left, they go
// without one and without the pause after a canary.
func TestNoWorseIsNoCanary(t *testing.T) {
	t.Run("a ready host is the canary again", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxLeasedPercent = 100
		stuckHosts(b, "s")

		for _, name := range []string{"h1", "h2"} {
			h := b.host(name, 0)
			h.Status.Node = healthyNode()
			h.Status.Hold.Holds = []protocol.HostHold{{Holder: fleet.HolderStaff}}
			b.wantsKind(name, protocol.KindCL, imgLighthouse, digestA)
		}

		b.step()
		require.Equal(t, "s", b.mustRollout(clKey).Canary, "the only candidate")

		b.verifyFor("s", 17*time.Minute)
		require.Equal(t, fleet.OutcomeNoWorse, b.lastLease("s").Outcome)

		for _, name := range []string{"h1", "h2"} {
			b.get(name).Status.Hold.Holds = nil
		}

		out := b.step()
		assert.Equal(t, "h1", b.mustRollout(clKey).Canary)
		assert.Equal(t, []string{"h1"}, b.leasedHosts())
		assert.Equal(t, "after canary h1", queueReasons(out.Plan)["h2"])
	})

	t.Run("only hosts not ready for something else are left", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxLeasedPercent = 100
		b.w.Settings.PauseAfterCanaryImage = true
		stuckHosts(b, "s1", "s2", "s3")

		out := b.step()
		require.Equal(t, "s1", b.mustRollout(clKey).Canary)
		require.Equal(t, []string{"s1"}, b.leasedHosts())
		require.Equal(t, "after canary s1", queueReasons(out.Plan)["s2"])

		b.verifyFor("s1", 17*time.Minute)
		require.Equal(t, fleet.OutcomeNoWorse, b.lastLease("s1").Outcome)

		b.step()
		r := b.mustRollout(clKey)
		assert.Empty(t, r.Canary)
		assert.False(t, r.Paused)
		assert.Equal(t, []string{"s2", "s3"}, b.leasedHosts())
	})
}

// A host that was not ready before its lease and passes the full check after
// it recovered: a success like any other.
func TestRecoveredIsASuccess(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxLeasedPercent = 100
	b.w.Settings.PauseAfterCanaryImage = true
	stuckHosts(b, "s1", "s2")

	b.step()
	require.Equal(t, []string{"s1"}, b.leasedHosts())

	b.get("s1").Status.Node = healthyNode()
	b.verifyFor("s1", 2*time.Minute)

	ended := b.lastLease("s1")
	assert.Equal(t, fleet.LeaseReleased, ended.State)
	assert.Equal(t, fleet.OutcomeRecovered, ended.Outcome)
	assert.Equal(t, "recovered: was not ready before the lease (node is syncing)", ended.Readiness)

	r := b.mustRollout(clKey)
	assert.Equal(t, "s1", r.Canary)
	assert.False(t, r.Host("s1").NoWorse)
	assert.Equal(t, reasonCanaryPause, r.PauseReason, "the first success")
}

// A signal worse than before the lease fails it at readinessTimeout.
func TestWorseFails(t *testing.T) {
	b := newWorldBuilder(t)
	stuckHosts(b, "s")

	b.step()
	b.finish("s")
	b.get("s").Status.Node.EL = &protocol.ELHealth{Reachable: true, Syncing: true, Peers: 10, Head: 90}
	b.step()
	b.advance(time.Minute)
	b.step()

	assert.Contains(t, b.lease("s").Readiness, "not ready before the lease (node is syncing): worse: EL head 90, was 100")
	assert.Nil(t, b.lease("s").NoWorseSince)

	for range 30 {
		b.advance(time.Minute)
		b.step()
	}

	ended := b.lastLease("s")
	assert.Equal(t, fleet.LeaseFailed, ended.State)
	assert.Equal(t, fleet.OutcomeWorse, ended.Outcome)
	assert.Equal(t, fleet.RolloutHalted, b.mustRollout(clKey).State)
}

func TestServingRank(t *testing.T) {
	packages := packagesNeed("2.0")

	for name, tt := range map[string]struct {
		node func(n *protocol.NodeHealth)
		live uint64
		kind protocol.ContainerKind
		want int
	}{
		"healthy":                {live: 10, kind: protocol.KindEL, want: rankHealthy},
		"a few misses, EL":       {live: 6, kind: protocol.KindEL, want: rankHealthy},
		"EL down, EL need":       {node: func(n *protocol.NodeHealth) { n.EL.Syncing = true }, kind: protocol.KindEL, want: rankDownRelated},
		"EL down, CL need":       {node: func(n *protocol.NodeHealth) { n.EL.Syncing = true }, kind: protocol.KindCL, want: rankDownUnrelated},
		"CL down, CL need":       {node: func(n *protocol.NodeHealth) { n.CL.Reachable = false }, kind: protocol.KindCL, want: rankDownRelated},
		"validators down, VC":    {live: 0, kind: protocol.KindVC, want: rankDownRelated},
		"validators down, CL":    {live: 0, kind: protocol.KindCL, want: rankDownRelated},
		"validators down, EL":    {live: 0, kind: protocol.KindEL, want: rankDownUnrelated},
		"validators down, other": {live: 0, want: rankDownUnrelated},
	} {
		t.Run(name, func(t *testing.T) {
			b := newWorldBuilder(t)
			h := b.host("h", 10)
			h.Status.Node = healthyNode()

			if tt.node != nil {
				tt.node(&h.Status.Node)
			} else {
				b.liveOverride["h"] = tt.live
			}

			if tt.kind != "" {
				b.wantsKind("h", tt.kind, imgLighthouse, digestA)
			} else {
				b.wants("h", packages)
			}

			b.fillLiveness()

			p := newPass(b.w)
			p.syncRollouts()
			require.Len(t, p.rollouts, 1)
			assert.Equal(t, tt.want, p.servingRank(p.rollouts[0], p.hosts["h"]))
		})
	}
}

// Inside a rollout a host that is down for what the rollout is about goes
// first and is the canary, a ready one next, one that is down for something
// else last: it waits for capacity the ready hosts took first.
func TestServingOrderByReadiness(t *testing.T) {
	b := newWorldBuilder(t)

	b.host("a-unrelated", 0).Status.Node = stuckNode()
	b.wantsKind("a-unrelated", protocol.KindCL, imgLighthouse, digestA)

	for _, name := range []string{"b-ready", "c-ready"} {
		b.host(name, 0).Status.Node = healthyNode()
		b.wantsKind(name, protocol.KindCL, imgLighthouse, digestA)
	}

	related := b.host("d-related", 0)
	related.Status.Node = healthyNode()
	related.Status.Node.CL.Reachable = false
	b.wantsKind("d-related", protocol.KindCL, imgLighthouse, digestA)

	out := b.step()
	r := b.mustRollout(clKey)
	assert.Equal(t, "d-related", r.Canary)
	assert.Equal(t, []string{"d-related"}, b.leasedHosts())

	order := make([]string, 0, len(out.Plan.Queue))
	for _, q := range out.Plan.Queue {
		order = append(order, q.Host)
	}

	assert.Equal(t, []string{"b-ready", "c-ready", "a-unrelated"}, order)

	// The canary recovers; one host at a time follows.
	b.get("d-related").Status.Node = healthyNode()
	b.serve("d-related")
	require.Equal(t, fleet.OutcomeRecovered, b.lastLease("d-related").Outcome)

	out = b.step()
	assert.Equal(t, []string{"b-ready"}, b.leasedHosts())
	assert.Equal(t, "after healthy hosts: not ready (node is syncing)", queueReasons(out.Plan)["a-unrelated"])
	assert.Equal(t, "hosts at once: 1 of 1 busy", queueReasons(out.Plan)["c-ready"])
	assert.Equal(t, "node is syncing", out.Plan.Hosts["a-unrelated"].NotReady)
}

func TestPullCap(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxLeasedPercent = 100
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.MaxConcurrentPulls = 1

	b.host("a", 0)
	b.wantsImage("a", imgLighthouse, digestA)
	b.host("b", 0)
	b.wantsImage("b", imgGeth, digestA)
	b.host("c", 0)
	b.wants("c", packagesNeed("2.0"))

	out := b.step()
	// The geth rollout is the older one.
	assert.Equal(t, []string{"b", "c"}, b.leasedHosts(), "packages pull nothing")

	q := out.Plan.Queue
	require.Len(t, q, 1)
	assert.Equal(t, "a", q[0].Host)
	assert.Equal(t, "image pulls at once: 1 of 1", q[0].Reason)
	assert.Equal(t, WaitPulls, q[0].Wait)

	b.report("b", protocol.PhasePulling, false)
	b.step()
	assert.Nil(t, b.lease("a"), "b still pulls")

	b.report("b", protocol.PhaseApplying, true)
	b.step()
	assert.NotNil(t, b.lease("a"), "b is past its pull")
}
