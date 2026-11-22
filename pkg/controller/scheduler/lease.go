package scheduler

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Reasons a lease ends without the agent saying so.
const (
	reasonAgentSilent   = "agent silent"
	reasonNeverStarted  = "the agent did not start before the lease expired"
	reasonNotDisrupted  = "nothing was disrupted: every step was superseded, refused or deferred"
	reasonLostOperation = "the agent no longer reports the operation"
	reasonHostForgotten = "host forgotten"
	reasonAgentFailed   = "the agent reported a failure"
	// noteAwaitSync is the readiness note until the agent has synced once
	// after its done report.
	noteAwaitSync = "waiting for a sync after the operation"
	// noteAwaitSyncStart is the readiness note after a controller start
	// until the agent has synced with it.
	noteAwaitSyncStart = "waiting for a sync after the controller's start"
)

// advanceLeases moves every open lease along its lifecycle from the host's
// last sync.
func (p *pass) advanceLeases() {
	p.refs = p.references()

	for _, l := range p.leases {
		hv := p.hosts[l.Host]
		if hv == nil {
			p.failLease(l, reasonHostForgotten)

			continue
		}

		op := hv.operation()
		if op != nil && op.LeaseID != l.ID {
			op = nil
		}

		switch l.State {
		case fleet.LeaseGranted:
			p.advanceGranted(l, hv, op)
		case fleet.LeaseRunning:
			p.advanceRunning(l, hv, op)
		case fleet.LeaseVerifying:
			p.verify(l, hv)
		default:
		}
	}
}

// advanceGranted handles a lease the agent has not started yet. It expires
// once a sync received leaseStartGrace or more after the expiry shows no
// operation: an earlier one may have been built before the agent started. An
// agent that stays silent for another leaseTtl is treated like a silent
// running one, because it may have started.
func (p *pass) advanceGranted(l *fleet.Lease, hv *hostView, op *protocol.Operation) {
	if op != nil {
		p.applyOperation(l, hv, op)

		return
	}

	deadline := l.ExpiresAt.Add(leaseStartGrace)

	switch {
	case l.Cancelled != nil && hv.h.LastSeen.After(l.Cancelled.At):
		// A person ended the lease, and the agent has synced since without an
		// operation for it. The lease is no longer offered (Assignment), so
		// the agent does not start it: there is nothing to wait for. An agent
		// that started it in that very moment works without a lease from its
		// next sync on (hostView.working), which the budget counts.
		p.cancelLease(l)
	case p.now.Before(deadline):
	case !hv.h.LastSeen.Before(deadline):
		p.expireLease(l, reasonNeverStarted)
	case !p.now.Before(later(deadline, p.w.Since).Add(p.set.LeaseTTL.Std())):
		// Counted from the scheduler's start at the earliest: after a
		// controller outage the agent gets leaseTtl to say what it did.
		p.failLease(l, reasonAgentSilent)
		p.chargeHost(l)
	}
}

func (p *pass) advanceRunning(l *fleet.Lease, hv *hostView, op *protocol.Operation) {
	switch {
	case p.now.Sub(later(hv.h.LastSeen, p.w.Since)) > p.set.LeaseTTL.Std():
		// Silence counts from the scheduler's start at the earliest: while the
		// controller was down the agent could not report.
		p.failLease(l, reasonAgentSilent)
		p.chargeHost(l)
	case op != nil:
		p.applyOperation(l, hv, op)
	case hv.h.Status != nil && l.Cancelled != nil:
		// The agent dropped the operation of a lease a person ended.
		p.cancelLease(l)
		p.chargeHost(l)
	case hv.h.Status != nil:
		// The status is replaced on every sync, so one without the operation
		// is newer than the one that reported it.
		p.failLease(l, reasonLostOperation)
		p.chargeHost(l)
	}
}

// applyOperation takes over the agent's report for a granted or running
// lease.
func (p *pass) applyOperation(l *fleet.Lease, hv *hostView, op *protocol.Operation) {
	l.Operation = cloneOperation(op)

	switch op.Phase {
	case protocol.PhaseFailed:
		// An agent that stopped because a person ended the lease cancelled
		// it; a step that failed is a failure all the same.
		if l.Cancelled != nil && !stepFailed(l) {
			p.cancelLease(l)
		} else {
			p.failLease(l, cmpOr(op.Error, reasonAgentFailed))
		}

		if op.Disrupted {
			p.chargeHost(l)
		}
	case protocol.PhaseDone:
		doneAt := hv.h.LastSeen
		if doneAt.After(p.now) || doneAt.IsZero() {
			doneAt = p.now
		}

		l.AgentDoneAt = &doneAt

		if !op.Disrupted {
			p.expireLease(l, reasonNotDisrupted)

			return
		}

		l.State = fleet.LeaseVerifying
		p.verify(l, hv)
	default:
		l.State = fleet.LeaseRunning
	}
}

