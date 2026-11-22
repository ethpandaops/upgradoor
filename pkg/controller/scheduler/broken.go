package scheduler

import (
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// A host is broken for a group (the rollouts of one unit, UnitName, or the
// packages rollouts) when its last lease for the
// group failed. The mark is kept with the host (fleet.Host.Broken): set where
// a lease fails, cleared where a lease is released with the group's steps
// done, gone with the host when it is forgotten. A host that is broken for
// the group of a rollout goes first in it, before the canary, and the other
// members wait for it.

// reasonEndedByOperator is the error of a member whose lease a person ended
// (end-lease).
const reasonEndedByOperator = "ended by operator"

// needGroup names the group of rollouts that serve needs like this one: the
// rollouts of its unit, or of the packages kind. The need of a manual
// directive and a reboot need belong to no group.
func needGroup(n *protocol.Need) string {
	switch {
	case n.DirectiveID != "", isLocal(n):
		return ""
	case n.Kind == protocol.NeedPackages && n.Packages != nil:
		return groupPackages
	default:
		return needGroupName(n)
	}
}

// rolloutGroup names the rollouts that are about the same thing: those of one
// unit, whatever the target, and those of the packages kind. Reboot and
// manual rollouts belong to no group.
func rolloutGroup(r *fleet.Rollout) string {
	switch r.Kind {
	case fleet.RolloutGroup:
		return rolloutGroupName(r)
	case fleet.RolloutPackages:
		return groupPackages
	default:
		return ""
	}
}

// leaseGroups returns the groups a lease carries a step of, in order.
func leaseGroups(l *fleet.Lease) []string {
	groups := make([]string, 0, len(l.Needs))

	for i := range l.Needs {
		if g := needGroup(&l.Needs[i]); g != "" && !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}

	slices.Sort(groups)

	return groups
}

// groupDone reports whether the agent reported done for every step the lease
// carries of the group.
func groupDone(l *fleet.Lease, group string) bool {
	if l.Operation == nil {
		return false
	}

	found := false

	for i := range l.Needs {
		n := &l.Needs[i]
		if needGroup(n) != group {
			continue
		}

		if stepStatus(l.Operation, n.ID) != protocol.StepDone {
			return false
		}

		found = true
	}

	return found
}

// stepFailed reports whether the agent reported a step of the lease failed.
func stepFailed(l *fleet.Lease) bool {
	return l.Operation != nil && slices.ContainsFunc(l.Operation.Steps,
		func(s protocol.Step) bool { return s.Status == protocol.StepFailed })
}

// breakHost marks the host of a failed lease broken for every group the
// lease carried. A group whose steps were all done while another step failed
// is spared: the failure is provably not about it (failLease spares the
// member the same way).
func (p *pass) breakHost(l *fleet.Lease) {
	spared := stepFailed(l)

	for _, group := range leaseGroups(l) {
		if !spared || !groupDone(l, group) {
			p.setBroken(l.Host, group, l.ID)
		}
	}
}

// repairHost clears the mark of the host of a released lease, which a lease
// is only after the host passed readiness, for every group whose steps the
// lease carried were done.
func (p *pass) repairHost(l *fleet.Lease) {
	for _, group := range leaseGroups(l) {
		if groupDone(l, group) {
			p.setBroken(l.Host, group, "")
		}
	}
}

// setBroken sets the host's mark for the group to the lease, or clears it
// when lease is empty. The host's row is not changed: the pass works on a
// copy, which the outcome carries when it differs.
func (p *pass) setBroken(host, group, lease string) {
	hv := p.hosts[host]
	if hv == nil || hv.broken[group] == lease {
		return
	}

	next := make(map[string]string, len(hv.broken)+1)
	for g, id := range hv.broken {
		next[g] = id
	}

	if lease == "" {
		delete(next, group)
	} else {
		next[group] = lease
	}

	hv.broken = next
	p.marked[host] = true
}

// broken reports whether the host is broken for the group of the rollout.
func (p *pass) broken(r *fleet.Rollout, host string) bool {
	hv := p.hosts[host]

	return hv != nil && hv.broken[rolloutGroup(r)] != ""
}

// brokenAwaited returns the first member whose host is broken and that is
// still to be served, or nil.
func (p *pass) brokenAwaited(r *fleet.Rollout) *fleet.RolloutHost {
	if rolloutGroup(r) == "" {
		return nil
	}

	for i := range r.Hosts {
		if m := &r.Hosts[i]; p.awaited(r, m) && p.broken(r, m.Host) {
			return m
		}
	}

	return nil
}
