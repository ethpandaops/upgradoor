package scheduler

import (
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// prefixSpec starts the rollout key of a spec and the group of the spec
// rollouts of one container.
const prefixSpec = "spec:"

// specKey is the rollout key of a container moving to one spec: every host
// whose container agent.yaml moves to the same image, digest and change of
// the command line shares it.
func specKey(container, key string) string {
	return prefixSpec + container + "@" + key
}

// specGroup is the group of the spec rollouts of one container.
func specGroup(container string) string {
	return prefixSpec + container
}

// specRollout is the rollout a spec need opens.
func specRollout(n *protocol.SpecNeed, client string) *fleet.Rollout {
	title := n.Container + " to " + n.Image + " " + shortDigest(n.ToDigest)
	if n.Flags {
		title += ", command line changed"
	}

	return &fleet.Rollout{
		Kind:      fleet.RolloutSpec,
		Key:       specKey(n.Container, n.Key),
		Title:     title,
		Container: n.Container,
		Image:     n.Image,
		Digest:    n.ToDigest,
		Revision:  n.ToRevision,
		Client:    client,
	}
}

// supersedeSpec closes a spec rollout whose container nobody moves to its
// spec any more while another spec of the container is reported: agent.yaml
// moved on before the rollout was done.
func (p *pass) supersedeSpec(r *fleet.Rollout, rep *reportedNeeds) {
	if _, ok := rep.specs[r.Key]; ok {
		return
	}

	for _, s := range rep.specs {
		if s.Container == r.Container {
			p.closeRollout(r, fleet.RolloutSuperseded, "agent.yaml names another spec for "+r.Container+" now")

			return
		}
	}
}