// verify releases a verifying lease when the host is ready and fails it when
// readinessTimeout passes first.
func (p *pass) verify(l *fleet.Lease, hv *hostView) {
	if l.Cancelled != nil {
		// A person ended the lease, and the agent's report that its operation
		// is over is on record: it stopped when it was told, or finished all
		// the same (the cancel came too late), or the lease was being verified
		// when it was ended. The lease ends here: nobody waits for the
		// readiness of a host that a person took out of the rollout.
		p.cancelLease(l)
		p.chargeHost(l)

		return
	}

	// A person approved the lease: released now, whatever the check says.
	if l.Force || l.Approved != nil {
		p.releaseLease(l)

		return
	}

	l.PeerChecks = nil
	l.ReadyAt = nil
	ready, note := p.ready(l, hv)

	// The needs of the sync that reported done may predate the operation:
	// only a later sync says whether it took effect.
	if ready && !hv.h.LastSeen.After(*l.AgentDoneAt) {
		ready, note = false, noteAwaitSync
	}

	// Last, and only of a host that is ready otherwise: its peers against the
	// fleet's, as both stand now. Falling short keeps the lease, not more: the
	// run of healthy syncs stays, so the lease is released in the first pass
	// in which the host does not fall short.
	if ready {
		if below := p.belowFleet(l, hv.h.Status.Node); below != "" {
			ready, note = false, below
		}
	}

	l.Readiness = note

	if ready {
		if id := p.stillReported(l, hv); id != "" {
			p.failLease(l, "need "+id+" is still reported after the operation")

			return
		}

		p.releaseLease(l)

		return
	}

	// Counted from the scheduler's start at the earliest: while the controller
	// was down nobody looked, and the attestations seen before are gone.
	if timeout := p.set.ReadinessTimeout.Std(); p.now.Sub(later(*l.AgentDoneAt, p.w.Since)) > timeout {
		p.failLease(l, fmt.Sprintf("not ready after %s: %s", timeout, note))
		p.chargeHost(l)
	}
}

// stillReported returns the ID of an observed need whose step the agent
// reported done but that a sync from after the done report still lists: the
// operation did not take effect (the container is still on the old digest,
// the package was not upgraded, the host booted the old kernel again).
// Serving it again would repeat the disruption without end, so it fails the
// lease instead. Manual needs are not checked: the agent drops them itself.
func (p *pass) stillReported(l *fleet.Lease, hv *hostView) string {
	if l.Operation == nil {
		return ""
	}

	for i := range l.Needs {
		n := &l.Needs[i]
		if n.DirectiveID != "" || stepStatus(l.Operation, n.ID) != protocol.StepDone {
			continue
		}

		if slices.ContainsFunc(hv.needs(), func(r protocol.Need) bool {
			return r.ID == n.ID && r.DirectiveID == ""
		}) {
			return n.ID
		}
	}

	return ""
}

// Readiness notes.
const (
	noteChainTiming = "chain timing unknown"
	noteNodeReady   = "node at head with peers"
	noteNoProbe     = "no node probe configured"
	noteAttesting   = "validators attesting"
	noteReadyPrefix = "ready: "
)

// ready evaluates the readiness check for a verifying lease and returns a
// one-line note saying what is still missing. The node must be back at head
// with peers, and the probe of every other kind the lease rolled healthy; a
// host with validators must also attest again.
func (p *pass) ready(l *fleet.Lease, hv *hostView) (ready bool, note string) {
	// An independent container has nothing of the node to wait for: the
	// agent's verify window, which it ran through, is its readiness.
	if impact := leaseImpact(l); impact != protocol.ImpactNode {
		return true, noteReadyPrefix + string(impact) + " work ran through the verify window"
	}

	// Validators that attest prove the node is back; the run of healthy syncs
	// only has to last readyStableFor where there are no attestations to go
	// by: the host has no active validators.
	noAttestations := hv.weight == 0

	// Always evaluated: it keeps the run of healthy syncs.
	nodeOK, nodeNote := p.nodeReady(l, hv, noAttestations)
	if !nodeOK {
		return false, nodeNote
	}

	if missing := kindsMissing(l, hv.h.Status); missing != "" {
		return false, missing
	}

	if noAttestations {
		return true, noteReadyPrefix + nodeNote
	}

	attesting, attestNote := p.validatorsReady(l, hv)
	if !attesting {
		return false, attestNote
	}

	return true, noteReadyPrefix + nodeNote + ", " + attestNote
}

