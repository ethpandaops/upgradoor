package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/lease"
	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// stubLink answers the syncs of a cycle test.
type stubLink struct {
	sync func(ctx context.Context) (*protocol.SyncResponse, error)
}

var _ link.Client = (*stubLink)(nil)

func (s *stubLink) Sync(ctx context.Context, _ *protocol.SyncRequest) (*protocol.SyncResponse, error) {
	return s.sync(ctx)
}

func (*stubLink) Inventory(context.Context, *protocol.Inventory) error { return nil }

func (*stubLink) Interval(bool) time.Duration { return time.Hour }

func TestCycle(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	a, ok := e.newAgent().(*agent)
	require.True(t, ok)
	require.NoError(t, a.open())

	ctx, cancel := context.WithCancel(t.Context())
	stub := &stubLink{sync: func(context.Context) (*protocol.SyncResponse, error) {
		cancel()

		return nil, context.Canceled
	}}
	a.link = stub

	assert.False(t, a.cycle(ctx))
	assert.Nil(t, a.view, "a sync cut short by the end publishes nothing")

	spy := &holdSpy{State: a.state}
	a.state = spy
	stub.sync = func(context.Context) (*protocol.SyncResponse, error) {
		return &protocol.SyncResponse{
			Lease:      e.lease("lease-1", agenttest.RebootNeed()),
			Directives: []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}},
		}, nil
	}

	assert.True(t, a.cycle(t.Context()), "a lease taken: sync soon")
	assert.Equal(t, []string{""}, spy.leases, "the hold is written before the lease is taken")
	assert.Equal(t, "lease-1", localAPI{a}.Status().Lease, "the status of the same cycle has the lease")
}

// holdSpy records the lease on record whenever a hold is written.
type holdSpy struct {
	lease.State

	leases []string
}

func (h *holdSpy) SetHold(source string, hold *plan.Hold) error {
	_, cur := h.ManualAndCurrent()

	id := ""
	if cur != nil {
		id = cur.Lease.ID
	}

	h.leases = append(h.leases, id)

	return h.State.SetHold(source, hold)
}

// countingLookup counts the controller's lookups of one tag.
type countingLookup struct {
	lookup func(ctx context.Context, ref string) (string, error)
	ref    string
	n      atomic.Int64
}

func (r *countingLookup) Resolve(ctx context.Context, ref string) (string, error) {
	if imageref.Same(ref, r.ref) {
		r.n.Add(1)
	}

	return r.lookup(ctx, ref)
}

// While Docker cannot be asked, the last good containers and the open rolls stand.
func TestDockerOutage(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	held := e.withhold(agenttest.GethImage)
	counted := &countingLookup{lookup: held.Resolve, ref: agenttest.FixImage}
	e.ctrl.Set(func(c *agenttest.Controller) { c.Lookup = counted.Resolve })
	e.start()

	teku := protocol.Directive{ID: "d-teku", Kind: protocol.DirectiveRoll, Container: "teku"}
	e.ctrl.Direct(teku)
	e.ctrl.Await("the teku roll", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.ManualNeedID("d-teku"))
	})

	e.ctrl.Direct(teku, protocol.Directive{ID: "d-geth", Kind: protocol.DirectiveRoll, Container: "geth"})
	e.ctrl.AwaitSyncs(3)

	e.host.Fail("observe", errors.New("docker is restarting"))

	from := e.ctrl.SyncCount()
	e.ctrl.AwaitSyncs(5)

	for _, req := range e.ctrl.Syncs()[from:] {
		require.Len(t, req.Containers, 4, "the last good containers")
		require.Equal(t, agenttest.GethV1, agenttest.ContainerOf(t, &req, "geth").Digest)
		require.True(t, hasNeed(&req, protocol.ManualNeedID("d-teku")), "the manual need is kept")
		require.Nil(t, agenttest.AckOf(&req, "d-geth"), "the waiting roll is not answered")
	}

	held.hold.Store(false)
	e.ctrl.Await("the geth roll answered", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-geth") != nil
	})

	// A new target while Docker is away is not followed from the stale containers.
	e.writeOverrides("overrides:\n  geth:\n    image: " + agenttest.FixImage + "\n")
	e.ctrl.AwaitSyncs(5)
	assert.Zero(t, counted.n.Load())

	e.host.Fail("observe", nil)

	require.Eventually(t, func() bool { return counted.n.Load() > 0 }, agenttest.AwaitTimeout, 2*time.Millisecond,
		"the override's tag is looked up once Docker answers")
}

