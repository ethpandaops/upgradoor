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
	// ended (end-lease), once the agent's report that its operation is over
	// is on record.
	reasonEndedByOperator = "ended by operator"
	// holdReasonEnded is the reason of the controller hold that ending a
	// lease puts on its host.
	holdReasonEnded = "lease ended by operator"
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
	// steps of the group were not done although it was released, they were
	// done and the lease failed on a step of something else, or a person
	// ended the lease and by the agent's report nothing of the group was
	// applied.
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
	return groupAll(l, group, protocol.StepDone)
}

// groupAll reports whether the agent reported the status for every step the
// lease carries of the group.
func groupAll(l *fleet.Lease, group string, status protocol.StepStatus) bool {
	if l.Operation == nil {
		return false
	}

	found := false

	for i := range l.Needs {
		n := &l.Needs[i]
		if needGroup(n) != group {
			continue
		}

		if stepStatus(l.Operation, n.ID) != status {
			return false
		}

		found = true
	}

	return found
}

// groupFailed reports whether the agent reported a step of the group failed.
func groupFailed(l *fleet.Lease, group string) bool {
	return groupAny(l, group, protocol.StepFailed)
}

// groupAny reports whether the agent reported the status for a step the
// lease carries of the group.
func groupAny(l *fleet.Lease, group string, status protocol.StepStatus) bool {
	if l.Operation == nil {
		return false
	}

	for i := range l.Needs {
		n := &l.Needs[i]
		if needGroup(n) == group && stepStatus(l.Operation, n.ID) == status {
			return true
		}
	}

	return false
}

// verdictOf judges an ended lease for one group. It is the table under
// "Broken hosts" in docs/scheduler.md: a host is repaired by a lease that was
// released with the group's steps done, and broken by a lease that failed,
// unless the failure is provably not about the group.
func verdictOf(l *fleet.Lease, group string) leaseVerdict {
	switch l.State {
	case fleet.LeaseReleased:
		if groupDone(l, group) {
			return verdictRepaired
		}

		return verdictNone
	case fleet.LeaseFailed:
		switch {
		case groupDone(l, group) && stepFailed(l):
			// The steps of the group were done and another step failed: the
			// member is spared (failLease).
			return verdictNone
		case groupFailed(l, group):
			return verdictBroke
		case l.Error == reasonEndedByOperator && !groupAny(l, group, protocol.StepDone):
			// A person ended the lease and by the agent's final report
			// nothing of this group was applied: its steps were cancelled,
			// refused, superseded or never begun. That shows nothing about
			// the host. With one step done something was applied and the
			// host was never shown to be back: when in doubt, below. Only
			// the report counts, not when the person clicked.
			return verdictNone
		case l.Cancelled != nil && groupAll(l, group, protocol.StepCancelled):
			// The lease failed for another reason after it was cancelled, but
			// no step of this group was ever begun. Only agents of an earlier
			// build report that: they said cancelled for the steps they never
			// began also when the operation failed. An agent of the current
			// build reports those steps as after any failure (pending), so a
			// cancel that is followed by a failure breaks the host for every
			// group of the lease, below.
			return verdictNone
		default:
			return verdictBroke
		}
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
// makes the order total. Neither the ID nor the grant says which lease is the
// last one: IDs are random, and a lease that was taken up again after it had
// expired ends after leases that were granted later.
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

// ledgerDepth is how many leases the ledger keeps per host and group. The
// most recent one is what counts. The one before it is kept for the case that
// the most recent one is taken up again by its agent (reopen): what it said
// at its first end is void then, and the one before it holds again. Only a
// host's most recent lease can be taken up again (the agent holds one
// operation, and it is the last one it started), and it comes back into the
// ledger when it ends again, so two are enough.
const ledgerDepth = 2

// ledger holds, per host and group, the most recent ended leases that settled
// the host for the group (verdictOf is not verdictNone), newest first. It is
// a digest of the lease table: built from it, and kept in step as leases end
// and are taken up again.
type ledger map[string]map[string][]*fleet.Lease

// newLedger builds the ledger of a set of leases; open ones are passed over.
func newLedger(leases []*fleet.Lease) ledger {
	ld := make(ledger, len(leases))

	for _, l := range leases {
		ld.note(l)
	}

	return ld
}

// note takes in a lease. One that is open or says nothing leaves what the
// leases before it said.
func (ld ledger) note(l *fleet.Lease) {
	if l == nil || !l.State.Terminal() {
		return
	}

	for _, group := range leaseGroups(l) {
		// A lease is in the ledger once, with its last end.
		ld.drop(l.Host, group, l.ID)

		if verdictOf(l, group) == verdictNone {
			continue
		}

		byGroup := ld[l.Host]
		if byGroup == nil {
			byGroup = make(map[string][]*fleet.Lease, 2)
			ld[l.Host] = byGroup
		}

		entries := append(slices.Clone(byGroup[group]), l)

		slices.SortStableFunc(entries, func(a, b *fleet.Lease) int {
			switch {
			case endedAfter(a, b):
				return -1
			case endedAfter(b, a):
				return 1
			default:
				return 0
			}
		})

		byGroup[group] = entries[:min(len(entries), ledgerDepth)]
	}
}

// drop removes the lease with the ID from what is on record for the host and
// the group.
func (ld ledger) drop(host, group, id string) {
	entries := ld[host][group]
	if len(entries) == 0 {
		return
	}

	ld[host][group] = slices.DeleteFunc(slices.Clone(entries), func(e *fleet.Lease) bool {
		return e.ID == id
	})
}

// retract takes back what the end of a lease said: the lease is open again
// (reopen).
func (ld ledger) retract(l *fleet.Lease) {
	for group := range ld[l.Host] {
		ld.drop(l.Host, group, l.ID)
	}
}

// brokenSince returns the lease that broke the host for the group and that
// no later lease of the group has repaired, or nil.
func (ld ledger) brokenSince(host, group string) *fleet.Lease {
	if group == "" {
		return nil
	}

	if entries := ld[host][group]; len(entries) > 0 && verdictOf(entries[0], group) == verdictBroke {
		return entries[0]
	}

	return nil
}

// brokenAny reports whether the host is broken for any group.
func (ld ledger) brokenAny(host string) bool {
	for group := range ld[host] {
		if ld.brokenSince(host, group) != nil {
			return true
		}
	}

	return false
}

// leases returns the leases of the ledger, each once, in a stable order.
func (ld ledger) leases() []*fleet.Lease {
	out := make([]*fleet.Lease, 0, len(ld))
	seen := make(map[string]bool, len(ld))

	for _, byGroup := range ld {
		for _, entries := range byGroup {
			for _, l := range entries {
				if !seen[l.ID] {
					seen[l.ID] = true

					out = append(out, l)
				}
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