// kindsMissing says which probe of a kind whose container the lease rolled
// is not healthy; empty when every one is, or has none. el and cl are part
// of the node's own check.
func kindsMissing(l *fleet.Lease, st *protocol.SyncRequest) string {
	for i := range l.Needs {
		name := l.Needs[i].Container()
		if name == "" {
			continue
		}

		for j := range st.Containers {
			if c := &st.Containers[j]; c.Name == name && c.Managed {
				if missing := st.Node.KindMissing(c.Kind); missing != "" {
					return missing
				}
			}
		}
	}

	return ""
}

// minPeers is the number of peers each of EL and CL must have for a node to
// count as back, in this fleet.
func (p *pass) minPeers() int {
	if p.peers > 0 {
		return p.peers
	}

	nodes := 0

	for _, hv := range p.hosts {
		if st := hv.h.Status; st != nil && (st.Node.EL != nil || st.Node.CL != nil) {
			nodes++
		}
	}

	p.peers = p.set.EffectiveMinPeers(nodes)

	return p.peers
}

// validatorsReady: the host's validators have attestations included on the
// canonical chain for readyAttestations distinct slots after the one the
// agent reported done in, or for one slot per validator where it has fewer.
func (p *pass) validatorsReady(l *fleet.Lease, hv *hostView) (ready bool, note string) {
	first, placed := p.firstSlotAfter(*l.AgentDoneAt)
	if !placed {
		return false, noteChainTiming
	}

	slots := p.w.Attested[l.Host]
	i, _ := slices.BinarySearch(slots, first)
	seen := uint64(len(slots) - i)
	since := l.AgentDoneAt.UTC().Format(time.TimeOnly)

	if seen == 0 {
		return false, "waiting for the first attestation since " + since
	}

	want := min(p.set.ReadyAttestations, hv.weight)

	return seen >= want, fmt.Sprintf("%s: %d of %d slots since %s", noteAttesting, min(seen, want), want, since)
}

// firstSlotAfter returns the first slot after the one containing at; before
// the chain's first known slot, that slot.
func (p *pass) firstSlotAfter(at time.Time) (slot uint64, placed bool) {
	n := &p.w.Health

	start, known := n.TimingStart()
	if !known {
		return 0, false
	}

	if at.Before(start) {
		return n.Phases[0].Epoch * n.SlotsPerEpoch, true
	}

	slot, placed = n.SlotAt(at)

	return slot + 1, placed
}

// nodeReady: the agent's probes report the node at head with enough peers in
// its newest sync. With span, that also held on every sync of an unbroken run
// that lasts readyStableFor: the newest sync of the run, which is the host's
// last one, came at least that long after the first. A run is unbroken when
// every sync in it was healthy, no two consecutive ones lie more than
// hostStaleAfter apart, and all of it lies after the scheduler's start: a bad
// sync, silence and a controller restart each end it, and the next healthy
// sync starts a new one. A host with no probe configured is ready after such
// a run of syncs.
func (p *pass) nodeReady(l *fleet.Lease, hv *hostView, span bool) (ready bool, note string) {
	seen := hv.h.LastSeen

	switch {
	case hv.h.Status == nil || !seen.After(*l.AgentDoneAt):
		endRun(l)

		return false, noteAwaitSync
	case seen.Before(p.w.Since):
		// What the host said to the controller before its restart is not a
		// sync of this run.
		endRun(l)

		return false, noteAwaitSyncStart
	}

	node := hv.h.Status.Node

	if missing := node.Missing(p.minPeers()); missing != "" {
		endRun(l)

		return false, missing
	}

	if hv.stale {
		endRun(l)

		return false, reasonAgentSilent
	}

	// A run from before the scheduler's start, or with a gap of more than
	// hostStaleAfter before this sync, is over: this sync starts a new one.
	if l.SyncedSince != nil && (l.SyncedSince.Before(p.w.Since) ||
		seen.Sub(*l.SyncedUntil) > p.set.HostStaleAfter.Std()) {
		endRun(l)
	}

	if l.SyncedSince == nil {
		since := seen
		l.SyncedSince = &since
	}

	until := seen
	l.SyncedUntil = &until

	note, wait := noteNodeReady, "%s, waiting for it to stay so for %s"
	if node.EL == nil && node.CL == nil {
		note, wait = noteNoProbe, "%s, waiting %s"
	}

	window := p.set.ReadyStableFor.Std()
	if !span || window <= 0 {
		return true, note
	}

	if seen.Sub(*l.SyncedSince) < window {
		readyAt := l.SyncedSince.Add(window)
		l.ReadyAt = &readyAt

		return false, fmt.Sprintf(wait, note, window)
	}

	return true, note
}