// A hold and a lease in one answer: the lease is refused.
func TestHoldAndLeaseInOneAnswer(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.start()

	needs := awaitNeeds(t, e, protocol.RebootNeedID)

	e.ctrl.Set(func(c *agenttest.Controller) {
		c.Directives = []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance"}}
		c.Lease = e.lease("lease-held", needs...)
	})

	op := e.ctrl.AwaitFinished("lease-held")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepRefused, s.Status)
		assert.Contains(t, s.Error, "controller hold")
	}

	assert.Empty(t, agenttest.Disruptive(e.host.Calls()))
}

func TestSyncReportsTheHost(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := newEnv(t, sc)
	// Spelled differently from the container's reference on purpose.
	e.deploy(map[string]plan.Deployed{"geth": {Image: "docker.io/" + agenttest.GethImage}})
	e.start()

	req := e.ctrl.Await("registry checks", func(req *protocol.SyncRequest) bool {
		for _, c := range req.Containers {
			if c.Managed && c.Registry == nil {
				return false
			}
		}

		return len(req.Containers) == 4
	})

	assert.Equal(t, protocol.Version, req.ProtocolVersion)
	assert.Equal(t, "test", req.AgentVersion)
	assert.Equal(t, "boot", req.BootID)
	assert.Equal(t, e.cfg.Policy.Verbs, req.Verbs)
	assert.False(t, req.Hold.Active())
	assert.Equal(t, "debian", req.OS.ID)
	assert.Equal(t, protocol.Pending{Upgradable: 2, Security: 1, Blocked: 1}, req.Pending)
	assert.NotEmpty(t, req.InventoryHash)
	assert.Nil(t, req.Operation)
	assert.Nil(t, req.Node.EL, "no probe, no node health")

	geth := agenttest.ContainerOf(t, &req, "geth")
	assert.True(t, geth.Managed)
	assert.Equal(t, protocol.KindEL, geth.Kind)
	assert.Equal(t, protocol.ImpactNode, geth.Impact)
	assert.Equal(t, "docker.io/"+agenttest.GethImage, geth.ConfiguredImage)
	for _, n := range req.Needs {
		assert.NotEqual(t, protocol.NeedSpec, n.Kind, "docker.io/x and x are the same reference")
	}
	require.NotNil(t, geth.Registry)
	assert.Equal(t, agenttest.GethV1, geth.Registry.Digest)
	assert.True(t, geth.Registry.Pulled)
	assert.Empty(t, geth.Registry.Error)

	dora := agenttest.ContainerOf(t, &req, "dora")
	assert.False(t, dora.Managed)
	assert.Empty(t, dora.Kind)
	assert.Nil(t, dora.Registry)
	assert.Empty(t, dora.Team)

	assert.Equal(t, agenttest.TeamGeth, geth.Team)
	assert.True(t, geth.Overridable)
	assert.Equal(t, agenttest.TeamTeku, agenttest.ContainerOf(t, &req, "validator").Team)
	assert.Equal(t, protocol.KindVC, agenttest.ContainerOf(t, &req, "validator").Kind)

	// The blocked package is the only need; openssl is not the agent's.
	require.Equal(t, []string{agenttest.PackagesNeed().ID}, agenttest.NeedIDs(&req))
	assert.Equal(t, agenttest.PackagesNeed(), req.Needs[0])
}

// The image of a need is pulled only in a pull window.
// The lease pulls its image first. When that fails nothing is begun and the
// need says why until the pull may be tried again.
func TestLeasePullsItsImage(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "rev-2")
	e.host.Fail("pull:"+agenttest.GethImage, errors.New("registry is slow today"))
	e.start()

	needID := protocol.ImageNeedID("geth", agenttest.GethV2)
	req := e.ctrl.Await("the need", func(req *protocol.SyncRequest) bool { return hasNeed(req, needID) })

	need := findNeed(t, &req, needID)
	assert.Empty(t, need.PullError)
	assert.False(t, agenttest.ContainerOf(t, &req, "geth").Registry.Pulled)
	assert.NotContains(t, e.host.Calls(), "pull "+agenttest.GethImage, "no pull without a lease")

	e.ctrl.Grant(e.lease("lease-1", need))

	op := e.ctrl.AwaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	require.Len(t, op.Steps, 1)
	assert.Equal(t, protocol.StepDeferred, op.Steps[0].Status)
	assert.Contains(t, op.Steps[0].Error, "registry is slow today")
	assert.Empty(t, agenttest.Disruptive(e.host.Calls()), "nothing was begun")

	e.ctrl.Set(func(c *agenttest.Controller) { c.Lease = nil })

	req = e.ctrl.Await("the need says why", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, needID) && findNeed(t, req, needID).PullError != ""
	})
	assert.Contains(t, findNeed(t, &req, needID).PullError, "registry is slow today")
	assert.True(t, agenttest.ContainerOf(t, &req, "geth").Registry.PullFailed)

	// The pause passes: the need may be granted again, and the pull works.
	e.host.Fail("pull:"+agenttest.GethImage, nil)
	e.offset.Add(int64(10 * time.Minute))

	req = e.ctrl.Await("the need is open again", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, needID) && findNeed(t, req, needID).PullError == ""
	})

	e.ctrl.Grant(e.lease("lease-2", findNeed(t, &req, needID)))

	op = e.ctrl.AwaitFinished("lease-2")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
}

