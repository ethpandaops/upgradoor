package scheduler

import (
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// prefixSpec starts the rollout key of a spec and the group of the spec
// rollouts of one container.
const prefixSpec = "spec:"

// specKey is the rollout key of a client's container moving to one spec:
// every host whose container of the client agent.yaml moves to the same
// image, digest and change of the command line shares it.
func specKey(client, container, key string) string {
	return specGroup(client, container) + "@" + key
}

// specGroup is the group of the spec rollouts of one container of a client;
// a container without a client has its name alone.
func specGroup(client, container string) string {
	if client == "" {
		return prefixSpec + container
	}

	return prefixSpec + container + "#" + client
}

// specRollout is the rollout a spec need opens.
func specRollout(n *protocol.SpecNeed) *fleet.Rollout {
	title := n.Container + " to " + n.Image + " " + shortDigest(n.ToDigest)
	if n.Flags {
		title += ", command line changed"
	}

	return &fleet.Rollout{
		Kind:      fleet.RolloutSpec,
		Key:       specKey(n.Client, n.Container, n.Key),
		Title:     title,
		Container: n.Container,
		Image:     n.Image,
		Digest:    n.ToDigest,
		Revision:  n.ToRevision,
		Client:    n.Client,
	}
}

// supersedeSpec closes a spec rollout whose client's container nobody moves
// to its spec any more while another spec of it is reported: agent.yaml moved
// on before the rollout was done.
func (p *pass) supersedeSpec(r *fleet.Rollout, rep *reportedNeeds) {
	if _, ok := rep.specs[r.Key]; ok {
		return
	}

	for _, s := range rep.specs {
		if s.Container == r.Container && s.Client == r.Client {
			p.closeRollout(r, fleet.RolloutSuperseded, "agent.yaml names another spec for "+r.Container+" now")

			return
		}
	}
}
