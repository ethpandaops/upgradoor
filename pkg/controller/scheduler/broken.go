package scheduler

import (
	"cmp"
	"slices"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Which hosts a change broke, and which leases are still in doubt, is read
// off the lease table and nothing else. No rollout hands anything to another:
// a rollout that is opened, reopened, retried or restarted on asks the same
// two questions of the same leases.
//
//   - A host is broken for a group (the rollouts of one image reference, or
//     the packages rollouts) when the last lease that settled it for the group
//     failed it. Such a host goes first in every rollout of the group until a
//     lease of the group succeeds on it.
//   - A lease is in flight for a rollout when it is open and carries a step of
//     the rollout's group that it was granted for a rollout which is closed by
//     now. Nobody knows yet how it ends, so the rollout grants nothing to a
//     host that is not broken and chooses no canary until it has ended.

const (
	// reasonEndedByOperator is the failure reason of a lease that a person
	// ended.
	reasonEndedByOperator = "ended by operator"
	// brokenPrefix starts the note on a waiting member whose host is broken
	// for the rollout's group. The note is for people and is written anew in
	// every pass; what makes the member go first is the lease it names.
	brokenPrefix = "broken since lease "
	// carriedPrefix started that note in the builds that carried the mark
	// from rollout to rollout. Such a note is replaced like the current one.
	carriedPrefix = "failed in rollout "
)

// leaseVerdict is what the end of a lease says about its host for one group.
type leaseVerdict int

const (
	// verdictNone: the lease says nothing. It stopped nothing (expired), its
	// steps of the group were not done although it was released, or they were
	// done and the lease failed on a step of something else.
	verdictNone leaseVerdict = iota
	// verdictRepaired: the lease was released, which it is only after the host
	// passed readiness, with every step of the group done.
	verdictRepaired
	// verdictBroke: the lease failed, and not provably on another step.
	verdictBroke
)

// needGroup names the group of rollouts that serve needs like this one: the
// rollouts of its image reference, or of the packages kind. It is the group
// of the rollout the need is served for (rolloutGroup). The need of a manual
// directive and a reboot need belong to no group.
func needGroup(n *protocol.Need) string {
	switch {
	case n.DirectiveID != "":
		return ""
	case n.Kind == protocol.NeedImage && n.Image != nil:
		return prefixImageKey + n.Image.Image
	case n.Kind == protocol.NeedPackages && n.Packages != nil:
		return groupPackages
	default:
		return ""
	}
}

// rolloutGroup names the rollouts that are about the same thing: those of one
// image reference, whatever the digest, and those of the packages kind.
// Reboot and manual rollouts belong to no group.
func rolloutGroup(r *fleet.Rollout) string {
	switch r.Kind {
	case fleet.RolloutImage:
		return prefixImageKey + r.Image
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

// verdictOf judges an ended lease for one group the way its end settles the
// host's member in a rollout of the group (releaseLease, failLease,
// expireLease): done through a released lease is a repair, failed is broken,
// and an end that leaves the member waiting or done without a lease says
// nothing.
func verdictOf(l *fleet.Lease, group string) leaseVerdict {
	switch l.State {
	case fleet.LeaseReleased:
		if groupDone(l, group) {
			return verdictRepaired
		}

		return verdictNone
	case fleet.LeaseFailed:
		if groupDone(l, group) && stepFailed(l) {
			// The steps of the group were done and another step failed: the
			// member is spared (failLease).
			return verdictNone
		}

		return verdictBroke
	default:
		return verdictNone
	}
}

// stepFailed reports whether the agent reported a step of the lease failed.
func stepFailed(l *fleet.Lease) bool {
	return l.Operation != nil && slices.ContainsFunc(l.Operation.Steps,
		func(s protocol.Step) bool { return s.Status == protocol.StepFailed })
}

// endedAfter orders ended leases of one host: by end, then grant, then ID. A
// host holds one lease at a time, so the ends alone order them; the rest only
// makes the order total.
func endedAfter(a, b *fleet.Lease) bool {
	var endA, endB int64

	if a.EndedAt != nil {
		endA = a.EndedAt.UnixMilli()
	}

	if b.EndedAt != nil {
		endB = b.EndedAt.UnixMilli()
	}

	return cmp.Or(
		cmp.Compare(endA, endB),
		cmp.Compare(a.GrantedAt.UnixMilli(), b.GrantedAt.UnixMilli()),
		cmp.Compare(a.ID, b.ID),
	) > 0
}

// ledger holds, per host and group, the most recent ended lease that settled
// the host for the group (verdictOf is not verdictNone). It is a digest of
// the lease table: built from it, and kept in step as leases end.
type ledger map[string]map[string]*fleet.Lease

// newLedger builds the ledger of a set of leases; open ones are passed over.
func newLedger(leases []*fleet.Lease) ledger {
	ld := make(ledger, len(leases))

	for _, l := range leases {
		ld.note(l)
	}

	return ld
}

// note takes in a lease. One that is open or says nothing changes nothing.
func (ld ledger) note(l *fleet.Lease) {
	if l == nil || !l.State.Terminal() {
		return
	}

	for _, group := range leaseGroups(l) {
		if verdictOf(l, group) == verdictNone {
			continue
		}

		byGroup := ld[l.Host]
		if byGroup == nil {
			byGroup = make(map[string]*fleet.Lease, 2)
			ld[l.Host] = byGroup
		}

		if cur := byGroup[group]; cur == nil || cur.ID == l.ID || endedAfter(l, cur) {
			byGroup[group] = l
		}
	}
}

// brokenSince returns the lease that broke the host for the group and that
// no later lease of the group has repaired, or nil.
func (ld ledger) brokenSince(host, group string) *fleet.Lease {
	if group == "" {
		return nil
	}

	if l := ld[host][group]; l != nil && verdictOf(l, group) == verdictBroke {
		return l
	}

	return nil
}

// leases returns the leases of the ledger, each once, in a stable order.
func (ld ledger) leases() []*fleet.Lease {
	out := make([]*fleet.Lease, 0, len(ld))
	seen := make(map[string]bool, len(ld))

	for _, byGroup := range ld {
		for _, l := range byGroup {
			if !seen[l.ID] {
				seen[l.ID] = true

				out = append(out, l)
			}
		}
	}

	slices.SortFunc(out, func(a, b *fleet.Lease) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.ID, b.ID))
	})

	return out
}

