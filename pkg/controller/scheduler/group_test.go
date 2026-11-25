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

// wantsInGroup makes the host run a container of the kind and team on image
// at digestOld, with a need for the digest to.
func (b *worldBuilder) wantsInGroup(host, name string, kind protocol.ContainerKind, team, image, to string) {
	h := b.get(host)
	c := container(name, image, digestOld, to, b.w.Now)
	c.Kind, c.Team = kind, team
	h.Status.Containers = append(h.Status.Containers, c)

	n := imageNeed(name, image, to)
	n.Image.Kind, n.Image.Team = kind, team
	h.Status.Needs = append(h.Status.Needs, n)
}

// deploysInGroup makes the host run a container of the kind and team that
// Ansible deployed anew: a spec need for image at digest to.
func (b *worldBuilder) deploysInGroup(host, name string, kind protocol.ContainerKind, team, image, to string) {
	h := b.get(host)
	c := container(name, image, digestOld, digestOld, b.w.Now)
	c.Kind, c.Team = kind, team
	h.Status.Containers = append(h.Status.Containers, c)

	key := image + "@" + to
	h.Status.Needs = append(h.Status.Needs, protocol.Need{
		ID:   protocol.SpecNeedID(name, key),
		Kind: protocol.NeedSpec,
		Spec: &protocol.SpecNeed{
			Container: name, Kind: kind, Team: team, Key: key, Image: image, FromDigest: digestOld, ToDigest: to,
		},
	})
}

// openGroup returns the one open rollout of a kind and team.
func (b *worldBuilder) openGroup(kind protocol.ContainerKind, team string) *fleet.Rollout {
	b.t.Helper()

	var found *fleet.Rollout

	for _, r := range b.w.Rollouts {
		if r.Kind == fleet.RolloutGroup && r.ContainerKind == kind && r.Team == team && !r.State.Terminal() {
			require.Nil(b.t, found, "two open rollouts of %s %s", team, kind)
			found = r
		}
	}

	require.NotNil(b.t, found, "no open rollout of %s %s", team, kind)

	return found
}

// A beacon node and its validator client are rollouts of their own kinds;
// active together, one lease per host takes both.
func TestKindsRideAlongInOneLease(t *testing.T) {
	b := newWorldBuilder(t)

	for _, h := range []string{"a", "b"} {
		b.host(h, 10)
		b.wantsInGroup(h, "beacon", protocol.KindCL, "lighthouse", imgLighthouse, digestA)
		b.wantsInGroup(h, "validator", protocol.KindVC, "lighthouse", imgVC, digestB)
	}

	b.step()

	cl, vc := b.openGroup(protocol.KindCL, "lighthouse"), b.openGroup(protocol.KindVC, "lighthouse")
	assert.Equal(t, UnitName(protocol.KindCL, "lighthouse"), rolloutGroupName(cl))
	assert.Contains(t, cl.Title, "lighthouse cl: ")
	assert.Contains(t, vc.Title, "lighthouse vc: ")

	require.Len(t, b.w.Leases, 1, "one canary")
	assert.Len(t, b.w.Leases[0].Needs, 2, "the canary's lease takes both kinds: the host restarts once")
	assert.Len(t, b.w.Leases[0].Rollouts, 2)
}

// A rollout is per team and kind; containers without a team are one rollout
// per kind across the fleet. A team's rollouts answer to its policy, the
// others to policyImage.
func TestRolloutPerTeamAndKind(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyClients = fleet.ClientPolicies{"geth": fleet.PolicyManual}

	b.host("a", 10)
	b.host("b", 10)
	b.wantsInGroup("a", "geth", protocol.KindEL, "geth", imgGeth, digestA)
	b.wantsInGroup("b", "besu", protocol.KindEL, "besu", "hyperledger/besu:develop", digestA)
	b.wantsInGroup("a", "snooper", "snooper-el", "", "ethpandaops/snooper:latest", digestB)
	b.wantsInGroup("b", "snooper", "snooper-el", "", "ethpandaops/snooper:latest", digestB)

	b.step()

	geth, besu := b.openGroup(protocol.KindEL, "geth"), b.openGroup(protocol.KindEL, "besu")
	snooper := b.openGroup("snooper-el", "")
	assert.Len(t, snooper.Hosts, 2, "one global rollout of the kind")
	assert.Contains(t, snooper.Title, "snooper-el: ")
	assert.Equal(t, UnitName("snooper-el", ""), rolloutGroupName(snooper))

	assert.Equal(t, fleet.PolicyManual, b.w.Settings.RolloutPolicy(geth))
	assert.Equal(t, fleet.PolicyAuto, b.w.Settings.RolloutPolicy(besu))
	assert.Equal(t, fleet.PolicyAuto, b.w.Settings.RolloutPolicy(snooper))

	b.w.Settings.PolicyImage = fleet.PolicyManual
	assert.Equal(t, fleet.PolicyManual, b.w.Settings.RolloutPolicy(snooper), "policyImage")
}

