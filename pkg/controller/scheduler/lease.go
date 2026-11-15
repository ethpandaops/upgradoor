package scheduler

import (
	"fmt"
	"math"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Reasons a lease ends without the agent saying so.
const (
	reasonAgentSilent   = "agent silent"
	reasonNeverStarted  = "the agent did not start before the lease expired"
	reasonNotDisrupted  = "nothing was disrupted: every step was superseded or refused"
	reasonLostOperation = "the agent no longer reports the operation"
	reasonHostForgotten = "host forgotten"
	reasonOtherLease    = "the agent works on another lease"
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
	case p.now.Before(deadline):
	case !hv.h.LastSeen.Before(deadline):
		p.expireLease(l, reasonNeverStarted)
	case !p.now.Before(later(deadline, p.w.Since).Add(p.set.LeaseTTL.Std())):
		// Counted from the scheduler's start at the earliest: after a
		// controller outage the agent gets leaseTtl to say what it did.
		p.failLease(l, reasonAgentSilent)
	}
}

func (p *pass) advanceRunning(l *fleet.Lease, hv *hostView, op *protocol.Operation) {
	switch {
	case p.now.Sub(later(hv.h.LastSeen, p.w.Since)) > p.set.LeaseTTL.Std():
		// Silence counts from the scheduler's start at the earliest: while the
		// controller was down the agent could not report.
		p.failLease(l, reasonAgentSilent)
	case op != nil:
		p.applyOperation(l, hv, op)
	case hv.h.Status != nil:
		// The status is replaced on every sync, so one without the operation
		// is newer than the one that reported it.
		p.failLease(l, reasonLostOperation)
	}
}

