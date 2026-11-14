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
	reasonNotDisrupted  = "nothing was disrupted: every step was superseded or refused"
	reasonLostOperation = "the agent no longer reports the operation"
	reasonHostForgotten = "host forgotten"
	reasonOtherLease    = "the agent works on another lease"
	reasonAgentFailed   = "the agent reported a failure"
)

// advanceLeases moves every open lease along its lifecycle from the host's
// last sync.
func (p *pass) advanceLeases() {
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

// advanceGranted handles a lease the agent has not started yet. Once the
// expiry has passed it expires as soon as a sync from after the expiry shows
// no operation; an agent that stays silent for another leaseTtl is treated
// like a silent running one, because it may have started.
func (p *pass) advanceGranted(l *fleet.Lease, hv *hostView, op *protocol.Operation) {
	if op != nil {
		p.applyOperation(l, hv, op)

		return
	}

	if p.now.Before(l.ExpiresAt) {
		return
	}

	switch {
	case !hv.h.LastSeen.Before(l.ExpiresAt):
		p.expireLease(l, reasonNeverStarted)
	case !p.now.Before(l.ExpiresAt.Add(p.set.LeaseTTL.Std())):
		p.failLease(l, reasonAgentSilent)
	}
}

func (p *pass) advanceRunning(l *fleet.Lease, hv *hostView, op *protocol.Operation) {
	switch {
	case p.now.Sub(hv.h.LastSeen) > p.set.LeaseTTL.Std():
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

	if l.Force {
		p.releaseLease(l)

		return
	}

	ready, note := p.ready(l, hv)
	l.Readiness = note

	if ready {
		if id := p.stillReported(l, hv); id != "" {
			p.failLease(l, "need "+id+" is still reported after the operation")

			return
		}

		p.releaseLease(l)

		return
	}

	if timeout := p.set.ReadinessTimeout.Std(); p.now.Sub(*l.AgentDoneAt) > timeout {
		p.failLease(l, fmt.Sprintf("not ready after %s: %s", timeout, note))
	}
}

// stillReported returns the ID of an observed image or packages need whose
// step the agent reported done but that a sync from after the done report
// still lists: the operation did not take effect. Reboot and manual needs
// are not checked, their IDs do not change when the cause recurs.
func (p *pass) stillReported(l *fleet.Lease, hv *hostView) string {
	if l.Operation == nil || !hv.h.LastSeen.After(*l.AgentDoneAt) {
		return ""
	}

	for i := range l.Needs {
		n := &l.Needs[i]
		if n.DirectiveID != "" || n.Kind == protocol.NeedReboot {
			continue
		}

		if stepStatus(l.Operation, n.ID) != protocol.StepDone {
			continue
		}

		if slices.ContainsFunc(hv.needs(), func(r protocol.Need) bool { return r.ID == n.ID }) {
			return n.ID
		}
	}

	return ""
}

// ready evaluates the readiness rule for a verifying lease and returns a
// one-line progress note.
func (p *pass) ready(l *fleet.Lease, hv *hostView) (ready bool, note string) {
	if hv.weight > 0 && p.w.Health.LivenessKnown {
		return p.readyByLiveness(l, hv)
	}

	return p.readyByProbe(l, hv)
}

// readyByLiveness: for soakEpochs consecutive complete epochs that started
// after the agent's done time, enough of the host's validators attested.
func (p *pass) readyByLiveness(l *fleet.Lease, hv *hostView) (ready bool, note string) {
	soak := p.set.SoakEpochs
	if soak == 0 {
		return true, "no soak configured"
	}

	epoch := p.w.Health.EpochDuration()
	if epoch <= 0 || p.w.Health.GenesisTime.IsZero() {
		return false, "epoch timing unknown"
	}

	want := max(uint64(math.Floor(p.set.MinParticipationPercent*float64(hv.weight)/100+budgetEpsilon)), 1)
	run := uint64(0)

	if p.haveRows {
		for e := p.newestEpoch; ; e-- {
			start := p.w.Health.GenesisTime.Add(time.Duration(e) * epoch)
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

	note = fmt.Sprintf("%d of %d epochs with at least %d of %d validators attesting",
		min(run, soak), soak, want, hv.weight)

	return run >= soak, note
}

// readyByProbe: the agent's probes reported synced on every sync of an
// unbroken run that has lasted soakEpochs epoch durations, with at least one
// sync after the done report.
func (p *pass) readyByProbe(l *fleet.Lease, hv *hostView) (ready bool, note string) {
	seen := hv.h.LastSeen

	if hv.h.Status == nil || !seen.After(*l.AgentDoneAt) {
		l.SyncedSince = nil

		return false, "waiting for a sync after the operation"
	}

	if !hv.h.Status.Node.Synced() {
		l.SyncedSince = nil

		return false, "probes do not report synced"
	}

	if l.SyncedSince == nil {
		since := seen
		l.SyncedSince = &since
	}

	if hv.stale {
		return false, reasonAgentSilent
	}

	soak := p.set.SoakEpochs
	if soak == 0 {
		return true, "probes synced"
	}

	epoch := p.w.Health.EpochDuration()
	if epoch <= 0 {
		return false, "epoch timing unknown"
	}

	window := time.Duration(soak) * epoch
	if p.now.Sub(*l.SyncedSince) < window {
		return false, fmt.Sprintf("probes synced, soaking for %s", window)
	}

	return true, "probes synced"
}

// endLease closes a lease and frees the host's slot.
func (p *pass) endLease(l *fleet.Lease, state fleet.LeaseState) {
	ended := p.now
	l.State = state
	l.EndedAt = &ended

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

	p.record("lease.expired", l.Host, map[string]any{keyLease: l.ID, keyReason: reason})
}

// releaseLease ends a lease whose host is ready. Steps the agent reported
// done settle their rollout's member; anything else returns to pending.
func (p *pass) releaseLease(l *fleet.Lease) {
	p.endLease(l, fleet.LeaseReleased)

	for _, r := range p.leaseRollouts(l) {
		m := r.Host(l.Host)

		if stepsDone(l, r.ID) {
			p.setMember(m, fleet.HostDone)

			continue
		}

		m.LeaseID = ""
		p.setMember(m, fleet.HostPending)
	}

	p.record("lease.released", l.Host, map[string]any{keyLease: l.ID, keyForce: l.Force})
}

// failLease ends a lease as failed and halts every rollout it served,
// except rollouts whose own steps are all done while another step failed.
func (p *pass) failLease(l *fleet.Lease, reason string) {
	p.endLease(l, fleet.LeaseFailed)
	l.Error = reason

	otherFailed := l.Operation != nil && slices.ContainsFunc(l.Operation.Steps,
		func(s protocol.Step) bool { return s.Status == protocol.StepFailed })

	halted := make([]string, 0, len(l.Rollouts))

	for _, r := range p.leaseRollouts(l) {
		m := r.Host(l.Host)

		if otherFailed && stepsDone(l, r.ID) {
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

	p.record("lease.failed", l.Host, map[string]any{
		keyLease: l.ID, keyReason: reason, keyRollouts: halted,
	})
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

// adoptOperations creates lease rows for operations that hosts report for
// leases the controller does not know, so the hosts count as holding a
// lease.
func (p *pass) adoptOperations() {
	for _, name := range p.names {
		hv := p.hosts[name]
		op := hv.operation()

		if op == nil || op.LeaseID == "" || p.w.ClosedLeases[op.LeaseID] || p.knownLease(op.LeaseID) {
			continue
		}

		if op.Phase == protocol.PhaseDone && !op.Disrupted {
			continue
		}

		if hv.lease != nil {
			if hv.lease.State != fleet.LeaseGranted {
				continue
			}

			p.expireLease(hv.lease, reasonOtherLease)
		}

		p.adopt(hv, op)
	}
}

func (p *pass) knownLease(id string) bool {
	return slices.ContainsFunc(p.leases, func(l *fleet.Lease) bool { return l.ID == id })
}

// adopt creates the lease row for an unknown operation in the reported
// state and links it to the open rollouts whose needs its steps name.
func (p *pass) adopt(hv *hostView, op *protocol.Operation) {
	l := &fleet.Lease{
		ID:        op.LeaseID,
		Host:      hv.h.Name,
		State:     fleet.LeaseRunning,
		Rollouts:  make(map[string]string, len(op.Steps)),
		Weight:    hv.weight,
		Charged:   hv.live,
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

		if !r.Force && r.Canary == "" && hv.class() == fleet.StageBatches {
			r.Canary = l.Host
			m.Stage = fleet.StageCanary
		}

		return
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