// The agent lists its tags and knows of them what the controller answers.
func TestTagsAreListedAndTheirDigestsTaken(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.ctrl.Set(func(c *agenttest.Controller) { c.Lookup = nil })
	e.start()

	req := e.ctrl.AwaitSyncs(3)
	assert.Equal(t, []string{agenttest.TekuImage, agenttest.GethImage, "sigp/lighthouse:stable"}, req.Tags)
	assert.Nil(t, agenttest.ContainerOf(t, &req, "geth").Registry, "the controller has not looked it up")
	assert.Empty(t, req.Needs)

	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "rev-2")
	e.ctrl.Set(func(c *agenttest.Controller) {
		c.Relay = []protocol.TagDigest{{Ref: agenttest.GethImage, Digest: agenttest.GethV2, CheckedAt: e.now()}}
	})

	awaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))
}

func TestVerbsBoundTheNeeds(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()
	sc.RebootRequired = true
	sc.NewestKernel = "6.12.48-amd64"

	e := newEnv(t, sc)
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.cfg.Policy.Verbs = []protocol.Verb{protocol.VerbReboot}
	e.start()

	e.ctrl.Await("new digest reported", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Digest == agenttest.GethV2
	})

	req := e.ctrl.AwaitSyncs(8)

	geth := agenttest.ContainerOf(t, &req, "geth")
	require.NotNil(t, geth.Registry)
	assert.Equal(t, agenttest.GethV2, geth.Registry.Digest)
	assert.False(t, geth.Registry.Pulled)
	assert.Empty(t, geth.Registry.Error)
	assert.Empty(t, e.host.Calls(), "nothing is pulled without the roll verb")

	require.Equal(t, []string{protocol.RebootNeedID}, agenttest.NeedIDs(&req))
	assert.Equal(t, []string{protocol.RebootReasonFlag, protocol.RebootReasonKernel}, req.Needs[0].Reboot.Reasons)
}

// A container unlike its deployed block gets a spec need and is recreated onto it.
func TestContainerIsRolledOntoItsSpec(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())

	require.NoError(t, e.host.SetContainerImage("geth", "myfork/geth:debug", "sha256:debug-1"))
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.deploy(map[string]plan.Deployed{"geth": {Image: "docker.io/" + agenttest.GethImage}})
	e.start()

	var need protocol.Need

	e.ctrl.Await("a pulled spec need", func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedSpec {
				need = n

				return true
			}
		}

		return false
	})
	require.NotNil(t, need.Spec)
	assert.Equal(t, "geth", need.Spec.Container)
	assert.Equal(t, agenttest.GethV2, need.Spec.ToDigest)
	assert.Equal(t, "myfork/geth:debug", need.Spec.FromImage)
	assert.False(t, need.Spec.Flags)
	assert.False(t, need.Local, "hosts share the rollout of a spec")

	e.ctrl.Grant(e.lease("lease-spec", need))

	op := e.ctrl.AwaitFinished("lease-spec")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, &op, need.ID).Status)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
	assert.Equal(t, "docker.io/"+agenttest.GethImage, e.host.Container("geth").Image)

	req := e.ctrl.Await("in line with the deployed block", func(req *protocol.SyncRequest) bool {
		return !hasNeed(req, need.ID)
	})
	assert.Empty(t, req.Needs)
}

