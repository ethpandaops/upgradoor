package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func specNeed(container, key, image, to string) protocol.Need {
	return protocol.Need{
		ID:   protocol.SpecNeedID(container, key),
		Kind: protocol.NeedSpec,
		Spec: &protocol.SpecNeed{Container: container, Key: key, Image: image, ToDigest: to},
	}
}

// wantsSpec makes the host run container on another spec than agent.yaml
// says and report the need to move it.
func (b *worldBuilder) wantsSpec(host, container, key string, impact protocol.Impact) {
	h := b.get(host)
	c := protocol.Container{Name: container, Image: "org/x:old", Managed: true, Impact: impact}
	h.Status.Containers = append(h.Status.Containers, c)
	h.Status.Needs = append(h.Status.Needs, specNeed(container, key, "org/x:new", digestA))
}

// Hosts whose container agent.yaml moves to the same spec share one rollout,
// staggered like any other: canary first, then the rest.
func TestSpecRolloutIsShared(t *testing.T) {
	b := newWorldBuilder(t)

	for _, h := range []string{"h1", "h2", "h3"} {
		b.host(h, 10)
		b.wantsSpec(h, "exporter", "k1", protocol.ImpactIndependent)
	}

	b.step()

	r := b.mustRollout(specKey("exporter", "k1"))
	assert.Equal(t, fleet.RolloutSpec, r.Kind)
	assert.Equal(t, "exporter", r.Container)
	assert.Equal(t, "exporter to org/x:new "+shortDigest(digestA), r.Title)
	assert.Len(t, r.Hosts, 3)
	require.Equal(t, []string{"h1"}, b.leasedHosts(), "the canary")
	assert.Equal(t, protocol.ImpactIndependent, b.lease("h1").Impact)

	b.finish("h1")
	b.step()
	b.advance(5 * time.Second)
	b.step()

	assert.Equal(t, []string{"h2"}, b.leasedHosts(), "the independent limit is one host of three")
}

// A rollout of a spec that nobody moves to any more is superseded by the
// next spec of the container.
func TestSpecRolloutIsSuperseded(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.Paused = true
	b.host("h1", 10)
	b.wantsSpec("h1", "geth", "k1", protocol.ImpactNode)
	b.step()

	first := b.mustRollout(specKey("geth", "k1"))

	b.get("h1").Status.Needs = []protocol.Need{specNeed("geth", "k2", "org/x:newer", digestB)}
	b.step()

	assert.Equal(t, fleet.RolloutSuperseded, b.closed(first.Key).State)
	assert.NotNil(t, b.mustRollout(specKey("geth", "k2")))
}

func TestSpecRolloutAnswersToTheImagePolicy(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyImage = fleet.PolicyManual
	b.host("h1", 10)
	b.wantsSpec("h1", "geth", "k1", protocol.ImpactNode)

	out := b.step()

	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, reasonApproval, queueReasons(out.Plan)["h1"])
}