// minReferences is the fewest reference hosts a client's peer count is
// compared with. maxComparedPeers bounds the peer counts that are compared:
// what an agent reports beyond it (or below zero) is taken as the bound, so
// that no report can make the arithmetic overflow.
const (
	minReferences    = 3
	maxComparedPeers = 1_000_000
)

// comparedPeers is a reported peer count as it enters the comparison.
func comparedPeers(n int) int {
	return min(max(n, 0), maxComparedPeers)
}

// peerLayers are the clients whose peer counts are compared with the fleet,
// each with its own kind: EL with EL, CL with CL.
var peerLayers = []struct {
	name  string
	peers func(n protocol.NodeHealth) (peers int, probed bool)
}{
	{"EL", func(n protocol.NodeHealth) (int, bool) {
		if n.EL == nil {
			return 0, false
		}

		return comparedPeers(n.EL.Peers), true
	}},
	{"CL", func(n protocol.NodeHealth) (int, bool) {
		if n.CL == nil {
			return 0, false
		}

		return comparedPeers(n.CL.Peers), true
	}},
}

// references are the hosts that a host under a readiness check is compared
// with, as the pass finds them before it changes anything: hosts that hold no
// lease, are broken for nothing, are not stale and whose last sync shows the
// node at head with the required peers. The host of a lease that is compared
// is never among them: it held that lease when the pass began. There are none
// when readyPeerShare is 0.
func (p *pass) references() []*hostView {
	if p.set.ReadyPeerShare <= 0 {
		return nil
	}

	floor := p.minPeers()
	refs := make([]*hostView, 0, len(p.names))

	for _, name := range p.names {
		hv := p.hosts[name]
		if !hv.occupied() && !hv.stale && hv.h.Status != nil && hv.h.Status.Node.Synced(floor) &&
			len(hv.broken) == 0 {
			refs = append(refs, hv)
		}
	}

	return refs
}

// belowFleet compares the peer count of each client of a node with the
// median of the same client on the reference hosts (the lower of the two
// middle values of an even number of them) and names the first that has fewer
// than readyPeerShare of it; "" when none has. A client that the node does
// not probe, or that fewer than minReferences reference hosts probe, is not
// compared. What was compared is kept on the lease for people. It is asked
// once per pass, of a host that is ready otherwise (verify).
func (p *pass) belowFleet(l *fleet.Lease, node protocol.NodeHealth) string {
	below := ""

	for _, layer := range peerLayers {
		peers, probed := layer.peers(node)
		if !probed {
			continue
		}

		median, enough := p.fleetMedian(layer.name, layer.peers)
		if !enough {
			continue
		}

		needs := int(math.Ceil(p.set.ReadyPeerShare*float64(median) - budgetEpsilon))

		l.PeerChecks = append(l.PeerChecks, fleet.PeerCheck{Layer: layer.name, Peers: peers, Median: median, Needs: needs})

		if peers < needs && below == "" {
			below = fmt.Sprintf("%s peers %d, the fleet's median is %d (needs %d)", layer.name, peers, median, needs)
		}
	}

	return below
}

// fleetMedian is the median peer count of one client on the reference hosts
// (the lower of the two middle values of an even number of them); enough is
// false when fewer than minReferences of them probe the client. The
// reference hosts are those of the pass, so it is found once per client.
func (p *pass) fleetMedian(
	layer string, peers func(n protocol.NodeHealth) (int, bool),
) (median int, enough bool) {
	if m, ok := p.medians[layer]; ok {
		return m, m >= 0
	}

	fleetPeers := make([]int, 0, len(p.refs))

	for _, ref := range p.refs {
		if n, ok := peers(ref.h.Status.Node); ok {
			fleetPeers = append(fleetPeers, n)
		}
	}

	median = -1

	if len(fleetPeers) >= minReferences {
		slices.Sort(fleetPeers)

		median = fleetPeers[(len(fleetPeers)-1)/2]
	}

	p.medians[layer] = median

	return median, median >= 0
}