func TestLocalHold(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	st, err := lease.OpenState(e.log, e.cfg.StateDir)
	require.NoError(t, err)
	require.NoError(t, st.SetHold(plan.HoldLocal, &plan.Hold{By: "pk", Reason: "debugging geth, ask pk"}))

	e.start()

	req := e.ctrl.Await("new digest seen under hold", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Registry != nil && !c.Registry.Pulled && c.Registry.Digest == agenttest.GethV2
	})
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging geth, ask pk"}, req.Hold)
	assert.Empty(t, req.Needs)

	// The controller can neither lift a local hold nor work around it.
	e.ctrl.Direct(
		protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold},
		protocol.Directive{ID: "d-reboot", Kind: protocol.DirectiveReboot},
		protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"},
	)

	req = e.ctrl.Await("directives answered", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-release") != nil && agenttest.AckOf(req, "d-reboot") != nil &&
			agenttest.AckOf(req, "d-roll") != nil
	})
	assert.True(t, req.Hold.Local, "release-hold does not lift the local hold")
	assert.Equal(t, "pk", field(t, e.stateOnDisk(), "holds", "local", "by"))
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-release").Status)
	assert.Equal(t, protocol.DirectiveRejected, agenttest.AckOf(&req, "d-reboot").Status)
	assert.Contains(t, agenttest.AckOf(&req, "d-reboot").Error, "local hold: debugging geth, ask pk")
	assert.Equal(t, protocol.DirectiveRejected, agenttest.AckOf(&req, "d-roll").Status)
	assert.Empty(t, req.Needs)
	e.ctrl.Direct()

	need := agenttest.ImageNeedFor("geth", agenttest.GethImage, agenttest.GethV1, agenttest.GethV2)
	e.ctrl.Grant(e.lease("lease-held", need, agenttest.RebootNeed()))

	op := e.ctrl.AwaitFinished("lease-held")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepRefused, s.Status)
		assert.Contains(t, s.Error, "local hold")
	}

	assert.Empty(t, agenttest.Disruptive(e.host.Calls()))

	// Lifted on the host, the needs come back.
	_, err = local.NewClient(e.cfg.Local.Socket.Path).Hold(t.Context(), &local.HoldRequest{Release: true})
	require.NoError(t, err)

	req = e.ctrl.Await("needs after the hold", func(req *protocol.SyncRequest) bool { return len(req.Needs) == 2 })
	assert.False(t, req.Hold.Active())
	assert.Equal(t, []string{need.ID, protocol.RebootNeedID}, agenttest.NeedIDs(&req))
}

func TestControllerHold(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	stop := e.start()

	e.ctrl.Await("reboot need", func(req *protocol.SyncRequest) bool { return hasNeed(req, protocol.RebootNeedID) })

	e.ctrl.Direct(protocol.Directive{
		ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance",
		By: &protocol.Actor{User: "sam@example.org", Feature: "staff"},
	})

	req := e.ctrl.Await("held", func(req *protocol.SyncRequest) bool { return req.Hold.Controller })
	assert.False(t, req.Hold.Local)
	assert.Empty(t, req.Needs)
	assert.Equal(t, map[string]any{"by": "sam@example.org", "reason": "maintenance", "feature": "staff"},
		field(t, e.stateOnDisk(), "holds", "controller"))

	// The hold outlives the directive and the agent process.
	e.ctrl.Direct()
	stop()
	e.start()

	req = e.ctrl.AwaitSyncs(3)
	assert.True(t, req.Hold.Controller)
	assert.Empty(t, req.Needs)

	e.ctrl.Grant(e.lease("lease-held", agenttest.RebootNeed()))

	op := e.ctrl.AwaitFinished("lease-held")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, op.Steps[0].Status)
	assert.Contains(t, op.Steps[0].Error, "controller hold")
	assert.Empty(t, e.host.Calls())

	e.ctrl.Direct(protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold})

	req = e.ctrl.Await("released", func(req *protocol.SyncRequest) bool {
		return !req.Hold.Active() && hasNeed(req, protocol.RebootNeedID)
	})
	assert.Equal(t, "boot", req.BootID, "releasing a hold does not reboot")
	assert.NotContains(t, e.stateOnDisk(), "holds")
}

func TestControllerDowntimeIsTolerated(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.ctrl.Set(func(c *agenttest.Controller) { c.Down = true })
	e.start()

	time.Sleep(60 * time.Millisecond)
	require.Zero(t, e.ctrl.SyncCount())

	e.ctrl.Set(func(c *agenttest.Controller) { c.Down = false })

	req := e.ctrl.Await("sync after the outage", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.RebootNeedID)
	})
	assert.Equal(t, "boot", req.BootID)
}

