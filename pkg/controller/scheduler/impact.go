package scheduler

import (
	"cmp"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// needImpact is the impact class of one need on a host: the class of the
// action it runs, or the class the host reports for the container the need
// is about. A need about no container
// (packages, a reboot) takes the whole node down, and so does one about a
// container the host does not report.
func needImpact(h *fleet.Host, n *protocol.Need) protocol.Impact {
	// An action is as heavy as its agent.yaml says.
	if n.Action != nil {
		if n.Action.Impact.Valid() {
			return n.Action.Impact
		}

		return protocol.ImpactNode
	}

	name := n.Container()
	if name == "" || h == nil || h.Status == nil {
		return protocol.ImpactNode
	}

	for i := range h.Status.Containers {
		if c := &h.Status.Containers[i]; c.Name == name {
			return cmp.Or(c.Impact, protocol.ImpactNode)
		}
	}

	return protocol.ImpactNode
}

// needsImpact is the class of a set of needs served together: the heaviest.
func needsImpact(h *fleet.Host, needs []protocol.Need) protocol.Impact {
	impact := protocol.ImpactIndependent

	for i := range needs {
		impact = impact.Heavier(needImpact(h, &needs[i]))
	}

	return impact
}

// leaseImpact is the class of a lease; a lease from before the classes, or
// without one, counts as node.
func leaseImpact(l *fleet.Lease) protocol.Impact {
	return cmp.Or(l.Impact, protocol.ImpactNode)
}

// occupation is the class of what occupies the host: its lease, or node for
// an operation without a lease and for a charged host; empty when nothing
// does.
func (hv *hostView) occupation() protocol.Impact {
	switch {
	case hv.lease != nil:
		return leaseImpact(hv.lease)
	case hv.working(), hv.charged():
		return protocol.ImpactNode
	default:
		return ""
	}
}