// endRun ends the lease's run of healthy syncs.
func endRun(l *fleet.Lease) {
	l.SyncedSince = nil
	l.SyncedUntil = nil
}

// endLease closes a lease and frees the host's slot.
func (p *pass) endLease(l *fleet.Lease, state fleet.LeaseState) {
	ended := p.now
	l.State = state
	l.EndedAt = &ended
	l.ReadyAt = nil

	if hv := p.hosts[l.Host]; hv != nil && hv.lease == l {
		hv.lease = nil
	}
}

// chargeHost charges the host of a node lease that failed after it may have
// disrupted the host: the host counts as down (hostView.charged) until
// proveHosts finds it back. A stale host is not charged.
func (p *pass) chargeHost(l *fleet.Lease) {
	hv := p.hosts[l.Host]
	if hv == nil || hv.stale || leaseImpact(l) != protocol.ImpactNode {
		return
	}

	ended := p.now
	hv.unproven = &ended
}

// proveHosts takes the charge off every host that is back since the lease
// that charged it ended, and off every host that is stale. A host with
// validators is back once a liveness epoch that began after the end has
// settled with a row for it: from then on what its validators do is counted.
// A host without validators is back with a sync after the end whose node
// passes the node part of the readiness check (one without probes on that
// sync alone).
func (p *pass) proveHosts() {
	for _, name := range p.names {
		hv := p.hosts[name]
		if hv.unproven != nil && (hv.stale || p.provenBack(hv)) {
			hv.unproven = nil
		}
	}
}

// provenBack reports whether the charged host is back (proveHosts).
func (p *pass) provenBack(hv *hostView) bool {
	ended := *hv.unproven

	if hv.weight == 0 {
		st := hv.h.Status

		return st != nil && hv.h.LastSeen.After(ended) && st.Node.Synced(p.minPeers())
	}

	for epoch := range p.rows[hv.h.Name] {
		if start, placed := p.w.Health.EpochStart(epoch); placed && start.After(ended) {
			return true
		}
	}

	return false
}

// leaseRollouts returns the open and the aborted rollouts a lease serves
// with the host's member entry, in a stable order. Rollouts that closed
// otherwise meanwhile are left out: such a rollout is not written again, and
// its member stays leased with the ID of the lease, which says how it ended.
func (p *pass) leaseRollouts(l *fleet.Lease) []*fleet.Rollout {
	ids := make([]string, 0, len(l.Rollouts))

	for _, id := range l.Rollouts {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}

	slices.Sort(ids)

	out := make([]*fleet.Rollout, 0, len(ids))

	for _, id := range ids {
		r := p.rolloutByID[id]
		if r == nil || (r.State.Terminal() && r.State != fleet.RolloutAborted) {
			continue
		}

		if m := p.member(r, l.Host); m != nil && m.State == fleet.HostLeased && m.LeaseID == l.ID {
			out = append(out, r)
		}
	}

	return out
}

// expireLease ends a lease that cost no availability; its hosts wait again.
// A lease that a person ended does not expire: it is cancelled, so that its
// host is not offered the same work the moment the lease is gone.
func (p *pass) expireLease(l *fleet.Lease, reason string) {
	if l.Cancelled != nil {
		p.cancelLease(l)

		return
	}

	p.endLease(l, fleet.LeaseExpired)
	l.Readiness = ""

	for _, r := range p.leaseRollouts(l) {
		m := p.member(r, l.Host)
		m.LeaseID = ""
		p.setMember(m, unserved(r))
	}

	p.record("lease.expired", l.Host, map[string]any{keyLease: l.ID, keyReason: reason})
}

// unserved is the state of a member whose lease ended without serving it:
// pending, or aborted in an aborted rollout.
func unserved(r *fleet.Rollout) fleet.HostState {
	if r.State == fleet.RolloutAborted {
		return fleet.HostAborted
	}

	return fleet.HostPending
}