// With the controller silent for fallback.after, the containers of a kind
// whose kindPolicies entry rolls follow their tags on the agent's own lease;
// the others wait.
func TestFallbackRollsWithoutTheController(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.cfg.Policy.KindPolicies = map[protocol.ContainerKind]plan.KindPolicy{
		protocol.KindEL: {Roll: true}, protocol.KindCL: {Roll: false},
	}
	e.cfg.Policy.Fallback = plan.Fallback{After: 5 * time.Minute, Interval: time.Minute}
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "rev-2")
	e.host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "rev-t2")
	e.ctrl.Set(func(c *agenttest.Controller) { c.Down = true })
	e.start()

	time.Sleep(60 * time.Millisecond)
	require.NotContains(t, e.host.Calls(), "pull "+agenttest.GethImage, "not silent for long enough")

	// The first pull is at a random point of the interval.
	e.offset.Add(int64(6 * time.Minute))
	require.Eventually(t, func() bool {
		if slices.Contains(e.host.Calls(), "pull "+agenttest.GethImage) {
			return true
		}

		e.offset.Add(int64(20 * time.Second))

		return false
	}, 5*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return e.host.Container("geth").Digest == agenttest.GethV2
	}, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, agenttest.TekuV1, e.host.Container("teku").Digest)
	assert.NotContains(t, e.host.Calls(), "pull "+agenttest.TekuImage)
	require.Zero(t, e.ctrl.SyncCount())

	// The controller hears of the operation when it is back.
	e.ctrl.Set(func(c *agenttest.Controller) { c.Down = false })

	req := e.ctrl.Await("the fallback's operation", func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.Phase.Terminal()
	})
	assert.True(t, strings.HasPrefix(req.Operation.LeaseID, "fallback-"))
	assert.Equal(t, protocol.PhaseDone, req.Operation.Phase, req.Operation.Error)

	e.ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
}

func TestInventoryIsSentWhenAsked(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := newEnv(t, sc)
	e.start()

	req := e.ctrl.AwaitSyncs(2)

	assert.Empty(t, e.ctrl.Inventories(), "no inventory unless asked")
	e.ctrl.Set(func(c *agenttest.Controller) { c.WantInventory = true })

	require.Eventually(t, func() bool {
		return len(e.ctrl.Inventories()) > 0
	}, agenttest.AwaitTimeout, 2*time.Millisecond)

	inv := e.ctrl.Inventories()[0]
	e.ctrl.Set(func(c *agenttest.Controller) { c.WantInventory = false })

	hash, err := inv.ComputeHash()
	require.NoError(t, err)
	assert.Equal(t, hash, inv.Hash)
	assert.Equal(t, req.InventoryHash, inv.Hash, "the sync announces the hash of this inventory")
	assert.Len(t, inv.Packages, 3)
	assert.Len(t, inv.Upgradable, 2)
}

// hangingHost is a host whose package query never returns by itself.
type hangingHost struct {
	*agenttest.Host
}