// applyOperation takes over the agent's report for a granted or running
// lease.
func (p *pass) applyOperation(l *fleet.Lease, hv *hostView, op *protocol.Operation) {
	l.Operation = cloneOperation(op)

	switch op.Phase {
	case protocol.PhaseFailed:
		p.failLease(l, cmpOr(op.Error, reasonAgentFailed))
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
	if l.AgentDoneAt == nil {
		doneAt := p.now
		l.AgentDoneAt = &doneAt
	}

	if l.Cancelled != nil {
		// A person ended the lease, and the agent's report that its operation
		// is over is on record: it stopped when it was told, or finished all
		// the same (an older build, or the cancel came too late), or the lease
		// was being verified when it was ended. The lease ends here: nobody
		// waits for the readiness of a host that a person took out of the
		// rollout. What the end says about the host is read off the report
		// alone (verdictOf), never off the moment of the click.
		p.failLease(l, reasonEndedByOperator)

		return
	}

	if l.Force {
		p.releaseLease(l)

		return
	}

	l.PeerChecks = nil
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
	// was down nobody looked, and a host with validators cannot be ready
	// before the beacon service has liveness again.
	if timeout := p.set.ReadinessTimeout.Std(); p.now.Sub(later(*l.AgentDoneAt, p.w.Since)) > timeout {
		p.failLease(l, fmt.Sprintf("not ready after %s: %s", timeout, note))
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
	noteEpochTiming = "epoch timing unknown"
	noteNodeReady   = "node at head with peers"
	noteNoProbe     = "no node probe configured"
	noteAttesting   = "validators attesting"
	// noteAwaitLiveness is the readiness note of a host with validators
	// while the beacon node has not said yet whether they attest.
	noteAwaitLiveness = "waiting for attestation data from the beacon node"
	noteReadyPrefix   = "ready: "
)

// ready evaluates the readiness check for a verifying lease and returns a
// one-line note saying what is still missing. The node must be back at head
// with peers; a host with validators must also attest again. Only a beacon
// node that is known not to serve liveness takes the attestation check away;
// while liveness is merely not known yet, the host is not ready.
func (p *pass) ready(l *fleet.Lease, hv *hostView) (ready bool, note string) {
	// Always evaluated: it keeps the run of healthy syncs.
	nodeOK, nodeNote := p.nodeReady(l, hv)
	if !nodeOK {
		return false, nodeNote
	}

	switch {
	case hv.weight == 0 || p.w.Health.LivenessUnsupported:
		return true, noteReadyPrefix + nodeNote
	case !p.w.Health.LivenessKnown:
		return false, noteAwaitLiveness
	}

	if attesting, attestNote := p.validatorsReady(l, hv); !attesting {
		return false, attestNote
	}

	return true, noteReadyPrefix + nodeNote + ", " + noteAttesting
}

// minPeers is the number of peers each of EL and CL must have for a node to
// count as back, in this fleet.
func (p *pass) minPeers() int {
	nodes := 0

	for _, hv := range p.hosts {
		if st := hv.h.Status; st != nil && (st.Node.EL != nil || st.Node.CL != nil) {
			nodes++
		}
	}

	return p.set.EffectiveMinPeers(nodes)
}

// validatorsReady: for readyEpochs consecutive complete epochs that started
// after the agent's done time, enough of the host's validators attested.
func (p *pass) validatorsReady(l *fleet.Lease, hv *hostView) (ready bool, note string) {
	epochs := p.set.ReadyEpochs
	if epochs == 0 {
		return true, noteAttesting
	}

	want := attestationBar(p.set.MinParticipationPercent, hv.weight)
	run := uint64(0)

	if p.haveRows {
		for e := p.newestEpoch; ; e-- {
			// An epoch that cannot be placed in time proves nothing.
			start, placed := p.w.Health.EpochStart(e)
			if !placed {
				return false, noteEpochTiming
			}

			row, ok := p.rows[l.Host][e]

			if start.Before(*l.AgentDoneAt) || !ok || row.Live < want {
				break
			}

			run++

			if e == 0 {
				break
			}
		}
	}

	note = fmt.Sprintf("%s: %d of %d epochs with at least %d of %d",
		noteAttesting, min(run, epochs), epochs, want, hv.weight)

	return run >= epochs, note
}

// attestationBar is the smallest number of validators that is at least
// percent of weight, and at least 1.
func attestationBar(percent float64, weight uint64) uint64 {
	return max(uint64(math.Ceil(percent*float64(weight)/100-budgetEpsilon)), 1)
}

// nodeReady: the agent's probes reported the node at head with enough peers
// on every sync of an unbroken run that spans readyEpochs epochs: the newest
// sync of the run, which is the host's last one, came at least that long
// after the first. A run is unbroken when every sync in it was healthy, no
// two consecutive ones lie more than hostStaleAfter apart, and all of it
// lies after the scheduler's start: a bad sync, silence and a controller
// restart each end it, and the next healthy sync starts a new one. A host
// with no probe configured is ready after such a run of syncs. Of a run that
// began before genesis only the part from genesis on is measured.
func (p *pass) nodeReady(l *fleet.Lease, hv *hostView) (ready bool, note string) {
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
	if l.SyncedSince != nil && (l.SyncedUntil == nil || l.SyncedSince.Before(p.w.Since) ||
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

	if p.set.ReadyEpochs == 0 {
		return true, note
	}

	// A run that began before the chain did (before the first slot phase)
	// counts from there: no epoch begins earlier, so the readyEpochs epochs are
	// the chain's first ones.
	began := *l.SyncedSince
	if first, known := p.w.Health.TimingStart(); known {
		began = later(began, first)
	}

	window, placed := p.w.Health.EpochSpan(began, p.set.ReadyEpochs)
	if !placed {
		return false, noteEpochTiming
	}

	if seen.Sub(began) < window {
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
// is never among them: it held that lease when the pass began. (A host whose
// operation the pass adopts later held none then, and is one for this pass.)
// There are none when readyPeerShare is 0.
func (p *pass) references() []*hostView {
	if p.set.ReadyPeerShare <= 0 {
		return nil
	}

	floor := p.minPeers()
	refs := make([]*hostView, 0, len(p.names))

	for _, name := range p.names {
		hv := p.hosts[name]
		if hv.lease == nil && !hv.stale && hv.h.Status != nil && hv.h.Status.Node.Synced(floor) &&
			!p.ledger.brokenAny(name) {
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

		fleetPeers := make([]int, 0, len(p.refs))

		for _, ref := range p.refs {
			if n, ok := layer.peers(ref.h.Status.Node); ok {
				fleetPeers = append(fleetPeers, n)
			}
		}

		if len(fleetPeers) < minReferences {
			continue
		}

		slices.Sort(fleetPeers)

		median := fleetPeers[(len(fleetPeers)-1)/2]
		needs := int(math.Ceil(p.set.ReadyPeerShare*float64(median) - budgetEpsilon))

		l.PeerChecks = append(l.PeerChecks, fleet.PeerCheck{Layer: layer.name, Peers: peers, Median: median, Needs: needs})

		if peers < needs && below == "" {
			below = fmt.Sprintf("%s peers %d, the fleet's median is %d (needs %d)", layer.name, peers, median, needs)
		}
	}

	return below
}

// endRun ends the lease's run of healthy syncs.
func endRun(l *fleet.Lease) {
	l.SyncedSince = nil
	l.SyncedUntil = nil
}

// endLease closes a lease and frees the host's slot. What the end says about
// the host (broken, repaired, nothing) is on record from here on.
func (p *pass) endLease(l *fleet.Lease, state fleet.LeaseState) {
	ended := p.now
	l.State = state
	l.EndedAt = &ended
	p.ended[l.Host] = true
	p.ledger.note(l)

	if hv := p.hosts[l.Host]; hv != nil && hv.lease == l {
		hv.lease = nil
	}
}

// leaseRollouts returns the open rollouts a lease serves with the host's
// member entry, in a stable order. Rollouts that closed meanwhile are left
// out.
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
		if r == nil || r.State.Terminal() {
			continue
		}

		if m := r.Host(l.Host); m != nil && m.State == fleet.HostLeased && m.LeaseID == l.ID {
			out = append(out, r)
		}
	}

	return out
}

// expireLease ends a lease that cost no availability; its hosts wait again.
func (p *pass) expireLease(l *fleet.Lease, reason string) {
	p.endLease(l, fleet.LeaseExpired)
	l.Readiness = ""

	for _, r := range p.leaseRollouts(l) {
		m := r.Host(l.Host)
		m.LeaseID = ""
		p.setMember(m, fleet.HostPending)
	}

	p.settleClosed(l)
	p.record("lease.expired", l.Host, map[string]any{keyLease: l.ID, keyReason: reason})
}

// releaseLease ends a lease whose host is ready. Steps the agent reported
// done settle their rollout's member; anything else returns to pending.
func (p *pass) releaseLease(l *fleet.Lease) {
	p.endLease(l, fleet.LeaseReleased)

	for _, r := range p.leaseRollouts(l) {
		m := r.Host(l.Host)

		if stepsDone(l, r.ID) {
			if !p.succeeded(r) {
				p.first[r.ID] = l
			}

			p.setMember(m, fleet.HostDone)

			continue
		}

		m.LeaseID = ""
		p.setMember(m, fleet.HostPending)
	}

	p.settleClosed(l)
	p.record("lease.released", l.Host, map[string]any{
		keyLease: l.ID, keyForce: l.Force,
		// What the controller saw before it gave the slot back.
		"readiness": l.Readiness,
	})
}

// failLease ends a lease as failed and halts every rollout it served,
// except rollouts whose own steps are all done while another step failed.
func (p *pass) failLease(l *fleet.Lease, reason string) {
	// The reason first: what the end says about the host depends on it.
	l.Error = reason
	p.endLease(l, fleet.LeaseFailed)

	spared := stepFailed(l)
	halted := make([]string, 0, len(l.Rollouts))

	for _, r := range p.leaseRollouts(l) {
		m := r.Host(l.Host)

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
				"host": l.Host, keyLease: l.ID, keyReason: reason,
			})
		}
	}

	p.settleClosed(l)
	p.record("lease.failed", l.Host, map[string]any{
		keyLease: l.ID, keyReason: reason, keyRollouts: halted,
	})
}

// settleClosed gives the host's member in every closed rollout that the
// ended lease served the lease's outcome, for whoever looks at the closed
// rollout: no decision reads it. A rollout that is closed while one of its
// leases is open (superseded, aborted) leaves the member leased; the lease
// finishes all the same. Its end halts nothing there: the rollout is closed.
// What the end means for the rollouts that follow is read off the lease
// (ledger). Only the latest closed rollout of a key is at hand; an older one
// keeps its leased member.
func (p *pass) settleClosed(l *fleet.Lease) {
	ids := invert(l.Rollouts)

	for _, id := range sortedKeys(ids) {
		r := p.rolloutByID[id]
		if r != nil && !r.State.Terminal() {
			continue
		}

		if r == nil {
			r = p.closedCopy(id, l)
		}

		if r == nil {
			continue
		}

		m := r.Host(l.Host)
		if m == nil || m.State != fleet.HostLeased || m.LeaseID != l.ID {
			continue
		}

		done := stepsDone(l, id)

		switch {
		case l.State == fleet.LeaseReleased && done:
			p.setMember(m, fleet.HostDone)
		case l.State == fleet.LeaseFailed && !done:
			p.setMember(m, fleet.HostFailed)
			m.Error = l.Error
		case l.State == fleet.LeaseFailed:
			m.LeaseID = ""
			p.setMember(m, fleet.HostDone)
		default:
			m.LeaseID = ""
			p.setMember(m, fleet.HostPending)
		}
	}
}

// invert returns the values of a map as the keys of a set.
func invert(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for _, v := range m {
		out[v] = true
	}

	return out
}

// closedCopy returns a copy to change of the closed rollout with the ID, if
// it is the latest closed one of its key and the lease's end changes
// anything in it, and queues it for writing.
func (p *pass) closedCopy(id string, l *fleet.Lease) *fleet.Rollout {
	for _, r := range p.closedDirty {
		if r.ID == id {
			return r
		}
	}

	for _, key := range sortedKeys(p.closedLatest) {
		r := p.closedLatest[key]
		if r.ID != id {
			continue
		}

		m := r.Host(l.Host)
		if m == nil || m.State != fleet.HostLeased || m.LeaseID != l.ID {
			return nil
		}

		c := cloneRollout(r)
		p.closedLatest[key] = c
		p.closedDirty = append(p.closedDirty, c)

		return c
	}

	return nil
}

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

// adoptOperations makes every host that works on an operation hold a lease
// for it: an operation for a lease the controller does not know is adopted,
// and one for a lease that expired as not started, or failed without the
// agent's word, is reopened. A lease that failed that way and that its agent
// says it never started is expired (neverStarted).
func (p *pass) adoptOperations() {
	for _, name := range p.names {
		hv := p.hosts[name]
		op := hv.operation()

		if op == nil || op.LeaseID == "" || p.knownLease(op.LeaseID) {
			continue
		}

		closed := p.w.ClosedLeases[op.LeaseID]

		// An operation that ended without stopping anything needs no lease.
		// But a lease that failed without the agent's word did not fail by
		// what the agent now says: it never started it.
		if op.Phase == protocol.PhaseDone && !op.Disrupted {
			if closed != nil && closed.State == fleet.LeaseFailed && resumes(closed, hv.h) {
				p.neverStarted(hv, closed, op)
			}

			continue
		}

		if closed != nil && !resumes(closed, hv.h) {
			continue
		}

		if hv.lease != nil {
			// The agent holds one operation. A lease of the host that it has
			// started (running: advanceRunning has just failed it for want of
			// its operation; verifying: its operation is behind it) cannot be
			// open next to this one, so only a granted one is found here. An
			// agent that is at work cannot start it: it is expired in favour of
			// the lease the agent works on. An agent whose operation is over can
			// start it at any moment, so it stays, and the report is passed
			// over: the host is back up, and the lease it reports keeps its end.
			if hv.lease.State != fleet.LeaseGranted || op.Phase.Terminal() {
				continue
			}

			p.expireLease(hv.lease, reasonOtherLease)
		}

		if closed != nil {
			p.reopen(hv, closed, op)

			continue
		}

		p.adopt(hv, op)
	}
}

// resumes reports whether an operation that the host reports for a lease
// which has ended means that the lease did not end when the controller said
// so: the host works under it, or has worked under it, without the controller
// having taken that into account.
//
//   - An expired lease stopped nothing by the controller's account. An
//     operation for it that stopped something (the caller has passed over the
//     ones that did not) says otherwise.
//   - A failed lease that ended without the agent's final word (the agent was
//     silent, or no longer reported the operation: the report on record is
//     missing or not terminal) is taken up again when the agent reports the
//     operation after all, in a sync from after the lease ended (the last
//     sync from before it is what the lease failed on). Otherwise the host
//     would be down, or back up
//     again, under a lease that the controller has closed: it would not count
//     against the budget while it is down, nobody would look whether it comes
//     back, and it would stay broken although the operation succeeded.
//
// What stays ignored: the report for a lease that ended with the agent's
// final word on record (released, failed on the agent's failure or on
// readiness, expired as undisrupted). The agent repeats that report until it
// sees a response without the lease; it says nothing new. The report for a
// lease that belongs to another host: there is no lease to give it. And a
// report of an operation that is over while the host holds a lease it has
// been granted since (adoptOperations): the host is up again and may start
// that lease at any moment.
func resumes(closed *fleet.Lease, host *fleet.Host) bool {
	if closed.Host != host.Name {
		return false
	}

	switch closed.State {
	case fleet.LeaseExpired:
		return true
	case fleet.LeaseFailed:
		final := closed.Operation != nil && closed.Operation.Phase.Terminal()

		return !final && closed.EndedAt != nil && host.LastSeen.After(*closed.EndedAt)
	default:
		return false
	}
}

func (p *pass) knownLease(id string) bool {
	return slices.ContainsFunc(p.leases, func(l *fleet.Lease) bool { return l.ID == id })
}

// reopen takes back the end of a lease whose agent works on it after all
// (resumes): it expired, and the agent's sync from before the start arrived
// after the grace period or the agent ran past its deadline; or it failed for
// the agent's silence, and the agent is back with the operation. What the
// first end said about the host is void (ledger.retract). A rollout that
// halted for it stays halted until a person retries it. The host is down, so
// it holds the lease
// again; where it still waits in the open rollouts the lease served, or is
// still failed by this lease, it is leased again. Where a rollout it served was closed meanwhile nothing is to
// be done: the lease is open, so it is in flight for the rollouts that follow
// (inFlight), and how it ends is on record like the end of any lease.
func (p *pass) reopen(hv *hostView, closed *fleet.Lease, op *protocol.Operation) {
	l := cloneLease(closed)
	l.State = fleet.LeaseRunning
	l.EndedAt = nil
	l.Error = ""

	for _, needID := range sortedKeys(l.Rollouts) {
		r := p.rolloutByID[l.Rollouts[needID]]
		if r == nil || r.State.Terminal() {
			continue
		}

		// The member waits again (the lease had expired, or a person retried
		// it), or it is still failed by this very lease.
		m := r.Host(l.Host)
		if m == nil || (m.State != fleet.HostPending && (m.State != fleet.HostFailed || m.LeaseID != l.ID)) {
			continue
		}

		m.LeaseID = l.ID
		m.Error = ""
		p.setMember(m, fleet.HostLeased)
	}

	p.leases = append(p.leases, l)
	p.origLeases[l.ID] = closed
	hv.lease = l

	p.ledger.retract(closed)

	p.record("lease.reopened", l.Host, map[string]any{
		keyLease: l.ID, "phase": string(op.Phase), "was": string(closed.State),
	})

	// The host worked under the lease, or still does: the hold that belongs
	// to its cancel is in place again before anything else is decided.
	if l.Cancelled != nil && (op.Disrupted || !op.Phase.Terminal()) {
		p.holdEnded(hv, l)
	}

	p.applyOperation(l, hv, op)
}

// holdEnded puts the host of a lease that a person ended back on controller
// hold, if it is not held for that any more. A cancelled lease whose host is
// not held exists only by being taken up again: the lease had ended without
// its agent's word (expired as not started, failed as silent), a person
// lifted the hold, which is allowed once the lease has ended, and then the
// agent reported the operation after all. The hold is part of the cancel
// (action.go, EndLease): without it the host would be granted the same work
// the moment the lease ends again. It is written in the transaction of the
// reopened lease, and audited. A person lifts it when the lease has ended.
func (p *pass) holdEnded(hv *hostView, l *fleet.Lease) {
	reason := endedHoldReason(hv.h)
	if hv.h.HoldWanted && reason == hv.h.HoldReason {
		return
	}

	p.holds = append(p.holds, store.HostHold{Host: l.Host, Wanted: true, Reason: reason})
	hv.held = true

	p.record("host.held", l.Host, map[string]any{
		keyLease:  l.ID,
		keyReason: "a lease that a person ended was taken up again",
		keyBefore: map[string]any{keyHoldWanted: hv.h.HoldWanted, keyHoldReason: hv.h.HoldReason},
		keyAfter:  map[string]any{keyHoldWanted: true, keyHoldReason: reason},
	})
}

// neverStarted gives a lease that failed without the agent's final word
// (resumes: the agent was silent, or no longer reported the operation) the
// end it had when its agent, back again, reports that it stopped nothing
// under it: it declined the lease, or every step was refused or superseded.
// Such a lease cost nothing, so it is expired, like a lease whose agent says
// so in time. What the failure said about the host is void: the host is not
// broken by it, and its member is not failed because of it (it waits again).
// A rollout that halted for it stays halted until a person retries it, as for
// every lease that is taken up again.
//
// It is the reopen path, with the operation's end applied in the same pass
// (applyOperation expires the lease). The row is expired with the agent's
// report on record from then on, which nothing takes up again, so a restart
// finds the same. The lease the host holds meanwhile, if any, stays its
// lease: the reported operation is over and holds no slot.
func (p *pass) neverStarted(hv *hostView, closed *fleet.Lease, op *protocol.Operation) {
	held := hv.lease

	p.reopen(hv, closed, op)

	if held != nil {
		hv.lease = held
	}
}

// adopt creates the lease row for an unknown operation in the reported
// state and links it to the open rollouts whose needs its steps name.
func (p *pass) adopt(hv *hostView, op *protocol.Operation) {
	l := &fleet.Lease{
		ID:       op.LeaseID,
		Host:     hv.h.Name,
		State:    fleet.LeaseRunning,
		Rollouts: make(map[string]string, len(op.Steps)),
		Weight:   hv.weight,
		Charged:  hv.live,
		// Nobody knows how an adopted lease was granted. While the gate is
		// closed, the lease of a host with validators counts as a repair for
		// the one-at-a-time rule: no repair is granted next to it.
		Repair:    hv.weight > 0 && !p.gate().OK,
		GrantedBy: grantedByAdoption,
		GrantedAt: p.now,
		ExpiresAt: p.now.Add(p.set.LeaseTTL.Std()),
	}

	for i := range op.Steps {
		p.adoptStep(l, hv, op.Steps[i].NeedID)
	}

	p.leases = append(p.leases, l)
	hv.lease = l

	p.record("lease.adopted", l.Host, map[string]any{keyLease: l.ID, "phase": string(op.Phase)})
	p.applyOperation(l, hv, op)
}

// adoptStep links one step of an adopted operation to the open rollout in
// which the host is a waiting member for that need.
func (p *pass) adoptStep(l *fleet.Lease, hv *hostView, needID string) {
	for _, n := range hv.needs() {
		if n.ID == needID {
			l.Needs = append(l.Needs, n)

			break
		}
	}

	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		m := r.Host(l.Host)
		if m == nil || m.NeedID != needID || m.State != fleet.HostPending {
			continue
		}

		l.Rollouts[needID] = r.ID
		m.LeaseID = l.ID
		p.setMember(m, fleet.HostLeased)

		return
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