// releaseLease ends a lease whose host is ready. Steps the agent reported
// done settle their rollout's member, as does a recheck; anything else
// returns to pending. The first success of a rollout that has no canary
// (its broken hosts went first) makes that host its canary.
func (p *pass) releaseLease(l *fleet.Lease) {
	p.endLease(l, fleet.LeaseReleased)
	p.repairHost(l)

	// Node work released through the full readiness check proves the host
	// back. A forced or approved lease proves nothing: the host's charge, if
	// it has one, stays.
	if hv := p.hosts[l.Host]; hv != nil && leaseImpact(l) == protocol.ImpactNode &&
		!l.Force && l.Approved == nil {
		hv.unproven = nil
	}

	for _, r := range p.leaseRollouts(l) {
		m := p.member(r, l.Host)

		if stepsDone(l, r.ID) || l.Recheck {
			if !p.succeeded(r) {
				p.first[r.ID] = l

				if r.Canary == "" && !r.Force && m.Stage == fleet.StageRepair {
					r.Canary = l.Host
				}
			}

			p.setMember(m, fleet.HostDone)

			continue
		}

		m.LeaseID = ""
		p.setMember(m, unserved(r))
	}

	detail := map[string]any{
		keyLease: l.ID, keyForce: l.Force,
		// What the controller saw before it gave the slot back.
		"readiness": l.Readiness,
	}

	if l.Approved != nil {
		detail["approvedBy"] = l.Approved.By
	}

	p.record("lease.released", l.Host, detail)
}

// failLease ends a lease as failed, marks its host broken for what the lease
// carried and halts every rollout it served, except rollouts whose own steps
// are all done while another step failed.
func (p *pass) failLease(l *fleet.Lease, reason string) {
	l.Error = reason
	p.endLease(l, fleet.LeaseFailed)
	p.breakHost(l)

	spared := stepFailed(l)
	halted := make([]string, 0, len(l.Rollouts))

	for _, r := range p.leaseRollouts(l) {
		m := p.member(r, l.Host)

		if spared && stepsDone(l, r.ID) {
			// Done, but not through a lease that passed readiness: it is no
			// success of the rollout.
			m.LeaseID = ""
			p.setMember(m, fleet.HostDone)

			continue
		}

		p.setMember(m, fleet.HostFailed)
		m.Error = reason

		halted = append(halted, r.ID)

		if r.State == fleet.RolloutActive {
			r.State = fleet.RolloutHalted
			r.Reason = l.Host + ": " + reason

			p.record("rollout.halted", r.ID, map[string]any{
				keyHost: l.Host, keyLease: l.ID, keyReason: reason,
			})
		}
	}

	p.record("lease.failed", l.Host, map[string]any{
		keyLease: l.ID, keyReason: reason, keyRollouts: halted,
	})
}

// cancelLease ends a lease a person ended once it is over. It is no failure:
// the host gets no broken mark, and each member waits as cancelled for a
// retry (which rechecks the readiness of a lease that did its steps, like
// that of a failed one). An active rollout pauses with the reason.
func (p *pass) cancelLease(l *fleet.Lease) {
	p.endLease(l, fleet.LeaseCancelled)

	by := l.Cancelled.By
	paused := make([]string, 0, len(l.Rollouts))

	for _, r := range p.leaseRollouts(l) {
		m := p.member(r, l.Host)
		p.setMember(m, fleet.HostCancelled)
		m.Error = reasonEndedByOperator

		if r.State == fleet.RolloutActive && !r.Paused {
			r.Paused = true
			r.PauseReason = cancelPauseReason(l.Host, by)
			paused = append(paused, r.ID)

			p.record("rollout.paused", r.ID, map[string]any{keyHost: l.Host, keyLease: l.ID, keyReason: r.PauseReason})
		}
	}

	p.record("lease.cancelled", l.Host, map[string]any{
		keyLease: l.ID, keyBy: by, keyRollouts: paused,
	})
}

// cancelPauseReason is Rollout.PauseReason of a rollout paused because a
// person ended the lease of a member.
func cancelPauseReason(host, by string) string {
	return host + " " + cancelPauseMark + " " + by
}

// cancelPauseMark is what a pause reason of cancelPauseReason says.
const cancelPauseMark = "cancelled by"

// stepsDone reports whether the agent reported done for every need the lease
// serves for the rollout.
func stepsDone(l *fleet.Lease, rolloutID string) bool {
	if l.Operation == nil {
		return false
	}

	found := false

	for needID, id := range l.Rollouts {
		if id != rolloutID {
			continue
		}

		if stepStatus(l.Operation, needID) != protocol.StepDone {
			return false
		}

		found = true
	}

	return found
}

func stepStatus(op *protocol.Operation, needID string) protocol.StepStatus {
	for i := range op.Steps {
		if op.Steps[i].NeedID == needID {
			return op.Steps[i].Status
		}
	}

	return ""
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