// brokenSince returns the lease that broke the member's host for the group of
// the rollout, or nil when the host is in order (or the rollout has no group).
func (p *pass) brokenSince(r *fleet.Rollout, host string) *fleet.Lease {
	return p.ledger.brokenSince(host, rolloutGroup(r))
}

// broken reports whether the host is broken for the group of the rollout.
func (p *pass) broken(r *fleet.Rollout, host string) bool {
	return p.brokenSince(r, host) != nil
}

// brokenNote is the note on a waiting member whose host is broken: the lease
// in which it last failed, and the rollout that lease served it for.
func brokenNote(l *fleet.Lease, group string) string {
	note := brokenPrefix + l.ID

	for i := range l.Needs {
		n := &l.Needs[i]
		if id := l.Rollouts[n.ID]; needGroup(n) == group && id != "" {
			return note + " (rollout " + id + ")"
		}
	}

	return note
}

// isBrokenNote reports whether a member's note is the one markBroken writes
// (or its predecessor of the builds that carried a mark).
func isBrokenNote(note string) bool {
	return strings.HasPrefix(note, brokenPrefix) || strings.HasPrefix(note, carriedPrefix)
}

// markBroken keeps the note of the members whose host is broken in line with
// the lease table: a waiting member of a broken host names the lease, and a
// note whose host was repaired meanwhile goes.
func (p *pass) markBroken(r *fleet.Rollout) {
	group := rolloutGroup(r)

	for i := range r.Hosts {
		m := &r.Hosts[i]
		l := p.ledger.brokenSince(m.Host, group)
		waits := m.State == fleet.HostPending || m.State == fleet.HostExpected

		switch {
		case l != nil && waits && (m.Error == "" || isBrokenNote(m.Error)):
			m.Error = brokenNote(l, group)
		case l == nil && isBrokenNote(m.Error):
			m.Error = ""
		}
	}
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

// inFlight returns the host of an open lease that is in flight for the
// rollout, or "": the lease carries a step of the rollout's group for a
// rollout that is closed (aborted, superseded, completed) or that the lease
// table does not name. That lease is in effect the canary in flight of this
// rollout: until it has ended, nobody who is not broken is granted. It makes
// no difference how the lease came to be open: granted before its rollout was
// closed, or taken up again after it had expired. The lease of a rollout of
// the group that is open next to this one is that rollout's business.
func (p *pass) inFlight(r *fleet.Rollout) string {
	group := rolloutGroup(r)
	if group == "" {
		return ""
	}

	host := ""

	for _, l := range p.leases {
		if !l.State.Terminal() && p.foreign(l, group, r.ID) && (host == "" || l.Host < host) {
			host = l.Host
		}
	}

	return host
}

// foreign reports whether the lease carries a step of the group that was not
// granted for the rollout with the ID nor for another open rollout.
func (p *pass) foreign(l *fleet.Lease, group, rolloutID string) bool {
	for i := range l.Needs {
		n := &l.Needs[i]
		if needGroup(n) != group {
			continue
		}

		id := l.Rollouts[n.ID]
		if id == rolloutID {
			continue
		}

		if o := p.rolloutByID[id]; o == nil || o.State.Terminal() {
			return true
		}
	}

	return false
}