func (hangingHost) Inventory(ctx context.Context) (*protocol.Inventory, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func TestInventoryCollectionIsBounded(t *testing.T) {
	t.Parallel()

	h, err := agenttest.NewHost(agenttest.BaseScenario())
	require.NoError(t, err)

	log, logs := logtest.NewNullLogger()
	a := &agent{log: log, host: hangingHost{h}, inventoryTimeout: 20 * time.Millisecond}

	done := make(chan struct{})

	go func() {
		defer close(done)

		a.sendInventory(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(agenttest.AwaitTimeout):
		require.FailNow(t, "sendInventory waits for a hanging inventory without a bound")
	}

	entry := logs.LastEntry()
	require.NotNil(t, entry)
	assert.Equal(t, "cannot collect the inventory", entry.Message)

	logged, ok := entry.Data[logrus.ErrorKey].(error)
	require.True(t, ok)
	assert.ErrorIs(t, logged, context.DeadlineExceeded)
}

func TestSyncReportsTheRoles(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.cfg.Roles = []string{"bootnode"}
	e.start()

	req := e.ctrl.Await("a sync", func(*protocol.SyncRequest) bool { return true })
	assert.Equal(t, []string{"bootnode"}, req.Roles)
}

// A declined lease leaves its needs open: they are reported again.
func TestDeclinedLeaseLeavesTheNeedsOpen(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.RebootNeedID)

	late := e.lease("lease-late", needs...)
	late.ExpiresAt = e.now().Add(-time.Second)
	e.ctrl.Grant(late)

	op := e.ctrl.AwaitFinished("lease-late")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	req := e.ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
	assert.Equal(t, []string{needs[0].ID, protocol.RebootNeedID}, agenttest.NeedIDs(&req))
	assert.Empty(t, agenttest.Disruptive(e.host.Calls()))
}

// After a served lease: no blocked upgrade, no need, the impacts of the
// rolled containers: node for a labelled one, independent for an unlabelled
// one under defaultPolicy roll.
func TestServedLeaseIsReported(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()
	sc.Containers[3].Kind = ""

	e := newEnv(t, sc)
	e.cfg.Policy.DefaultPolicy = plan.DefaultRoll
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.host.SetRegistry("ethpandaops/dora:latest", "sha256:dora-2", "")
	e.start()

	needs := awaitNeeds(t, e, agenttest.PackagesNeed().ID,
		protocol.ImageNeedID("dora", "sha256:dora-2"), protocol.ImageNeedID("geth", agenttest.GethV2))
	assert.Empty(t, needs[1].Image.Kind)
	assert.Equal(t, protocol.KindEL, needs[2].Image.Kind)
	assert.Equal(t, agenttest.TeamGeth, needs[2].Image.Team)

	e.ctrl.Grant(e.lease("lease-all", needs...))

	op := e.ctrl.AwaitFinished("lease-all")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.KindEL, e.host.Container("geth").Kind, "the recreate kept the labels")

	req := e.ctrl.Await("all served", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Zero(t, req.Pending.Blocked)
	assert.Equal(t, protocol.ImpactNode, agenttest.ContainerOf(t, &req, "geth").Impact)
	assert.Empty(t, agenttest.ContainerOf(t, &req, "dora").Kind)
	assert.Equal(t, protocol.ImpactIndependent, agenttest.ContainerOf(t, &req, "dora").Impact)
}

// A hand-written override is applied, its tag followed, and put back once gone.
func TestOverrideByHand(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.start()
	e.ctrl.AwaitSyncs(2)

	e.writeOverrides("overrides:\n  geth:\n    image: " + agenttest.FixImage + "\n    command:\n" +
		"      - --http\n      - --pprof\n    by: sam\n    reason: sync\n")

	need := awaitOverrideNeed(t, e, "geth", false)
	assert.True(t, need.Local)
	assert.Equal(t, &protocol.OverrideNeed{
		Container: "geth", Image: agenttest.FixImage, FromImage: agenttest.GethImage,
		FromDigest: agenttest.GethV1, ToDigest: agenttest.FixV1, Flags: true,
	}, need.Override)

	req := e.ctrl.Await("the override is reported", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override != nil
	})
	geth := agenttest.ContainerOf(t, &req, "geth")
	assert.False(t, geth.Override.Applied)
	assert.Equal(t, "sam", geth.Override.By)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.Command)
	assert.Empty(t, agenttest.ContainerOf(t, &req, "dora").Command, "no command line of an unmanaged container")

	e.ctrl.Grant(e.lease("lease-override", need))

	op := e.ctrl.AwaitFinished("lease-override")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	req = e.ctrl.Await("the override is applied", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Override != nil && c.Override.Applied && len(req.Needs) == 0
	})
	geth = agenttest.ContainerOf(t, &req, "geth")
	assert.Equal(t, agenttest.FixImage, geth.Image)
	assert.Equal(t, agenttest.FixV1, geth.Digest)
	assert.Equal(t, agenttest.GethImage, geth.ConfiguredImage)
	assert.Equal(t, []string{"--http", "--pprof"}, geth.Command)

	// The override's tag moves: an image need like any tag.
	e.host.SetRegistry(agenttest.FixImage, agenttest.FixV2, "")

	follow := awaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.FixV2))
	assert.Equal(t, protocol.NeedImage, follow[0].Kind)
	assert.Equal(t, agenttest.FixImage, follow[0].Image.Image)

	e.writeOverrides("# nothing\noverrides:\n")

	back := awaitOverrideNeed(t, e, "geth", true)
	assert.Equal(t, agenttest.GethImage, back.Override.Image, "what the container was created with")
	assert.True(t, back.Override.Flags)

	req = e.ctrl.Await("restoring is reported", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Override != nil && c.Override.Restoring
	})
	assert.Equal(t, agenttest.FixImage, agenttest.ContainerOf(t, &req, "geth").Image)

	e.ctrl.Grant(e.lease("lease-restore", back))

	op = e.ctrl.AwaitFinished("lease-restore")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	req = e.ctrl.Await("the container is back", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override == nil && len(req.Needs) == 0
	})
	geth = agenttest.ContainerOf(t, &req, "geth")
	assert.Equal(t, agenttest.GethImage, geth.Image)
	assert.Equal(t, agenttest.GethV1, geth.Digest)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.Command)
	assert.Nil(t, e.host.Container("geth").Base, "the labels are gone with the override")

	records := e.auditRecords()
	assert.Equal(t, []string{
		"override.edit accepted", "override.apply done", "override.edit accepted", "override.restore done",
	}, auditActions(records))
	assert.Equal(t, "lease-override", records[1].Lease)
	assert.Equal(t, "geth", records[1].Container)
}