// Unlabelled containers are one rollout per image, under policyImage, and
// their work is independent: the closed gate holds nothing back.
func TestUnlabelledIsOneRolloutPerImage(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Health.FinalityLagEpochs = 5

	for _, h := range []string{"a", "b"} {
		b.host(h, 10)
		b.wantsInGroup(h, "tools", "", "", imgLighthouse, digestA)
		b.wantsInGroup(h, "proxy", "", "", imgVC, digestB)

		for i := range b.get(h).Status.Containers {
			b.get(h).Status.Containers[i].Impact = protocol.ImpactIndependent
		}
	}

	b.step()

	var open []*fleet.Rollout

	for _, r := range b.w.Rollouts {
		if r.Kind == fleet.RolloutGroup && r.ContainerKind == "" && !r.State.Terminal() {
			open = append(open, r)
		}
	}

	require.Len(t, open, 2, "one rollout per image")

	for _, r := range open {
		require.Len(t, r.Targets, 1)
		assert.Contains(t, r.Title, "unlabelled: ")
	}

	require.NotEmpty(t, b.w.Leases, "independent work passes the closed gate")
	assert.Equal(t, protocol.ImpactIndependent, b.w.Leases[0].Impact)

	b.w.Settings.PolicyImage = fleet.PolicyManual
	assert.Equal(t, fleet.PolicyManual, b.w.Settings.RolloutPolicy(open[0]), "policyImage")
}

// A second tag of the unit moving while its rollout is open supersedes it
// with the combined target.
func TestGroupTargetCombines(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 10)
	b.host("b", 10)
	b.wantsInGroup("a", "beacon", protocol.KindCL, "", imgLighthouse, digestA)
	b.wantsInGroup("b", "beacon", protocol.KindCL, "", imgLighthouse, digestA)
	b.step()

	first := b.openGroup(protocol.KindCL, "")
	require.Len(t, first.Targets, 1)

	b.wantsInGroup("b", "sidecar", protocol.KindCL, "", imgVC, digestB)
	b.advance(time.Second)
	b.step()

	next := b.openGroup(protocol.KindCL, "")
	assert.NotEqual(t, first.ID, next.ID)
	assert.Len(t, next.Targets, 2, "the combined target")

	closed := b.closed(first.Key)
	require.NotNil(t, closed)
	assert.Equal(t, fleet.RolloutSuperseded, closed.State)
}

// A host whose needs of the unit are not all in the rollout's target (it
// has not heard of the newest digest yet) is served none of them.
func TestGroupNeedsAllOrNone(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 10)
	b.host("b", 10)
	b.wantsInGroup("a", "beacon", protocol.KindCL, "", imgLighthouse, digestA)
	b.wantsInGroup("a", "sidecar", protocol.KindCL, "", imgVC, digestC)
	b.wantsInGroup("b", "beacon", protocol.KindCL, "", imgLighthouse, digestA)
	b.wantsInGroup("b", "sidecar", protocol.KindCL, "", imgVC, digestB)
	// a looked at the registry later: its digest of the validator image wins.
	b.get("b").Status.Containers[1].Registry.CheckedAt = b.w.Now.Add(-time.Minute)

	b.step()

	r := b.openGroup(protocol.KindCL, "")
	assert.Equal(t, fleet.HostLeased, memberStates(r)["a"])
	assert.Equal(t, fleet.HostExpected, memberStates(r)["b"],
		"b asks for another digest of the sidecar image: none of its needs is served yet")
	assert.Nil(t, b.lease("b"))
}

// Independent work of another rollout rides along with a node lease the host
// is granted anyway.
func TestReadyNeedsRideAlong(t *testing.T) {
	b := newWorldBuilder(t)

	// The lighter host is the canary of both rollouts.
	b.host("a", 10)
	b.host("b", 20)
	b.wantsInGroup("a", "geth", protocol.KindEL, "", imgGeth, digestA)
	b.wantsInGroup("b", "geth", protocol.KindEL, "", imgGeth, digestA)
	b.wantsInGroup("a", "exporter", "", "", "org/exporter:latest", digestB)
	b.wantsInGroup("b", "exporter", "", "", "org/exporter:latest", digestB)
	b.get("a").Status.Containers[1].Impact = protocol.ImpactIndependent
	b.get("b").Status.Containers[1].Impact = protocol.ImpactIndependent

	b.step()

	l := b.lease("a")
	require.NotNil(t, l, "a is a canary")
	assert.Len(t, l.Needs, 2, "the exporter rides along with the node lease")
}

// A tag that moves while an Ansible deploy of the unit is open supersedes it
// with a rollout that answers to the image policy again.
func TestDeployWithAMovedTagAnswersToTheImagePolicy(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyImage = fleet.PolicyManual
	b.host("a", 10)
	b.host("b", 10)
	b.deploysInGroup("a", "geth", protocol.KindEL, "geth", imgGeth, digestA)
	b.deploysInGroup("b", "geth", protocol.KindEL, "geth", imgGeth, digestA)
	b.step()

	first := b.openGroup(protocol.KindEL, "geth")
	assert.Equal(t, fleet.PolicyAuto, b.w.Settings.RolloutPolicy(first))

	b.wantsInGroup("b", "exporter", protocol.KindEL, "geth", "org/exporter:latest", digestB)
	b.advance(time.Second)
	b.step()

	next := b.openGroup(protocol.KindEL, "geth")
	assert.NotEqual(t, first.ID, next.ID)
	assert.Len(t, next.Targets, 2)
	assert.Equal(t, fleet.PolicyManual, b.w.Settings.RolloutPolicy(next))
}
