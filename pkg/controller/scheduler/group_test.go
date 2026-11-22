package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const imgVC = "sigp/lighthouse-vc:unstable"

// wantsInGroup makes the host run a container of the group and client on
// image at digestOld, with a need for the digest to.
func (b *worldBuilder) wantsInGroup(host, name, group, client, image, to string) {
	h := b.get(host)
	c := container(name, image, digestOld, to, b.w.Now)
	c.Group, c.Client = group, client
	h.Status.Containers = append(h.Status.Containers, c)

	n := imageNeed(name, image, to)
	n.Image.Group, n.Image.Client = group, client
	h.Status.Needs = append(h.Status.Needs, n)
}

// openGroup returns the one open rollout of a group.
func (b *worldBuilder) openGroup(group, client string) *fleet.Rollout {
	b.t.Helper()

	var found *fleet.Rollout

	for _, r := range b.w.Rollouts {
		if r.Kind == fleet.RolloutGroup && r.Group == group && r.Client == client && !r.State.Terminal() {
			require.Nil(b.t, found, "two open rollouts of group %s", group)
			found = r
		}
	}

	require.NotNil(b.t, found, "no open rollout of group %s", group)

	return found
}

// A beacon node and its validator client in one group are one rollout, and
// one lease per host takes both.
func TestGroupIsServedByOneLease(t *testing.T) {
	b := newWorldBuilder(t)

	for _, h := range []string{"a", "b"} {
		b.host(h, 10)
		b.wantsInGroup(h, "beacon", "consensus", "lighthouse", imgLighthouse, digestA)
		b.wantsInGroup(h, "validator", "consensus", "lighthouse", imgVC, digestB)
	}

	b.step()

	r := b.openGroup("consensus", "lighthouse")
	assert.Len(t, r.Targets, 2, "both tags are the group's target")
	assert.Equal(t, "lighthouse", r.Client)

	require.Len(t, b.w.Leases, 1, "one canary")
	assert.Len(t, b.w.Leases[0].Needs, 2, "the canary's lease takes the whole group")
}

// The default group is a group per image: all hosts' needs of one image are
// one rollout, under the default group's own policy, and its work is
// independent: the closed gate holds nothing back.
func TestDefaultGroupIsOneRolloutPerImage(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyImage = fleet.PolicyManual
	b.w.Health.FinalityLagEpochs = 5

	for _, h := range []string{"a", "b"} {
		b.host(h, 10)
		b.wantsInGroup(h, "tools", protocol.DefaultGroup, "", imgLighthouse, digestA)
		b.wantsInGroup(h, "proxy", protocol.DefaultGroup, "", imgVC, digestB)

		for i := range b.get(h).Status.Containers {
			b.get(h).Status.Containers[i].Impact = protocol.ImpactIndependent
		}
	}

	b.step()

	var open []*fleet.Rollout

	for _, r := range b.w.Rollouts {
		if r.Group == protocol.DefaultGroup && !r.State.Terminal() {
			open = append(open, r)
		}
	}

	require.Len(t, open, 2, "one rollout per image")

	for _, r := range open {
		require.Len(t, r.Targets, 1)
		assert.Equal(t, fleet.PolicyAuto, rolloutPolicy(&b.w.Settings, r), "not the image policy")
	}

	require.NotEmpty(t, b.w.Leases, "independent work passes the closed gate")
	assert.Equal(t, protocol.ImpactIndependent, b.w.Leases[0].Impact)

	b.w.Settings.PolicyDefaultGroup = fleet.PolicyManual
	assert.Equal(t, fleet.PolicyManual, rolloutPolicy(&b.w.Settings, open[0]))
}

// A second tag of the group moving while its rollout is open supersedes it
// with the combined target.
func TestGroupTargetCombines(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 10)
	b.host("b", 10)
	b.wantsInGroup("a", "beacon", "consensus", "", imgLighthouse, digestA)
	b.wantsInGroup("b", "beacon", "consensus", "", imgLighthouse, digestA)
	b.step()

	first := b.openGroup("consensus", "")
	require.Len(t, first.Targets, 1)

	b.wantsInGroup("b", "validator", "consensus", "", imgVC, digestB)
	b.advance(time.Second)
	b.step()

	next := b.openGroup("consensus", "")
	assert.NotEqual(t, first.ID, next.ID)
	assert.Len(t, next.Targets, 2, "the combined target")

	closed := b.closed(first.Key)
	require.NotNil(t, closed)
	assert.Equal(t, fleet.RolloutSuperseded, closed.State)
}

// A host whose needs of the group are not all in the rollout's target (it
// has not heard of the newest digest yet) is served none of them.
func TestGroupNeedsAllOrNone(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 10)
	b.host("b", 10)
	b.wantsInGroup("a", "beacon", "consensus", "", imgLighthouse, digestA)
	b.wantsInGroup("a", "validator", "consensus", "", imgVC, digestC)
	b.wantsInGroup("b", "beacon", "consensus", "", imgLighthouse, digestA)
	b.wantsInGroup("b", "validator", "consensus", "", imgVC, digestB)
	// a looked at the registry later: its digest of the validator image wins.
	b.get("b").Status.Containers[1].Registry.CheckedAt = b.w.Now.Add(-time.Minute)

	b.step()

	r := b.openGroup("consensus", "")
	assert.Equal(t, fleet.HostLeased, memberStates(r)["a"])
	assert.Equal(t, fleet.HostExpected, memberStates(r)["b"],
		"b asks for another digest of the validator image: none of its needs is served yet")
	assert.Nil(t, b.lease("b"))
}

// Independent work of another rollout rides along with a node lease the host
// is granted anyway.
func TestReadyNeedsRideAlong(t *testing.T) {
	b := newWorldBuilder(t)

	// The lighter host is the canary of both rollouts.
	b.host("a", 10)
	b.host("b", 20)
	b.wantsInGroup("a", "geth", "execution", "", imgGeth, digestA)
	b.wantsInGroup("b", "geth", "execution", "", imgGeth, digestA)
	b.wantsInGroup("a", "exporter", "tools", "", "org/exporter:latest", digestB)
	b.wantsInGroup("b", "exporter", "tools", "", "org/exporter:latest", digestB)
	b.get("a").Status.Containers[1].Impact = protocol.ImpactIndependent
	b.get("b").Status.Containers[1].Impact = protocol.ImpactIndependent

	b.step()

	l := b.lease("a")
	require.NotNil(t, l, "a is a canary")
	assert.Len(t, l.Needs, 2, "the exporter rides along with the node lease")
}