// An expired override is taken out of the file and the container put back.
func TestOverrideExpires(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.start()
	e.ctrl.AwaitSyncs(2)

	until := e.now().Add(time.Hour).UTC().Format(time.RFC3339)
	e.writeOverrides("# mine\noverrides:\n  geth:\n    image: " + agenttest.FixImage + "\n    by: sam\n" +
		"    reason: sync\n    until: " + until + "\n  teku:\n    command: [\"--x\"]\n")

	need := awaitOverrideNeed(t, e, "geth", false)
	teku := awaitOverrideNeed(t, e, "teku", false)
	e.ctrl.Grant(e.lease("lease-1", need, teku))
	require.Equal(t, protocol.PhaseDone, e.ctrl.AwaitFinished("lease-1").Phase)

	e.offset.Add(int64(2 * time.Hour))

	back := awaitOverrideNeed(t, e, "geth", true)

	content := e.readOverrides()
	assert.NotContains(t, content, "    image: "+agenttest.FixImage)
	assert.Contains(t, content, "upgradoor: expired; was image "+agenttest.FixImage+" by sam\n"+
		"  geth:\n    # default image: "+agenttest.GethImage+"\n")
	assert.Contains(t, content, "    #   - --maxpeers=50\n    # image:\n    # command:\n    # reason:\n    # until:\n",
		"the structure is back")
	assert.True(t, strings.HasPrefix(content, "# upgradoor overrides: "))
	assert.Contains(t, content, "\n# mine\noverrides:\n")
	assert.Contains(t, content, "    # default command: []\n    command: [\"--x\"]\n", "the other entry is as it was written")

	e.ctrl.Grant(e.lease("lease-2", back))
	require.Equal(t, protocol.PhaseDone, e.ctrl.AwaitFinished("lease-2").Phase)

	req := e.ctrl.Await("geth is back, teku stays", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override == nil && len(req.Needs) == 0
	})
	assert.Equal(t, agenttest.GethImage, agenttest.ContainerOf(t, &req, "geth").Image)
	assert.True(t, agenttest.ContainerOf(t, &req, "teku").Override.Applied)
	assert.Contains(t, auditActions(e.auditRecords()), "override.expire accepted")
}

// An unknown image, a broken file or a stale lease changes nothing.
func TestOverrideThatCannotBeApplied(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.start()
	e.ctrl.AwaitSyncs(2)

	e.writeOverrides("overrides:\n  geth:\n    image: ethpandaops/geth:nope\n")

	req := e.ctrl.Await("the override is reported", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override != nil
	})
	assert.False(t, agenttest.ContainerOf(t, &req, "geth").Override.Applied)

	req = e.ctrl.AwaitSyncs(3)
	assert.Empty(t, req.Needs)

	e.writeOverrides("overrides: [geth\n")

	req = e.ctrl.AwaitSyncs(3)
	assert.Empty(t, req.Needs)
	assert.Nil(t, agenttest.ContainerOf(t, &req, "geth").Override)
	assert.True(t, req.Hold.Local, "a file that cannot be read holds the host")

	e.writeOverrides("overrides:\n  geth:\n    image: " + agenttest.FixImage + "\n")
	need := awaitOverrideNeed(t, e, "geth", false)

	e.writeOverrides("overrides:\n  geth:\n    image: " + agenttest.FixImage + "\n    command: [\"--y\"]\n")
	e.ctrl.Await("another override need", func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedOverride && n.ID != need.ID {
				return true
			}
		}

		return false
	})

	e.ctrl.Grant(e.lease("lease-stale", need))

	op := e.ctrl.AwaitFinished("lease-stale")
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepSuperseded, agenttest.StepOf(t, &op, need.ID).Status)
	assert.Contains(t, agenttest.StepOf(t, &op, need.ID).Error, "asks for something else now")
	assert.Empty(t, agenttest.Disruptive(e.host.Calls()))
}

// signAs signs an answer with the given signer.
func signAs(t *testing.T, s *identity.AnswerSigner, host, id string, body []byte) []byte {
	t.Helper()

	out, err := s.Sign(host, id, body)
	require.NoError(t, err)

	return out
}

// A refused answer is acted on in no part; the controller's own is.
func TestRefusedAnswersAreNotActedOn(t *testing.T) {
	t.Parallel()

	tests := map[string]func(t *testing.T) func(answer []byte, host, proofID string) []byte{
		"unsigned": func(*testing.T) func([]byte, string, string) []byte {
			return func(answer []byte, _, _ string) []byte { return answer }
		},
		"for another host": func(t *testing.T) func([]byte, string, string) []byte {
			t.Helper()

			signer := agenttest.AnswerSigner(t, agenttest.ControllerSeed)

			return func(answer []byte, _, proofID string) []byte {
				return signAs(t, signer, "prysm-besu-1", proofID, answer)
			}
		},
		"body altered": func(t *testing.T) func([]byte, string, string) []byte {
			t.Helper()

			signer := agenttest.AnswerSigner(t, agenttest.ControllerSeed)

			return func(answer []byte, host, proofID string) []byte {
				harmless, err := json.Marshal(protocol.SyncResponse{})
				require.NoError(t, err)

				var signed identity.SignedAnswer
				require.NoError(t, json.Unmarshal(signAs(t, signer, host, proofID, harmless), &signed))

				signed.Payload = base64.RawURLEncoding.EncodeToString(answer)

				out, err := json.Marshal(signed)
				require.NoError(t, err)

				return out
			}
		},
		"a key the host does not accept": func(t *testing.T) func([]byte, string, string) []byte {
			t.Helper()

			signer := agenttest.AnswerSigner(t, "another controller seed, at least 32 bytes long")

			return func(answer []byte, host, proofID string) []byte {
				return signAs(t, signer, host, proofID, answer)
			}
		},
	}

	for name, forge := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sc := agenttest.BaseScenario()
			sc.RebootRequired = true
			sc.Packages.Upgradable = agenttest.DockerUpgrade()

			e := newEnv(t, sc)
			e.ctrl.Set(func(c *agenttest.Controller) {
				c.Forge = forge(t)
				c.Lease = e.lease("lease-1", agenttest.RebootNeed())
				c.Directives = []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "forged"}}
				c.WantInventory = true
			})
			e.start()

			e.ctrl.AwaitSyncs(8)

			for _, req := range e.ctrl.Syncs() {
				require.Nil(t, req.Operation, "no lease is taken")
				require.Empty(t, req.DirectiveAcks, "no directive is answered")
				require.False(t, req.Hold.Controller, "no directive is applied")
			}

			assert.Empty(t, agenttest.Disruptive(e.host.Calls()))
			assert.Nil(t, e.stateOnDisk()["current"], "no lease is persisted")
			assert.Empty(t, e.ctrl.Inventories())

			e.ctrl.Set(func(c *agenttest.Controller) {
				c.Forge = nil
				c.Directives = nil
				c.WantInventory = false
			})

			op := e.ctrl.AwaitFinished("lease-1")
			require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
			assert.Contains(t, e.host.Calls(), "reboot")
		})
	}
}

// Without controller keys a signed answer is acted on in no part.
func TestSignedAnswersAreRefusedWithoutControllerKeys(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.cfg.Controller.PublicKey = ""
	e.cfg.Controller.AllowUnsignedAnswers = true

	e.ctrl.Set(func(c *agenttest.Controller) {
		c.Directives = []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}}
	})
	e.start()

	e.ctrl.AwaitSyncs(4)

	for _, req := range e.ctrl.Syncs() {
		require.Empty(t, req.DirectiveAcks, "no directive is answered")
		require.False(t, req.Hold.Controller, "no directive is applied")
	}

	assert.NotContains(t, e.readOverridesIfAny(), "controller:")
}

// readOverridesIfAny is the overrides file, "" when there is none.
func (e *env) readOverridesIfAny() string {
	raw, err := os.ReadFile(e.cfg.Local.OverridesFile)
	if err != nil {
		return ""
	}

	return string(raw)
}

// While syncs fail the loop wakes when the silence reaches fallback.after,
// however far the sync backoff has grown.
func TestUntilFallback(t *testing.T) {
	t.Parallel()

	now := time.Now()
	a := &agent{
		cfg: &Config{Policy: plan.Policy{
			KindPolicies: map[protocol.ContainerKind]plan.KindPolicy{protocol.KindEL: {Roll: true}},
			Fallback:     plan.Fallback{After: time.Hour},
		}},
		now:         func() time.Time { return now },
		syncFailing: true,
		lastAnswer:  now.Add(-50 * time.Minute),
	}

	assert.Equal(t, 10*time.Minute, a.untilFallback())

	a.fallbackTried = map[string]struct{}{}
	assert.LessOrEqual(t, a.untilFallback(), time.Duration(0), "begun already")

	a.fallbackTried, a.syncFailing = nil, false
	assert.LessOrEqual(t, a.untilFallback(), time.Duration(0), "the controller answers")

	a.syncFailing, a.cfg.Policy.KindPolicies = true, nil
	assert.LessOrEqual(t, a.untilFallback(), time.Duration(0), "no fallback")
}
