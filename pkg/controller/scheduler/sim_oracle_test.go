package scheduler

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// simHostFact is what the simulated world knows about one registered host.
// None of it comes from the scheduler.
type simHostFact struct {
	weight uint64
	// live is how many validators attested in the newest liveness epoch the
	// beacon service stored (the full weight without a row). When the beacon
	// node does not serve liveness it is the probe rule instead: none when
	// the host is stale or its last sync's probes do not show the node at head
	// with a peer, else all.
	live uint64
	// holdWanted is the operator's hold switch.
	holdWanted bool
	// lastSync and status are the agent's last sync as it sent it.
	lastSync time.Time
	status   *protocol.SyncRequest
	// disrupted and doneAt are the agent's own record per lease ID: whether
	// it stopped anything, and when it finished successfully.
	disrupted map[string]bool
	doneAt    map[string]time.Time
	// late names the leases the agent started with a slow clock.
	late map[string]bool
	// reported is the agent's own record, per lease ID, of the last operation
	// it sent to the controller for the lease. What the end of a lease means
	// the oracle reads off this record, not off the row the scheduler keeps.
	reported map[string]*protocol.Operation
	// oldBuild: the agent does not know the cancel mark.
	oldBuild bool
}

func (h *simHostFact) held() bool {
	return h.holdWanted || (h.status != nil && h.status.Hold.Active())
}

// simFacts is the state of the simulated world at one observation.
type simFacts struct {
	now time.Time
	// since is when the controller last started. It cannot have heard from
	// an agent before that, so silence is counted from the later of the last
	// sync and since.
	since    time.Time
	settings fleet.Settings
	health   fleet.NetworkHealth
	hosts    map[string]*simHostFact
	// rows are the published liveness rows per epoch and host;
	// publishedEpoch is the newest epoch.
	rows           map[uint64]map[string]uint64
	publishedEpoch uint64
	havePublished  bool
	// retried names the rollouts a person retried since the last
	// observation.
	retried []string
	// skipped holds, per rollout key, the hosts a person skipped for it and
	// has not retried, and that have reported the need ever since.
	skipped map[string]map[string]bool
	// holds is the person's record of the hosts on controller hold: set by
	// hold, cleared by unhold, and kept when the controller loses its
	// database or forgets the host.
	holds map[string]bool
	// epochStart and epochOf are the simulated chain's own clock: when an
	// epoch begins and which epoch a time is in, across its slot-duration
	// forks.
	epochStart func(epoch uint64) time.Time
	epochOf    func(t time.Time) uint64
	// registered is when this database first saw each host: the time of the
	// sync that created its row. A lost database and a forgotten host start
	// over.
	registered map[string]time.Time
	// cancelled is the person's record of the leases they ended. The state
	// each was in at that moment is kept for the statistics; nothing is
	// judged by it.
	cancelled map[string]fleet.LeaseState
	// fresh is true when a reconcile has just been committed against
	// exactly these facts.
	fresh bool
}

// stale: no sync for hostStaleAfter, counted from the controller's start at
// the earliest.
func (f *simFacts) stale(h *simHostFact) bool {
	return f.now.Sub(later(h.lastSync, f.since)) > f.settings.HostStaleAfter.Std()
}

// simSnapshot is the committed scheduler state at one observation, in
// creation order.
type simSnapshot struct {
	rollouts   []*fleet.Rollout
	leases     []*fleet.Lease
	directives []*fleet.Directive
}

func (s *simSnapshot) lease(id string) *fleet.Lease {
	for _, l := range s.leases {
		if l.ID == id {
			return l
		}
	}

	return nil
}

func (s *simSnapshot) rollout(id string) *fleet.Rollout {
	for _, r := range s.rollouts {
		if r.ID == id {
			return r
		}
	}

	return nil
}

// oracleRun is the oracle's record of the readiness run of one lease: the
// first and the newest sync of the unbroken run of syncs, after the
// operation, that showed the node at head with peers.
type oracleRun struct {
	first, newest time.Time
	// syncs are the times of the syncs of the run, oldest first.
	syncs []time.Time
}

// oracle checks the invariants of docs/scheduler.md on a history of
// observations. It keeps its own books and takes none of its definitions
// from the scheduler: which leases and rollouts it has seen in which state,
// which rollouts a failed lease halted, what each lease took offline and
// which needs it served for which rollout and what those needs were about,
// how every lease ended and what that end says about its host, which rollouts
// have had a success, which leases passed a closed gate, and when each host's
// last lease ended.
type oracle struct {
	rollouts map[string]fleet.RolloutState
	leases   map[string]fleet.LeaseState
	// halted maps a rollout to the lease whose failure halted it.
	halted map[string]string
	// directives are the directives seen so far.
	directives map[string]bool
	// runs holds the readiness run per lease whose agent is done. The oracle
	// keeps it from the syncs it sees, not from the lease.
	runs map[string]oracleRun
	// charged is, per lease, how many of the host's validators attested when
	// the oracle first saw the lease: what the lease took offline.
	charged map[string]uint64
	// serves maps, per lease, each need to the rollout it is served for, as
	// the oracle first saw it.
	serves map[string]map[string]string
	// carries maps, per lease, each need to the thing it is about (an image
	// reference, the packages), as the oracle first saw the lease; needs that
	// are about nothing of the kind (a reboot, a manual action) are left out.
	carries map[string]map[string]string
	// words is the oracle's own record of who is broken: per host and thing,
	// every lease the oracle saw end that said something about it, and what
	// it said, in the order in which it saw them end. The last one counts. A
	// lease that is taken up again takes back what its end said; a host that
	// a person forgets has no record.
	words map[string]map[string][]oracleWord
	// ended is, per lease that has ended, when the oracle saw it end and
	// whether the agent's final report was on record then.
	ended map[string]oracleEnd
	// cancelSeen names the cancelled leases the oracle has looked at.
	cancelSeen map[string]bool
	// succeeded names the rollouts in which a lease that the oracle saw
	// released and judged ready itself had served all its needs.
	succeeded map[string]bool
	// reopened are the leases that were taken up again after they had
	// ended.
	reopened map[string]bool
	// members is the oracle's own book of the members of the open rollouts
	// as they stood at the observation before this one: per rollout and
	// host, the state and the lease. It is what a reconcile found.
	members map[string]map[string]oracleMember
	// again names, per lease that was taken up again, the rollouts it serves
	// since: those in which, by the oracle's book, the host's member waited
	// or was still failed by that lease when it was taken up, and which no
	// person had skipped the host for. The oracle decides it when it sees the
	// lease taken up, from what it had on record before.
	again map[string]map[string]bool
	// grants are the leases the oracle saw granted; repairs the leases that
	// passed a closed gate, or were adopted under one.
	grants  map[string]bool
	repairs map[string]bool
	// canary is each rollout's canary at the last observation; repairCanary
	// names the leases (with the rollout) whose host was the canary when the
	// oracle first saw the lease.
	canary       map[string]string
	repairCanary map[string]bool
	// leaseEnd is when the oracle saw the last lease of each host end;
	// repairEnded says that a repair lease ended in this observation.
	leaseEnd    map[string]time.Time
	repairEnded bool
	// verifying is when the oracle first saw each lease being checked for
	// readiness in a committed reconcile.
	verifying map[string]time.Time
	// stuck counts, per rollout and host, the committed reconciles in a row
	// that left the host waiting although nothing held it back.
	stuck map[string]int
	// counts says how often the oracle saw what the simulation wants to have
	// seen: a broken host granted first and one repaired; a host held back
	// by a lease in flight, and of those the leases of an aborted rollout, the
	// leases that were taken up again, the rollouts that a person's retry
	// opened and the rollouts that were open before the lease's rollout was
	// closed; and the leases a person ended.
	counts oracleCounts
}

// oracleCounts is what the oracle saw of the cases the simulation wants to
// have seen.
type oracleCounts struct {
	brokenFirst, repaired                            int
	flightHeld, flightAborted, flightReopened        int
	flightRetried, flightNextToOpen, endedByOperator int
	brokenByOperator, brokenAfterAbort               int
	// sparedByCancel: leases that ended as ended by operator and said nothing
	// about their host. reopenedFailed: failed leases taken up again.
	// forgottenBroken: hosts that were broken when a person forgot them.
	sparedByCancel, reopenedFailed, forgottenBroken int
	// sparedUntouched: leases that failed for another reason after a person
	// had ended them and said nothing for a thing none of whose steps ever
	// began (reported as cancelled, which only agents of an earlier build do
	// after a failure).
	sparedUntouched int
	// neverStarted: leases that had failed without the agent's word and were
	// expired when the agent said that it never started them.
	neverStarted int
}

// sameTime reports whether two optional times are the same moment.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Equal(*b)
}

// oracleEnd is how the oracle saw a lease end.
type oracleEnd struct {
	at time.Time
	// stamp is the end the lease itself carried.
	stamp *time.Time
	// final: the lease carried a terminal report of its agent.
	final bool
}

// oracleWord is what the end of a lease said about its host for one thing.
type oracleWord struct {
	// lease is the lease that ended; rollout the rollout it served the thing
	// for, as the oracle first saw it.
	lease, rollout string
	// broke: it failed the host. Otherwise it repaired it.
	broke bool
}

// oracleTimingUnknown is what a lease says whose epochs the scheduler could
// not place in time (docs/scheduler.md, Readiness).
const oracleTimingUnknown = "epoch timing unknown"

// simStuckPasses is how many committed reconciles in a row may leave a host
// waiting that nothing holds back. One can: the canary is chosen against the
// budget before the grants of the pass, and an older rollout may take that
// budget in the same pass.
const simStuckPasses = 5

// oracleView is what the oracle looks at while it checks the grants of one
// reconcile.
type oracleView struct {
	f    *simFacts
	snap *simSnapshot
	// before are the hosts that held a lease before the reconcile granted
	// anything; leased are those that hold one now. Each maps to what its
	// lease took offline.
	before map[string]uint64
	leased map[string]uint64
	// gate are the gate's failures at that reconcile.
	gate []string
}

func newOracle() *oracle {
	return &oracle{
		rollouts: make(map[string]fleet.RolloutState, 16),
		leases:   make(map[string]fleet.LeaseState, 64),
		halted:   make(map[string]string, 4),

		directives: make(map[string]bool, 64),
		runs:       make(map[string]oracleRun, 8),
		charged:    make(map[string]uint64, 64),
		serves:     make(map[string]map[string]string, 64),
		carries:    make(map[string]map[string]string, 64),
		words:      make(map[string]map[string][]oracleWord, 16),
		ended:      make(map[string]oracleEnd, 64),
		cancelSeen: make(map[string]bool, 2),
		succeeded:  make(map[string]bool, 8),
		reopened:   make(map[string]bool, 2),
		members:    make(map[string]map[string]oracleMember, 8),
		again:      make(map[string]map[string]bool, 2),
		grants:     make(map[string]bool, 64),
		repairs:    make(map[string]bool, 8),
		canary:     make(map[string]string, 8),
		leaseEnd:   make(map[string]time.Time, 16),
		verifying:  make(map[string]time.Time, 16),

		repairCanary: make(map[string]bool, 8),
		stuck:        make(map[string]int, 8),
	}
}

// observe checks one observation against the history and returns the first
// violated invariant.
func (o *oracle) observe(f *simFacts, snap *simSnapshot) error {
	if err := o.checkStates(f, snap); err != nil {
		return err
	}

	for _, id := range f.retried {
		delete(o.halted, id)
	}

	o.trackRuns(f, snap)
	o.repairEnded = false

	var (
		granted, adopted []*fleet.Lease
		leased           = make(map[string]uint64, len(snap.leases))
	)

	for _, l := range snap.leases {
		was, known := o.leases[l.ID]

		switch {
		case !known && l.GrantedBy == grantedByAdoption:
			adopted = append(adopted, l)
		case !known:
			granted = append(granted, l)
		case !l.State.Terminal():
			leased[l.Host] = o.charged[l.ID]
		}

		if !known {
			o.serves[l.ID] = maps.Clone(l.Rollouts)
			o.carries[l.ID] = oracleCarries(l)
		}

		again, err := o.takenUp(f, snap, l, was, known)
		if err != nil {
			return err
		}

		if err := o.checkCancel(f, l); err != nil {
			return err
		}

		if l.State != was || again {
			if err := o.checkLeaseEnd(f, snap, l); err != nil {
				return err
			}
		}
	}

	o.adopt(f, snap, adopted, leased)

	if err := o.checkBroken(f, snap); err != nil {
		return err
	}

	if err := o.checkReports(f, snap); err != nil {
		return err
	}

	if err := o.checkHalts(snap); err != nil {
		return err
	}

	if err := o.checkHolds(f, snap); err != nil {
		return err
	}

	view := &oracleView{f: f, snap: snap, before: maps.Clone(leased), leased: leased}

	// The gate is evaluated once per reconcile, before the first grant.
	view.gate = oracleGate(f, leased)

	for _, l := range granted {
		if host := f.hosts[l.Host]; host != nil {
			o.charged[l.ID] = host.live
		}

		if err := o.checkGrant(view, l); err != nil {
			return fmt.Errorf("grant of lease %s to %s: %w", l.ID, l.Host, err)
		}

		o.noteCanary(snap, l)
		leased[l.Host] = o.charged[l.ID]
	}

	for _, l := range snap.leases {
		o.leases[l.ID] = l.State

		if l.State.Terminal() {
			delete(o.runs, l.ID)
			delete(o.verifying, l.ID)
		}
	}

	if err := o.checkBooks(f, snap); err != nil {
		return err
	}

	for _, r := range snap.rollouts {
		o.rollouts[r.ID] = r.State
		o.canary[r.ID] = r.Canary
	}

	o.bookMembers(snap)

	if f.fresh {
		if err := o.checkProgress(view); err != nil {
			return err
		}

		return checkMembership(f, snap)
	}

	return nil
}

// takenUp looks at a lease that left a terminal state: it is open again, or
// (again is true) it was taken up and ended again between two looks, which
// shows as a lease that ended at another time. Such a lease is not the grant
// the oracle saw; its host may have become the canary in between. What its
// end said about the host is taken back.
func (o *oracle) takenUp(
	f *simFacts, snap *simSnapshot, l *fleet.Lease, was fleet.LeaseState, known bool,
) (again bool, err error) {
	if !known || !was.Terminal() {
		return false, nil
	}

	again = l.State.Terminal() && !sameTime(o.ended[l.ID].stamp, l.EndedAt)
	if again && !o.resumed(f, l, was) {
		return false, fmt.Errorf("lease %s, which had ended as %s, ended again as %s", l.ID, was, l.State)
	}

	if !again && l.State.Terminal() {
		return false, nil
	}

	delete(o.grants, l.ID)
	o.reopened[l.ID] = true
	o.noteCanary(snap, l)
	o.retract(l)

	if err := o.servesAgain(f, snap, l); err != nil {
		return false, err
	}

	if was == fleet.LeaseFailed {
		o.counts.reopenedFailed++
	}

	if was == fleet.LeaseFailed && again && l.State == fleet.LeaseExpired {
		o.counts.neverStarted++
	}

	return again, nil
}

// oracleMember is one member of a rollout in the oracle's book.
type oracleMember struct {
	state fleet.HostState
	lease string
}

// bookMembers writes down the members of the open rollouts as this
// observation found them.
func (o *oracle) bookMembers(snap *simSnapshot) {
	clear(o.members)

	for _, r := range snap.rollouts {
		if r.State.Terminal() {
			continue
		}

		book := make(map[string]oracleMember, len(r.Hosts))
		for i := range r.Hosts {
			book[r.Hosts[i].Host] = oracleMember{state: r.Hosts[i].State, lease: r.Hosts[i].LeaseID}
		}

		o.members[r.ID] = book
	}
}

// servesAgain decides, for a lease that has just been taken up again, which
// of the rollouts it was granted for it serves from now on, and holds the
// scheduler's rows against that. The lease serves a rollout again where the
// rollout was open and the host's member waited (pending, or leased by a
// lease the host had not started, which gives way), or was still failed by
// this very lease, at the observation before; a member that a
// person had skipped, that another lease had failed, or that was settled some
// other way is not its member any more. The oracle takes this from its own
// book and from the person's record of skips, not from the rows the reopen
// wrote: those rows must agree with it while the lease is open.
func (o *oracle) servesAgain(f *simFacts, snap *simSnapshot, l *fleet.Lease) error {
	again := make(map[string]bool, len(o.serves[l.ID]))

	for _, id := range o.serves[l.ID] {
		before, booked := o.members[id][l.Host]
		r := snap.rollout(id)

		// A lease the host had been granted since and had not started is
		// expired in favour of the one its agent works on: its member waits.
		waits := before.state == fleet.HostPending ||
			(before.state == fleet.HostLeased && before.lease != l.ID && o.leases[before.lease] == fleet.LeaseGranted)

		again[id] = booked && r != nil && !f.skipped[r.Key][l.Host] &&
			(waits || (before.state == fleet.HostFailed && before.lease == l.ID))
	}

	o.again[l.ID] = again

	if l.State.Terminal() {
		return nil
	}

	for id, serves := range again {
		r := snap.rollout(id)
		if r == nil || r.State.Terminal() {
			continue
		}

		m := r.Host(l.Host)
		leased := m != nil && m.State == fleet.HostLeased && m.LeaseID == l.ID

		if leased != serves {
			state := fleet.HostState("no member")
			if m != nil {
				state = m.State
			}

			return fmt.Errorf("lease %s of %s was taken up again; by the oracle's book its member in rollout %s was %q "+
				"(lease %q) before, so it serves the rollout again: %v, but the member is now %s",
				l.ID, l.Host, id, o.members[id][l.Host].state, o.members[id][l.Host].lease, serves, state)
		}
	}

	return nil
}

// adopt takes in the leases the scheduler adopted. An adopted lease took
// offline what attests now, as far as anybody knows. Under a closed gate it
// counts as a repair: the scheduler adopts host by host, each against the
// gate as the adoptions before it left it.
func (o *oracle) adopt(f *simFacts, snap *simSnapshot, adopted []*fleet.Lease, leased map[string]uint64) {
	slices.SortFunc(adopted, func(a, b *fleet.Lease) int { return cmp.Compare(a.Host, b.Host) })

	for _, l := range adopted {
		host := f.hosts[l.Host]
		if host == nil {
			continue
		}

		o.charged[l.ID] = host.live

		if host.weight > 0 && len(oracleGate(f, leased)) > 0 {
			o.repairs[l.ID] = true
		}

		o.noteCanary(snap, l)

		if !l.State.Terminal() {
			leased[l.Host] = host.live
		}
	}
}

// noteCanary remembers for a lease the oracle sees for the first time
// whether its host is, or just was, the canary of the rollouts it serves.
func (o *oracle) noteCanary(snap *simSnapshot, l *fleet.Lease) {
	for _, id := range o.serves[l.ID] {
		if r := snap.rollout(id); r != nil && (r.Canary == l.Host || o.canary[id] == l.Host) {
			o.repairCanary[l.ID+"/"+id] = true
		}
	}
}

// oracleStepsDone reports whether the agent reported done for every need
// that the lease serves for the rollout, by the oracle's own record of what
// the lease serves.
func (o *oracle) oracleStepsDone(f *simFacts, l *fleet.Lease, rollout string) bool {
	found := false
	report := oracleReport(f, l)

	for need, id := range o.serves[l.ID] {
		if id != rollout {
			continue
		}

		if report == nil || stepStatus(report, need) != protocol.StepDone {
			return false
		}

		found = true
	}

	return found
}

// checkBooks holds the scheduler's rows against the oracle's own records:
//
//   - a lease is marked as a repair exactly when the oracle saw it pass a
//     closed gate (or adopted under one), also after it was reopened;
//   - a member that waits carries no lease ID, and a member that is done
//     with a lease ID is done through a lease that was released with all its
//     steps for the rollout done;
//   - a canary that failed and halted the rollout stays the canary while the
//     rollout is halted;
//   - no canary is chosen while a lease is in flight for the rollout;
//   - a host that holds a repair lease is not the canary, unless it was the
//     canary before it got that lease;
//   - a lease that is being checked for readiness and is not ready is failed
//     once readinessTimeout has passed since the later of the oracle first
//     seeing it checked and the controller's start.
func (o *oracle) checkBooks(f *simFacts, snap *simSnapshot) error {
	open := make(map[string]*fleet.Lease, len(snap.leases))

	for _, l := range snap.leases {
		if _, seen := o.charged[l.ID]; seen && l.Repair != o.repairs[l.ID] {
			return fmt.Errorf("lease %s of %s is marked repair: %v, but the oracle saw: %v",
				l.ID, l.Host, l.Repair, o.repairs[l.ID])
		}

		if !l.State.Terminal() {
			open[l.ID] = l
		}

		// The simulated beacon service knows the chain's timing from its
		// start on: every epoch can be placed, also when a host came back
		// before genesis.
		if strings.Contains(l.Readiness, oracleTimingUnknown) || strings.Contains(l.Error, oracleTimingUnknown) {
			return fmt.Errorf("lease %s of %s is %s with %q %q, but the chain's timing is known",
				l.ID, l.Host, l.State, l.Readiness, l.Error)
		}

		if !f.fresh || l.State != fleet.LeaseVerifying || l.Force {
			continue
		}

		since, seen := o.verifying[l.ID]
		if !seen {
			since = f.now
			o.verifying[l.ID] = since
		}

		deadline := later(since, f.since).Add(f.settings.ReadinessTimeout.Std())
		if f.now.After(deadline) {
			return fmt.Errorf("lease %s of %s is still being checked for readiness %s after the later of its first "+
				"check and the controller's start, readinessTimeout is %s", l.ID, l.Host,
				f.now.Sub(later(since, f.since)), f.settings.ReadinessTimeout.Std())
		}
	}

	if !f.fresh {
		return nil
	}

	return o.checkMemberBooks(f, snap, open)
}

// checkMemberBooks is the part of checkBooks about members and canaries.
func (o *oracle) checkMemberBooks(f *simFacts, snap *simSnapshot, open map[string]*fleet.Lease) error {
	for _, r := range snap.rollouts {
		if r.State.Terminal() {
			continue
		}

		for i := range r.Hosts {
			m := &r.Hosts[i]

			switch m.State {
			case fleet.HostPending, fleet.HostExpected:
				if m.LeaseID != "" {
					return fmt.Errorf("member %s of rollout %s is %s and carries lease %s", m.Host, r.ID, m.State, m.LeaseID)
				}
			case fleet.HostDone:
				state, known := o.leases[m.LeaseID]
				if m.LeaseID == "" || !known {
					continue
				}

				l := snap.lease(m.LeaseID)
				if state != fleet.LeaseReleased || l == nil || !o.oracleStepsDone(f, l, r.ID) {
					return fmt.Errorf("member %s of rollout %s is done through lease %s, which is %s and did not "+
						"finish every step for the rollout", m.Host, r.ID, m.LeaseID, state)
				}
			default:
			}
		}

		was := o.canary[r.ID]
		if m := r.Host(was); was != "" && m != nil && r.State == fleet.RolloutHalted &&
			m.State == fleet.HostFailed && r.Canary != was {
			return fmt.Errorf("rollout %s is halted by its canary %s, which is no longer its canary (now %q)",
				r.ID, was, r.Canary)
		}

		// No canary is chosen while a lease is in flight for the rollout. One
		// that was chosen before stays.
		if l := o.inFlight(snap, r); l != nil && r.Canary != "" && r.Canary != was {
			return fmt.Errorf("rollout %s chose the canary %s while lease %s of a closed rollout is still open on %s",
				r.ID, r.Canary, l.ID, l.Host)
		}

		if m := r.Host(r.Canary); m != nil && m.State == fleet.HostLeased && open[m.LeaseID] != nil &&
			o.repairs[m.LeaseID] && !o.repairCanary[m.LeaseID+"/"+r.ID] {
			return fmt.Errorf("host %s became the canary of rollout %s while it holds the repair lease %s",
				m.Host, r.ID, m.LeaseID)
		}
	}

	return nil
}

// trackRuns keeps the oracle's own record of the readiness run of every open
// lease whose agent is done. The run starts with the first sync after the
// operation and after the controller's start that shows the node at head
// with peers, and grows with every further one. It ends with a sync that
// does not show that, when the agent goes stale, with a gap of more than
// hostStaleAfter between two of its syncs, and with a controller restart;
// the next healthy sync starts a new one. A sync the scheduler could not
// judge, because its reconcile was not committed, can start or extend a run
// but not end one: the scheduler cannot know of it either.
func (o *oracle) trackRuns(f *simFacts, snap *simSnapshot) {
	peers := oracleMinPeers(f)

	for _, l := range snap.leases {
		if was, known := o.leases[l.ID]; known && was.Terminal() {
			continue
		}

		host := f.hosts[l.Host]
		if host == nil {
			delete(o.runs, l.ID)

			continue
		}

		doneAt, done := host.doneAt[l.ID]
		back := done && host.status != nil && host.lastSync.After(doneAt) && !host.lastSync.Before(f.since) &&
			!f.stale(host) && oracleNodeBack(host.status.Node, peers)

		run, running := o.runs[l.ID]

		// A gap before this sync ends the run, and so does a controller
		// restart: what is left of a run is what came after the start.
		if running && back && host.lastSync.Sub(run.newest) > f.settings.HostStaleAfter.Std() {
			running = false
		}

		if running {
			run.syncs = slices.DeleteFunc(run.syncs, func(t time.Time) bool { return t.Before(f.since) })
			running = len(run.syncs) > 0
		}

		switch {
		case back && !running:
			o.runs[l.ID] = oracleRun{first: host.lastSync, newest: host.lastSync, syncs: []time.Time{host.lastSync}}
		case back:
			if !host.lastSync.Equal(run.newest) {
				run.syncs = append(run.syncs, host.lastSync)
			}

			run.first, run.newest = run.syncs[0], host.lastSync
			o.runs[l.ID] = run
		case f.fresh:
			delete(o.runs, l.ID)
		}
	}
}

// oracleGroup names the thing a rollout is about, as far as hosts can be
// broken for it: an image reference, or the packages. Reboot and manual
// rollouts are about nothing of the kind.
func oracleGroup(r *fleet.Rollout) string {
	switch r.Kind {
	case fleet.RolloutImage:
		return "image " + r.Image
	case fleet.RolloutPackages:
		return "packages"
	default:
		return ""
	}
}

// oracleCarries maps the needs of a lease to the things they are about. The
// need of a manual directive and a reboot need are about none.
func oracleCarries(l *fleet.Lease) map[string]string {
	out := make(map[string]string, len(l.Needs))

	for i := range l.Needs {
		n := &l.Needs[i]

		switch {
		case n.DirectiveID != "":
		case n.Kind == protocol.NeedImage && n.Image != nil:
			out[n.ID] = "image " + n.Image.Image
		case n.Kind == protocol.NeedPackages:
			out[n.ID] = "packages"
		default:
		}
	}

	return out
}

// oracleEndedOnReport reports whether a person ended the lease (by the
// person's own record) and it then failed with the agent's report on record
// that the operation is over. When the person clicked, before or after the
// agent finished, before or after a reconcile saw the report, makes no
// difference: only the steps of that report say what the end means.
//
// The report must have reached the controller: an agent that declined a
// lease says so once and forgets it when the response no longer carries the
// lease; if no reconcile read that sync, the controller never had the report
// (its row has none), and the lease ends as any lease whose agent says
// nothing.
func oracleEndedOnReport(f *simFacts, l *fleet.Lease) bool {
	_, cancelled := f.cancelled[l.ID]
	report := oracleReport(f, l)
	received := l.Operation != nil && l.Operation.Phase.Terminal()

	return cancelled && l.State == fleet.LeaseFailed && received &&
		report != nil && report.Phase == protocol.PhaseDone
}

// oracleReport is the agent's own record of the last operation it reported
// for the lease, or nil when it reported none. The row the scheduler keeps
// of that report (Lease.Operation) is held against it when the lease ends
// (checkLeaseEnd), and is otherwise not what the oracle judges by. Only of a
// host that this database does not know any more (a person forgot it) is
// the row all there is.
func oracleReport(f *simFacts, l *fleet.Lease) *protocol.Operation {
	if host := f.hosts[l.Host]; host != nil {
		return host.reported[l.ID]
	}

	return l.Operation
}

// oracleCancelSpares reports whether a failed lease that a person ended says
// nothing about its host for a thing no step of which failed: the lease ended
// on the agent's report and no step of the thing is done (anyDone is false:
// nothing of it was applied), or no step of the thing ever began (untouched),
// however the lease failed.
func oracleCancelSpares(f *simFacts, l *fleet.Lease, anyDone, untouched bool) bool {
	_, cancelled := f.cancelled[l.ID]

	return (oracleEndedOnReport(f, l) && !anyDone) || (cancelled && untouched)
}

// wordOf is the last word on the host for the thing.
func (o *oracle) wordOf(host, thing string) oracleWord {
	if words := o.words[host][thing]; len(words) > 0 {
		return words[len(words)-1]
	}

	return oracleWord{}
}

// retract takes back what the ends of a lease said: it is open again.
func (o *oracle) retract(l *fleet.Lease) {
	for thing, words := range o.words[l.Host] {
		o.words[l.Host][thing] = slices.DeleteFunc(words, func(w oracleWord) bool { return w.lease == l.ID })
	}
}

// forget drops the record of a host that a person forgot: a host that returns
// under the name is a new host.
func (o *oracle) forget(host string) {
	for thing := range o.words[host] {
		if o.wordOf(host, thing).broke {
			o.counts.forgottenBroken++

			break
		}
	}

	delete(o.words, host)
}

// noteEnd writes down what the end of a lease says about its host, for every
// thing the lease carried a step of:
//
//   - released (the oracle has judged the readiness itself, checkRelease)
//     with every step of the thing done: the host is in order;
//   - failed: the host is broken, unless every step of the thing was done
//     and the agent reported another step failed; or a person had ended the
//     lease, the agent reported its operation as ended, and by that report
//     no step of the thing failed and none was done (with one of them done
//     something was applied and the host never shown to be back: broken);
//     or a person had ended the lease and no step of the thing ever began;
//   - anything else (expired; released with a step of the thing not done)
//     says nothing, and what was said before stands.
func (o *oracle) noteEnd(f *simFacts, snap *simSnapshot, l *fleet.Lease) {
	things := make(map[string]bool, 2)
	for _, thing := range o.carries[l.ID] {
		things[thing] = true
	}

	report := oracleReport(f, l)
	stepFailed := report != nil && slices.ContainsFunc(report.Steps,
		func(s protocol.Step) bool { return s.Status == protocol.StepFailed })
	spared := false

	for thing := range things {
		done, anyDone, failed, untouched := true, false, false, true
		rollout := ""

		for need, of := range o.carries[l.ID] {
			if of != thing {
				continue
			}

			status := protocol.StepStatus("")
			if report != nil {
				status = stepStatus(report, need)
			}

			done = done && status == protocol.StepDone
			anyDone = anyDone || status == protocol.StepDone
			failed = failed || status == protocol.StepFailed
			untouched = untouched && status == protocol.StepCancelled

			if id := o.serves[l.ID][need]; id != "" && (rollout == "" || id < rollout) {
				rollout = id
			}
		}

		var broke bool

		switch {
		case l.State == fleet.LeaseReleased && done:
		case l.State != fleet.LeaseFailed:
			continue
		case done && stepFailed:
			continue
		case failed:
			broke = true
		case oracleCancelSpares(f, l, anyDone, untouched):
			spared = true

			continue
		default:
			broke = true
		}

		if o.words[l.Host] == nil {
			o.words[l.Host] = make(map[string][]oracleWord, 2)
		}

		was := o.wordOf(l.Host, thing)
		o.words[l.Host][thing] = append(o.words[l.Host][thing], oracleWord{lease: l.ID, rollout: rollout, broke: broke})

		switch r := snap.rollout(rollout); {
		case !broke && was.broke:
			o.counts.repaired++
		case !broke:
		case l.Error == oracleEndedByOperator:
			o.counts.brokenByOperator++
		case r != nil && r.State == fleet.RolloutAborted:
			o.counts.brokenAfterAbort++
		}
	}

	if spared {
		o.countSpared(l)
	}
}

// countSpared counts a failed lease that a person had ended and that said
// nothing about its host for some thing: by whether it ended on the agent's
// report, or failed for another reason with that thing never begun.
func (o *oracle) countSpared(l *fleet.Lease) {
	if l.Error == oracleEndedByOperator {
		o.counts.sparedByCancel++
	} else {
		o.counts.sparedUntouched++
	}
}

// checkCancel looks at a lease that a person ended. For as long as it is
// open its host is on controller hold: the hold is part of the cancel, and
// no unhold takes it away. It is never released: a person took the host out.
func (o *oracle) checkCancel(f *simFacts, l *fleet.Lease) error {
	if _, cancelled := f.cancelled[l.ID]; !cancelled {
		return nil
	}

	if l.State == fleet.LeaseReleased {
		return fmt.Errorf("lease %s of %s was released although a person ended it", l.ID, l.Host)
	}

	host := f.hosts[l.Host]

	if !l.State.Terminal() && host != nil && !host.holdWanted {
		return fmt.Errorf("lease %s of %s was ended by a person, is still open (%s), and its host is not on "+
			"controller hold", l.ID, l.Host, l.State)
	}

	if o.cancelSeen[l.ID] {
		return nil
	}

	o.cancelSeen[l.ID] = true

	if host != nil && !host.held() {
		return fmt.Errorf("lease %s of %s was ended by a person and its host is not held", l.ID, l.Host)
	}

	return nil
}

// checkReports: what an agent reports is not ignored. After a committed
// reconcile, an operation that stopped something and that the agent reports
// in a sync from after its lease ended, for a lease that ended without the
// agent's final report, has its lease open again (or ended again, with the
// report): the host is down under it, or was. An operation that stopped
// nothing, reported that way for a lease that failed, has that lease expired.
// And an operation for a lease this database has never seen is adopted.
func (o *oracle) checkReports(f *simFacts, snap *simSnapshot) error {
	if !f.fresh {
		return nil
	}

	for name, host := range f.hosts {
		if host.status == nil || host.status.Operation == nil {
			continue
		}

		op := host.status.Operation
		l := snap.lease(op.LeaseID)

		// The agent says that it never started the lease. If the lease failed
		// without the agent's word, it did not fail: it stopped nothing, and
		// is expired.
		if !op.Disrupted {
			end, ended := o.ended[op.LeaseID]
			if op.Phase == protocol.PhaseDone && l != nil && l.State == fleet.LeaseFailed && l.Host == name &&
				ended && !end.final && host.lastSync.After(end.at) && !host.disrupted[l.ID] {
				return fmt.Errorf("%s reports in a sync from after lease %s failed (%q) that it never started it, "+
					"and the lease is still failed", name, l.ID, l.Error)
			}

			continue
		}

		// An operation that is over does not displace a lease the host was
		// granted since: the host is up, and may start that one at any moment.
		other := slices.ContainsFunc(snap.leases, func(x *fleet.Lease) bool {
			return x.Host == name && x.ID != op.LeaseID && !x.State.Terminal()
		})
		if op.Phase.Terminal() && other {
			continue
		}

		if l == nil {
			return fmt.Errorf("%s reports operation %s (%s), which stopped something, and no lease was adopted for it",
				name, op.LeaseID, op.Phase)
		}

		end, ended := o.ended[l.ID]
		if !l.State.Terminal() || l.Host != name || !ended || end.final || !host.lastSync.After(end.at) {
			continue
		}

		// The oracle saw it end without the report and still sees it ended
		// without it.
		if l.Operation == nil || !l.Operation.Phase.Terminal() {
			return fmt.Errorf("%s reports operation %s (%s) in a sync from after its lease ended as %s (%q) without "+
				"the agent's final report: the lease was not taken up again", name, op.LeaseID, op.Phase, l.State, l.Error)
		}
	}

	return nil
}

// oracleEndedByOperator is the failure reason of a lease that a person ended.
const oracleEndedByOperator = "ended by operator"

// oracleBrokenNote starts the note of a waiting member whose host is broken.
const oracleBrokenNote = "broken since lease "

// broken reports whether the host is broken for the thing the rollout is
// about: the last lease that said something about it failed the host.
func (o *oracle) broken(r *fleet.Rollout, host string) bool {
	return o.wordOf(host, oracleGroup(r)).broke
}

// checkBroken holds what the scheduler says about broken hosts against the
// oracle's record of how the leases ended. After a reconcile, in every open
// rollout, a member that waits is in the repair stage exactly when its host
// is broken for the thing the rollout is about, and its note then names the
// lease that broke it. Nothing else decides it: not the rollout the host
// failed in, how that one was closed, or what followed it.
func (o *oracle) checkBroken(f *simFacts, snap *simSnapshot) error {
	if !f.fresh {
		return nil
	}

	for _, r := range snap.rollouts {
		if r.State.Terminal() {
			continue
		}

		for i := range r.Hosts {
			m := &r.Hosts[i]
			if m.State != fleet.HostPending && m.State != fleet.HostExpected {
				continue
			}

			word := o.wordOf(m.Host, oracleGroup(r))
			first := m.Stage == fleet.StageRepair
			noted := strings.HasPrefix(m.Error, oracleBrokenNote)

			if first != word.broke || noted != word.broke {
				return fmt.Errorf("member %s of rollout %s (%s) is in stage %d with the note %q, but by the leases "+
					"the oracle saw end its host is broken: %v (%+v)", m.Host, r.ID, r.Key, m.Stage, m.Error,
					word.broke, word)
			}

			want := oracleBrokenNote + word.lease
			if word.rollout != "" {
				want += " (rollout " + word.rollout + ")"
			}

			if word.broke && m.Error != want {
				return fmt.Errorf("member %s of rollout %s (%s) carries the note %q, the lease that broke its host "+
					"is %s of rollout %q", m.Host, r.ID, r.Key, m.Error, word.lease, word.rollout)
			}
		}
	}

	return nil
}

// inFlight returns the lease that is in flight for the rollout, or nil: an
// open lease that carries a step of the thing the rollout is about, which the
// oracle first saw served for another rollout that is closed by now (or for
// none). While there is one, nobody who is not broken is granted in the
// rollout.
func (o *oracle) inFlight(snap *simSnapshot, r *fleet.Rollout) *fleet.Lease {
	thing := oracleGroup(r)
	if thing == "" {
		return nil
	}

	var found *fleet.Lease

	for _, l := range snap.leases {
		if l.State.Terminal() || (found != nil && found.Host <= l.Host) {
			continue
		}

		for need, of := range o.carries[l.ID] {
			id := o.serves[l.ID][need]
			if of != thing || id == r.ID {
				continue
			}

			if other := snap.rollout(id); other == nil || other.State.Terminal() {
				found = l
			}
		}
	}

	return found
}

// countFlight counts a host that a lease in flight held back, by what makes
// the case one the simulation wants to have seen.
func (o *oracle) countFlight(snap *simSnapshot, r *fleet.Rollout, l *fleet.Lease) {
	o.counts.flightHeld++

	if o.reopened[l.ID] {
		o.counts.flightReopened++
	}

	if r.CreatedBy != actorScheduler {
		o.counts.flightRetried++
	}

	for need, of := range o.carries[l.ID] {
		from := snap.rollout(o.serves[l.ID][need])
		if of != oracleGroup(r) || from == nil || !from.State.Terminal() {
			continue
		}

		if from.State == fleet.RolloutAborted {
			o.counts.flightAborted++
		}

		if from.EndedAt != nil && r.CreatedAt.Before(*from.EndedAt) {
			o.counts.flightNextToOpen++
		}
	}
}

// checkStates: terminal states are terminal, nothing disappears, one
// non-terminal rollout per key and one non-terminal lease per host. The one
// exception: a lease that expired as not started is reopened when the world
// says the agent did stop something for it.
func (o *oracle) checkStates(f *simFacts, snap *simSnapshot) error {
	keys := make(map[string]string, len(snap.rollouts))
	seen := make(map[string]bool, len(snap.rollouts)+len(snap.leases))

	for _, r := range snap.rollouts {
		seen[r.ID] = true

		if was, ok := o.rollouts[r.ID]; ok && was.Terminal() && r.State != was {
			return fmt.Errorf("rollout %s left terminal state %s for %s", r.ID, was, r.State)
		}

		if r.State.Terminal() {
			continue
		}

		if other, ok := keys[r.Key]; ok {
			return fmt.Errorf("key %s has two open rollouts: %s and %s", r.Key, other, r.ID)
		}

		keys[r.Key] = r.ID
	}

	hosts := make(map[string]string, len(snap.leases))

	for _, l := range snap.leases {
		seen[l.ID] = true

		if was, ok := o.leases[l.ID]; ok && was.Terminal() && l.State != was {
			if !o.resumed(f, l, was) {
				return fmt.Errorf("lease %s left terminal state %s for %s", l.ID, was, l.State)
			}
		}

		if l.State.Terminal() {
			continue
		}

		if other, ok := hosts[l.Host]; ok {
			return fmt.Errorf("host %s holds two leases: %s and %s", l.Host, other, l.ID)
		}

		hosts[l.Host] = l.ID
	}

	for id := range o.rollouts {
		if !seen[id] {
			return fmt.Errorf("rollout %s disappeared", id)
		}
	}

	for id := range o.leases {
		if !seen[id] {
			return fmt.Errorf("lease %s disappeared", id)
		}
	}

	return nil
}

// resumed reports whether a lease may have left a terminal state: it had
// expired and the world says the agent did stop something for it; or it had
// failed without the agent's final report, and the agent reports the
// operation in a sync from after that end. When the world says that the
// agent stopped nothing under such a failed lease, the one thing that may
// have happened to it is that it is expired now: the agent says it never
// started it.
func (o *oracle) resumed(f *simFacts, l *fleet.Lease, was fleet.LeaseState) bool {
	host := f.hosts[l.Host]
	if host == nil {
		return false
	}

	switch was {
	case fleet.LeaseExpired:
		return host.disrupted[l.ID]
	case fleet.LeaseFailed:
		end := o.ended[l.ID]
		op := host.status.Operation
		back := !end.final && op != nil && op.LeaseID == l.ID && host.lastSync.After(end.at)

		if !host.disrupted[l.ID] {
			return back && l.State == fleet.LeaseExpired && op.Phase == protocol.PhaseDone && !op.Disrupted
		}

		return back
	default:
		return false
	}
}

// checkLeaseEnd looks at a lease that changed state. A failed lease halts
// the rollouts it served; a released or expired one must agree with what the
// agent really did.
func (o *oracle) checkLeaseEnd(f *simFacts, snap *simSnapshot, l *fleet.Lease) error {
	host := f.hosts[l.Host]

	if l.State.Terminal() {
		o.leaseEnd[l.Host] = f.now
		o.ended[l.ID] = oracleEnd{
			at: f.now, stamp: cloneTime(l.EndedAt), final: l.Operation != nil && l.Operation.Phase.Terminal(),
		}

		if o.repairs[l.ID] {
			o.repairEnded = true
		}

		if l.Error == oracleEndedByOperator {
			o.counts.endedByOperator++
		}
	}

	if err := checkRecordedReport(f, l); err != nil {
		return err
	}

	// What the end says about the host is written down once the checks below
	// have found it in order.
	defer o.noteEnd(f, snap, l)

	switch l.State {
	case fleet.LeaseFailed:
		if err := o.checkFailure(f, l, host); err != nil {
			return err
		}

		// A controller outage is not the agent's silence: the agent has
		// leaseTtl from the controller's start to be heard.
		if l.Error == reasonAgentSilent {
			heard := f.since
			if host != nil {
				heard = later(host.lastSync, f.since)
			}

			if silent := f.now.Sub(heard); silent < f.settings.LeaseTTL.Std() {
				return fmt.Errorf("lease %s of %s failed as %q after %s of silence since the later of its last "+
					"sync and the controller's start, leaseTtl is %s", l.ID, l.Host, l.Error, silent,
					f.settings.LeaseTTL.Std())
			}
		}

		// A controller outage is no time in which the host could have been
		// found ready either: the readiness timeout runs from the later of the
		// end of the operation and the controller's start.
		if strings.HasPrefix(l.Error, "not ready after") && host != nil {
			if doneAt, ok := host.doneAt[l.ID]; ok {
				waited := f.now.Sub(later(doneAt, f.since))
				if timeout := f.settings.ReadinessTimeout.Std(); waited <= timeout {
					return fmt.Errorf("lease %s of %s failed as %q %s after the later of the end of its operation "+
						"and the controller's start, readinessTimeout is %s", l.ID, l.Host, l.Error, waited, timeout)
				}
			}
		}

		o.haltedBy(f, snap, l)
	case fleet.LeaseExpired:
		// An agent whose clock is slow may have started the lease after the
		// controller had every reason to think it would not: the lease is
		// then taken up again when the agent says so.
		if host != nil && host.disrupted[l.ID] && !host.late[l.ID] {
			return fmt.Errorf("lease %s of %s expired as if it cost nothing, but the agent stopped containers for it",
				l.ID, l.Host)
		}
	case fleet.LeaseReleased:
		if host == nil {
			return fmt.Errorf("lease %s released for unknown host %s", l.ID, l.Host)
		}

		if err := o.checkRelease(f, l, host); err != nil {
			return err
		}

		// A lease that the oracle found ready is a success of every rollout
		// for which, by the oracle's record of what it served, all its steps
		// are done. A lease that was taken up again serves only the rollouts
		// in which, by the oracle's own book, its member was to be leased
		// again (servesAgain; haltedBy has the same rule): where a person
		// skipped the host meanwhile, or the member was settled some other
		// way, the lease is no success of that rollout, and the rollout still
		// sends a canary first.
		for _, id := range o.serves[l.ID] {
			if l.Force || !o.oracleStepsDone(f, l, id) {
				continue
			}

			if o.reopened[l.ID] && !o.again[l.ID][id] {
				continue
			}

			o.succeeded[id] = true
		}
	default:
	}

	return nil
}

// checkRecordedReport: the row of an ended lease keeps the agent's final
// report as the agent sent it. An end is judged by the steps of that report,
// so the scheduler must not have changed one.
func checkRecordedReport(f *simFacts, l *fleet.Lease) error {
	report := oracleReport(f, l)
	if !l.State.Terminal() || report == nil || l.Operation == nil ||
		!report.Phase.Terminal() || !l.Operation.Phase.Terminal() {
		return nil
	}

	if !slices.Equal(report.Steps, l.Operation.Steps) {
		return fmt.Errorf("lease %s of %s ended with the steps %v on record, but its agent reported %v",
			l.ID, l.Host, l.Operation.Steps, report.Steps)
	}

	return nil
}

// checkFailure: a lease fails for a reason the world knows.
//
//   - It does not fail while its agent reports the operation as going on,
//     unless the agent has been silent since. In particular, ending a lease
//     fails nothing: the lease stays open, with its place in the budget and
//     the host limit, until the agent has stopped.
//   - It fails as ended by operator only when a person ended it and the
//     agent's report that the operation ended is on record; and with that
//     report on a lease a person ended, that is its reason.
func (o *oracle) checkFailure(f *simFacts, l *fleet.Lease, host *simHostFact) error {
	_, cancelled := f.cancelled[l.ID]

	if host != nil && host.status != nil && l.Error != reasonAgentSilent {
		if op := host.status.Operation; op != nil && op.LeaseID == l.ID && !op.Phase.Terminal() {
			return fmt.Errorf("lease %s of %s failed as %q while its agent reports the operation in phase %s",
				l.ID, l.Host, l.Error, op.Phase)
		}
	}

	switch {
	case l.Error == oracleEndedByOperator && !cancelled:
		return fmt.Errorf("lease %s of %s failed as %q, but nobody ended it", l.ID, l.Host, l.Error)
	case l.Error == oracleEndedByOperator && !oracleEndedOnReport(f, l):
		return fmt.Errorf("lease %s of %s failed as %q without the agent's report that its operation ended",
			l.ID, l.Host, l.Error)
	case oracleEndedOnReport(f, l) && l.Error != oracleEndedByOperator:
		return fmt.Errorf("lease %s of %s was ended by a person, its agent reports the operation as ended, and it "+
			"failed as %q", l.ID, l.Host, l.Error)
	}

	return nil
}

// haltedBy records the rollouts a failed lease halts: all it served, except
// those whose own steps are done while another step failed. A lease that a
// person ended spares none.
func (o *oracle) haltedBy(f *simFacts, snap *simSnapshot, l *fleet.Lease) {
	report := oracleReport(f, l)
	stepFailed := report != nil && slices.ContainsFunc(report.Steps,
		func(s protocol.Step) bool { return s.Status == protocol.StepFailed })

	done := make(map[string]bool, len(l.Rollouts))
	for _, id := range l.Rollouts {
		done[id] = stepFailed
	}

	for needID, id := range l.Rollouts {
		if report == nil || stepStatus(report, needID) != protocol.StepDone {
			done[id] = false
		}
	}

	for id, spared := range done {
		r := snap.rollout(id)
		if r == nil || r.State.Terminal() || spared || r.Host(l.Host) == nil {
			continue
		}

		// A lease that was taken up again serves only the rollouts in which the
		// host was still waiting then (or still failed by it), by the oracle's
		// own book (servesAgain).
		if o.reopened[l.ID] && !o.again[l.ID][id] {
			continue
		}

		o.halted[id] = l.ID
	}
}

// checkHalts: a halted rollout stays halted until a person retries it or it
// closes.
func (o *oracle) checkHalts(snap *simSnapshot) error {
	for id, lease := range o.halted {
		r := snap.rollout(id)

		switch {
		case r == nil || r.State.Terminal():
			delete(o.halted, id)
		case r.State != fleet.RolloutHalted:
			return fmt.Errorf("rollout %s is %s although lease %s failed in it and nobody retried it",
				id, r.State, lease)
		}
	}

	return nil
}

// checkHolds: the scheduler lifts a controller hold only when a person asked
// for it. A controller that does not know a host (lost database, forgotten
// host) keeps the hold the agent reports.
func (o *oracle) checkHolds(f *simFacts, snap *simSnapshot) error {
	for _, d := range snap.directives {
		if o.directives[d.ID] {
			continue
		}

		o.directives[d.ID] = true

		if d.Kind == protocol.DirectiveReleaseHold && f.holds[d.Host] {
			return fmt.Errorf("directive %s lifts the controller hold of %s, which a person set and nobody lifted",
				d.ID, d.Host)
		}
	}

	return nil
}

// checkRelease: a lease is released only after the agent finished an
// operation that the world has seen recover: the node back at head with
// peers on every sync of readyEpochs epoch durations and, for a host with
// validators, the validators attesting again for readyEpochs complete
// epochs. Only a beacon node that does not serve liveness takes the second
// part away.
func (o *oracle) checkRelease(f *simFacts, l *fleet.Lease, host *simHostFact) error {
	doneAt, ok := host.doneAt[l.ID]
	if !ok {
		return fmt.Errorf("lease %s of %s released although the agent never finished it", l.ID, l.Host)
	}

	if l.Force {
		return nil
	}

	// A need that the operation served and that the agent still reports was
	// not served: releasing the lease would serve it again, without end.
	for i := range l.Needs {
		n := &l.Needs[i]
		if n.DirectiveID != "" || l.Operation == nil || stepStatus(l.Operation, n.ID) != protocol.StepDone ||
			host.status == nil {
			continue
		}

		if slices.ContainsFunc(host.status.Needs, func(r protocol.Need) bool {
			return r.ID == n.ID && r.DirectiveID == ""
		}) {
			return fmt.Errorf("lease %s of %s released although the agent still reports need %s, which it served",
				l.ID, l.Host, n.ID)
		}
	}

	epochs := f.settings.ReadyEpochs

	// The validators attest again. Not knowing whether they do is not enough.
	if host.weight > 0 && !f.health.LivenessUnsupported {
		if !f.health.LivenessKnown {
			return fmt.Errorf("lease %s of %s released although the beacon node has not said whether its %d "+
				"validators attest", l.ID, l.Host, host.weight)
		}

		// The smallest count that is at least minParticipationPercent of them.
		want := uint64(1)
		for float64(want)*100 < f.settings.MinParticipationPercent*float64(host.weight)-1e-6 {
			want++
		}

		for i := range epochs {
			if !f.havePublished || f.publishedEpoch < i {
				return fmt.Errorf("lease %s of %s released with fewer than %d liveness epochs", l.ID, l.Host, epochs)
			}

			e := f.publishedEpoch - i
			live, ok := f.rows[e][l.Host]

			switch {
			case f.epochStart(e).Before(doneAt):
				return fmt.Errorf("lease %s of %s released after %d of %d complete epochs since the agent was done",
					l.ID, l.Host, i, epochs)
			case !ok || live < want:
				return fmt.Errorf("lease %s of %s released although %d of %d validators attested in epoch %d (need %d)",
					l.ID, l.Host, live, host.weight, e, want)
			}
		}
	}

	// The node is back at head with peers: for every host, whatever the
	// beacon node says about its validators.
	peers := oracleMinPeers(f)

	if host.status == nil || !host.lastSync.After(doneAt) || !oracleNodeBack(host.status.Node, peers) {
		return fmt.Errorf("lease %s of %s released although its node is not seen at head with %d peers after the "+
			"operation (done at %s, last sync %s, node %s)", l.ID, l.Host, peers, doneAt.Format(time.TimeOnly),
			host.lastSync.Format(time.TimeOnly), oracleNode(host.status))
	}

	// The agent is still heard: an old sync says nothing about now.
	if f.stale(host) {
		return fmt.Errorf("lease %s of %s released although its agent is silent: last sync %s ago, controller "+
			"started %s ago", l.ID, l.Host, f.now.Sub(host.lastSync), f.now.Sub(f.since))
	}

	// It was seen so on every sync of readyEpochs epochs: some sync of the run
	// the oracle recorded lies that long before its newest one, measured in
	// the epochs that begin with the one that sync was in. (Which sync the
	// scheduler counts from depends on which reconciles were committed.)
	run, running := o.runs[l.ID]
	if !running {
		return fmt.Errorf("lease %s of %s released although no run of syncs with the node at head with %d peers "+
			"is on record", l.ID, l.Host, peers)
	}

	spanned := false

	var window time.Duration

	for _, at := range run.syncs {
		// A sync from before the chain began counts from its genesis: no
		// epoch begins earlier.
		at = later(at, f.epochStart(0))
		first := f.epochOf(at)
		window = f.epochStart(first + epochs).Sub(f.epochStart(first))

		if epochs == 0 || run.newest.Sub(at) >= window {
			spanned = true

			break
		}
	}

	if !spanned {
		return fmt.Errorf("lease %s of %s released on a run of syncs with the node at head with %d peers that "+
			"spans %s, it must span %s (first sync %s, newest %s, controller started %s)", l.ID, l.Host, peers,
			run.newest.Sub(run.first), window, run.first.Format(time.TimeOnly), run.newest.Format(time.TimeOnly),
			f.since.Format(time.TimeOnly))
	}

	return nil
}

// oracleMinPeers is the number of peers each client of a node must have:
// minPeers, but no more than half of the other hosts that run a node (rounded
// up, at least 1).
func oracleMinPeers(f *simFacts) int {
	nodes := 0

	for _, h := range f.hosts {
		if h.status != nil && (h.status.Node.EL != nil || h.status.Node.CL != nil) {
			nodes++
		}
	}

	// Half of the nodes-1 others, rounded up.
	half := nodes / 2

	switch {
	case half < 1:
		return 1
	case f.settings.MinPeers > half:
		return half
	default:
		return f.settings.MinPeers
	}
}

// oracleNodeBack reports whether the probes of one sync show the node at
// head with peers. A probe that is not configured says nothing.
func oracleNodeBack(n protocol.NodeHealth, peers int) bool {
	elOK := n.EL == nil || (n.EL.Reachable && !n.EL.Syncing && n.EL.Peers >= peers)
	clOK := n.CL == nil || (n.CL.Reachable && !n.CL.Syncing && !n.CL.ELOffline && n.CL.Peers >= peers)

	return elOK && clOK
}

// oracleNode renders the probes of a sync for a failure report.
func oracleNode(req *protocol.SyncRequest) string {
	if req == nil {
		return "unknown"
	}

	out := ""

	if el := req.Node.EL; el != nil {
		out += fmt.Sprintf("EL reachable=%v syncing=%v peers=%d ", el.Reachable, el.Syncing, el.Peers)
	}

	if cl := req.Node.CL; cl != nil {
		out += fmt.Sprintf("CL reachable=%v syncing=%v elOffline=%v peers=%d", cl.Reachable, cl.Syncing, cl.ELOffline, cl.Peers)
	}

	if out == "" {
		return "has no probe"
	}

	return out
}

// oracleGate evaluates the network gate from the world. leased are the hosts
// holding a lease; their validators are left out of the participation.
func oracleGate(f *simFacts, leased map[string]uint64) []string {
	var (
		reasons              []string
		n                    = &f.health
		current              = f.epochOf(n.UpdatedAt)
		epoch                = f.epochStart(current + 1).Sub(f.epochStart(current))
		tracked, total, live uint64
	)

	if n.UpdatedAt.IsZero() || f.now.Sub(n.UpdatedAt) > 2*epoch {
		reasons = append(reasons, "beacon node unreachable")
	}

	if n.FinalityLagEpochs > f.settings.MaxFinalityLagEpochs {
		reasons = append(reasons, "finality lag")
	}

	// Participation is judged over what no lease took offline: of a host
	// that holds a lease, the validators that attested when the lease began
	// are left out; those that were offline already still count, as offline.
	for name, h := range f.hosts {
		tracked += h.weight

		if charged, held := leased[name]; held {
			total += h.weight - min(charged, h.weight)
		} else {
			total += h.weight
			live += h.live
		}
	}

	switch {
	case tracked == 0:
	case n.LivenessUnsupported:
		// No liveness to be had: finality and freshness alone gate.
	case !n.LivenessKnown || !f.havePublished || n.CurrentEpoch > f.publishedEpoch+2:
		reasons = append(reasons, "liveness data stale")
	case total > 0 && float64(live)*100/float64(total) < f.settings.MinParticipationPercent:
		reasons = append(reasons, "participation")
	}

	return reasons
}

// oracleFits checks the three grant conditions for one host against the
// world's own count of unavailable weight: it costs nothing, it stays inside
// the budget, or it goes alone.
func oracleFits(f *simFacts, leased map[string]uint64, host string) error {
	var total, unavailable uint64

	for name, h := range f.hosts {
		total += h.weight

		if _, held := leased[name]; held {
			unavailable += h.weight
		} else {
			unavailable += h.weight - h.live
		}
	}

	allowed := f.settings.MaxUnavailablePercent * float64(total) / 100
	cost := f.hosts[host].live

	fits := cost == 0 ||
		float64(unavailable+cost) <= allowed+budgetEpsilon ||
		(unavailable == 0 && allowed > 0 && len(leased) == 0)
	if !fits {
		return fmt.Errorf("over budget: cost %d with %d of %d already unavailable, %v allowed and %d other leases",
			cost, unavailable, total, allowed, len(leased))
	}

	return nil
}

// oracleBudget checks the three grant conditions and the host-count limit
// for one host.
func oracleBudget(f *simFacts, leased map[string]uint64, host string) error {
	if err := oracleFits(f, leased, host); err != nil {
		return err
	}

	limit := max(1, int(math.Floor(f.settings.MaxUnavailablePercent*float64(len(f.hosts))/100+budgetEpsilon)))
	if len(leased) >= limit {
		return fmt.Errorf("host limit: %d hosts already hold a lease, at most %d of %d may", len(leased), limit, len(f.hosts))
	}

	return nil
}

// oracleHeld reports whether the host holds a lease.
func oracleHeld(leased map[string]uint64, host string) bool {
	_, held := leased[host]

	return held
}

// oracleOffline reports whether the host is established as offline: it has
// validators and none of them attested in the two newest published epochs,
// both of which began after the oracle saw the host's last lease end and
// after the database first saw the host.
func (o *oracle) oracleOffline(f *simFacts, name string) bool {
	host := f.hosts[name]
	if host == nil || host.weight == 0 || f.health.LivenessUnsupported || !f.havePublished || f.publishedEpoch == 0 {
		return false
	}

	for _, e := range []uint64{f.publishedEpoch, f.publishedEpoch - 1} {
		if live, ok := f.rows[e][name]; !ok || live != 0 {
			return false
		}
	}

	return f.epochStart(f.publishedEpoch - 1).After(later(o.leaseEnd[name], f.registered[name]))
}

// repairOpen reports whether a lease that passed a closed gate is open, or
// ended in this very reconcile: a repair has ended a reconcile before the
// next begins.
func (o *oracle) repairOpen(snap *simSnapshot) bool {
	return o.repairEnded || slices.ContainsFunc(snap.leases, func(l *fleet.Lease) bool {
		return o.repairs[l.ID] && !l.State.Terminal()
	})
}

// oracleRepairs reports whether a closed gate lets the repair of the host
// pass: the gate is closed on data it has (finality, participation), no
// other repair is open, and the host is established as offline.
func (o *oracle) oracleRepairs(v *oracleView, name string) error {
	switch {
	case slices.Contains(v.gate, "beacon node unreachable"), slices.Contains(v.gate, "liveness data stale"):
		return fmt.Errorf("the network gate fails for want of data: %v", v.gate)
	case o.repairOpen(v.snap):
		return fmt.Errorf("the network gate fails (%v) and another repair is open", v.gate)
	case !o.oracleOffline(v.f, name):
		return fmt.Errorf("the network gate fails (%v) and the host is not established as offline: none of its "+
			"validators attesting in the two newest epochs, both begun after its last lease ended", v.gate)
	default:
		return nil
	}
}

// checkGrant checks one new lease.
func (o *oracle) checkGrant(v *oracleView, l *fleet.Lease) error {
	f, snap := v.f, v.snap
	host := f.hosts[l.Host]

	switch {
	case l.State != fleet.LeaseGranted:
		return fmt.Errorf("new lease is %s, not granted", l.State)
	case host == nil || host.status == nil:
		return errors.New("the host is unknown or has never synced")
	case oracleHeld(v.leased, l.Host):
		return errors.New("the host already holds a lease")
	case host.held():
		return errors.New("the host is held")
	case f.stale(host):
		return fmt.Errorf("the host is stale: last sync %s ago, controller started %s ago",
			f.now.Sub(host.lastSync), f.now.Sub(f.since))
	case f.settings.Paused:
		return errors.New("the global pause is in effect")
	case len(l.Needs) == 0:
		return errors.New("the lease carries no need")
	}

	exempt := true
	rollouts := make([]*fleet.Rollout, 0, len(l.Rollouts))

	for i := range l.Needs {
		n := &l.Needs[i]

		if !slices.ContainsFunc(host.status.Needs, func(r protocol.Need) bool { return r.ID == n.ID }) {
			return fmt.Errorf("need %s is not one the agent reports", n.ID)
		}

		if !slices.Contains(host.status.Verbs, n.Kind.Verb()) {
			return fmt.Errorf("need %s wants verb %s, which the agent does not allow", n.ID, n.Kind.Verb())
		}

		if key := needKey(n); key != "" && f.skipped[key][l.Host] {
			return fmt.Errorf("need %s: a person skipped the host for %s and nobody retried it", n.ID, key)
		}

		r := snap.rollout(l.Rollouts[n.ID])
		if r == nil {
			return fmt.Errorf("need %s is served for no rollout", n.ID)
		}

		if !slices.Contains(rollouts, r) {
			rollouts = append(rollouts, r)
			exempt = exempt && r.IgnoreBudget
		}
	}

	// A closed gate lets nothing pass but one repair at a time, of a host
	// that is established as offline. A repair does not wait for its place in
	// the order of a rollout.
	repair := !exempt && len(v.gate) > 0
	if repair {
		if err := o.oracleRepairs(v, l.Host); err != nil {
			return err
		}
	}

	o.grants[l.ID] = true

	for _, r := range rollouts {
		if err := o.checkRolloutGrant(v, r, l, host, repair); err != nil {
			return fmt.Errorf("rollout %s (%s): %w", r.ID, r.Key, err)
		}
	}

	if repair {
		o.repairs[l.ID] = true
	}

	if exempt {
		return nil
	}

	return oracleBudget(f, v.leased, l.Host)
}

// oraclePolicy says why the policy of a rollout's kind lets nothing be
// granted in it, or "".
func oraclePolicy(f *simFacts, r *fleet.Rollout) string {
	policy := fleet.PolicyAuto

	switch r.Kind {
	case fleet.RolloutImage:
		policy = f.settings.PolicyImage
	case fleet.RolloutPackages:
		policy = f.settings.PolicyPackages
	case fleet.RolloutReboot:
		policy = f.settings.PolicyReboot
	default:
	}

	switch {
	case policy == fleet.PolicyPaused:
		return "the policy is paused"
	case policy == fleet.PolicyManual && !r.Approved:
		return "the policy is manual and nobody approved the rollout"
	default:
		return ""
	}
}

// checkRolloutGrant checks that a rollout may be granted to a host: state,
// pause, policy, halt and ordering. repair: the grant passed a closed gate.
func (o *oracle) checkRolloutGrant(
	v *oracleView, r *fleet.Rollout, l *fleet.Lease, host *simHostFact, repair bool,
) error {
	m := r.Host(l.Host)

	switch {
	case r.State != fleet.RolloutActive:
		return fmt.Errorf("the rollout is %s", r.State)
	case o.halted[r.ID] != "":
		return fmt.Errorf("lease %s failed in it and nobody retried it", o.halted[r.ID])
	case r.Paused:
		return errors.New("the rollout is paused")
	case oraclePolicy(v.f, r) != "":
		return errors.New(oraclePolicy(v.f, r))
	case m == nil || m.State != fleet.HostLeased || m.LeaseID != l.ID:
		return errors.New("the host is not a member leased by this lease")
	case r.Force, repair:
		// A forced rollout has no order, and a repair does not wait for its
		// place in one.
		return nil
	case o.broken(r, l.Host):
		// A host that is broken waits for nobody.
		o.counts.brokenFirst++

		return nil
	}

	return o.checkOrdering(v, r, m, host)
}

// brokenAhead reports whether a member other than the granted one is broken
// and still to be served: it holds a lease, or it waits and its host could
// be granted (or is expected to report).
func (o *oracle) brokenAhead(v *oracleView, r *fleet.Rollout, m *fleet.RolloutHost) bool {
	other := v.f.hosts[m.Host]
	if other == nil || !o.broken(r, m.Host) {
		return false
	}

	return m.State == fleet.HostLeased || oracleServable(v.f, v.snap, r, m, other)
}

// oracleRank is the place of a member among the candidates for the canary.
type oracleRank struct {
	overBudget, notLive, expected int
	weight                        uint64
	name                          string
}

// before reports whether a comes before b: hosts that fit the budget, hosts
// with live validators, hosts that report the need, ascending weight, name.
func (a oracleRank) before(b oracleRank) bool {
	switch {
	case a.overBudget != b.overBudget:
		return a.overBudget < b.overBudget
	case a.notLive != b.notLive:
		return a.notLive < b.notLive
	case a.expected != b.expected:
		return a.expected < b.expected
	case a.weight != b.weight:
		return a.weight < b.weight
	default:
		return a.name < b.name
	}
}

// rankOf ranks a member against the budget as it was before the reconcile
// granted anything.
func oracleRankOf(v *oracleView, m *fleet.RolloutHost, h *simHostFact) oracleRank {
	k := oracleRank{weight: h.weight, name: m.Host}

	if oracleFits(v.f, v.before, m.Host) != nil {
		k.overBudget = 1
	}

	if h.live == 0 {
		k.notLive = 1
	}

	if m.State == fleet.HostExpected {
		k.expected = 1
	}

	return k
}

// checkOrdering checks the grant, under an open gate, of a host that is not
// broken, in a rollout that is not forced. Broken hosts go first: nobody
// else is granted while one is still to be served, nor while a lease is in
// flight for the rollout. Until the rollout has its
// first success, one member goes alone, the canary: nobody is granted while
// another member holds a lease, except next to a repair that passed the
// closed gate. The canary is the first of the members that can be served, in
// this order: those that fit the budget as it was before the reconcile
// granted anything, those with live validators, those that report the need
// before those only expected to, ascending weight, name.
func (o *oracle) checkOrdering(v *oracleView, r *fleet.Rollout, granted *fleet.RolloutHost, host *simHostFact) error {
	for i := range r.Hosts {
		if m := &r.Hosts[i]; m.Host != granted.Host && o.brokenAhead(v, r, m) {
			return fmt.Errorf("granted although the broken host %s (%s) is still to be served", m.Host, m.State)
		}
	}

	// A lease of a closed rollout that is still open is the canary in flight
	// of this one.
	if l := o.inFlight(v.snap, r); l != nil {
		return fmt.Errorf("granted although lease %s of a closed rollout is still open on %s", l.ID, l.Host)
	}

	if o.succeeded[r.ID] {
		return nil
	}

	mine := oracleRankOf(v, granted, host)
	mine.expected = 0

	for i := range r.Hosts {
		m := &r.Hosts[i]

		other := v.f.hosts[m.Host]
		if m.Host == granted.Host || other == nil {
			continue
		}

		if m.State == fleet.HostLeased {
			if o.repairs[m.LeaseID] {
				continue
			}

			return fmt.Errorf("granted before the first success of the rollout while %s holds a lease", m.Host)
		}

		if o.broken(r, m.Host) || !oracleServable(v.f, v.snap, r, m, other) {
			continue
		}

		if theirs := oracleRankOf(v, m, other); theirs.before(mine) {
			return fmt.Errorf("canary %s although %s comes first (%+v before %+v)", granted.Host, m.Host, theirs, mine)
		}
	}

	return nil
}

// checkProgress: a host that nothing holds back is granted. After a committed
// reconcile, a member that waits with a need it may be granted, in a rollout
// that grants (active, not paused, approved, not halted, not forced), with
// the scheduler unpaused, the gate letting it pass (open, or its repair) and
// the budget and the host limit admitting it, can only be waiting for the
// order of its rollout. The order holds nobody back who passes the closed
// gate as a repair, and no broken host. It holds another host back for a
// broken member that is still to be served, for a lease in flight and, before
// the first success,
// for a member that holds a lease, for a member that is busy with a lease of
// another rollout, and for an expected member that comes before it as the
// canary. It never holds anybody back for a member that is held, silent or
// without the verb. A host left waiting without such a reason for
// simStuckPasses reconciles in a row is stuck.
func (o *oracle) checkProgress(v *oracleView) error {
	f := v.f
	seen := make(map[string]bool, len(o.stuck))

	for _, r := range v.snap.rollouts {
		if f.settings.Paused || r.State != fleet.RolloutActive || r.Paused || r.Force ||
			o.halted[r.ID] != "" || oraclePolicy(f, r) != "" {
			continue
		}

		for i := range r.Hosts {
			m := &r.Hosts[i]

			host := f.hosts[m.Host]
			if m.State != fleet.HostPending || host == nil || oracleHeld(v.leased, m.Host) ||
				!oracleServable(f, v.snap, r, m, host) {
				continue
			}

			repair := false

			if !r.IgnoreBudget {
				if len(v.gate) > 0 {
					if o.oracleRepairs(v, m.Host) != nil {
						continue
					}

					repair = true
				}

				if oracleBudget(f, v.leased, m.Host) != nil {
					continue
				}
			}

			if !repair && o.orderHolds(v, r, m, host) {
				continue
			}

			key := r.ID + "/" + m.Host
			seen[key] = true
			o.stuck[key]++

			if o.stuck[key] >= simStuckPasses {
				return fmt.Errorf("host %s is stuck in rollout %s (%s): %d reconciles in a row left it waiting although "+
					"the gate, the budget and the host limit let it pass and the order of the rollout does not hold "+
					"it back", m.Host, r.ID, r.Key, o.stuck[key])
			}
		}
	}

	for key := range o.stuck {
		if !seen[key] {
			delete(o.stuck, key)
		}
	}

	return nil
}

// orderHolds reports whether the order of the rollout holds the waiting
// member back.
func (o *oracle) orderHolds(v *oracleView, r *fleet.Rollout, waiting *fleet.RolloutHost, host *simHostFact) bool {
	if o.broken(r, waiting.Host) {
		return false
	}

	if l := o.inFlight(v.snap, r); l != nil {
		o.countFlight(v.snap, r, l)

		return true
	}

	first := !o.succeeded[r.ID]
	mine := oracleRankOf(v, waiting, host)

	for i := range r.Hosts {
		m := &r.Hosts[i]

		other := v.f.hosts[m.Host]
		if m.Host == waiting.Host || other == nil {
			continue
		}

		if m.State == fleet.HostLeased {
			// Before the first success every lease is waited for; after it, the
			// lease of a broken host.
			if first || o.broken(r, m.Host) {
				return true
			}

			continue
		}

		if !oracleServable(v.f, v.snap, r, m, other) {
			continue
		}

		if o.broken(r, m.Host) {
			return true
		}

		// The canary may be a host that is busy with another lease, or one
		// that is not here yet and comes first all the same.
		if first && (oracleHeld(v.leased, m.Host) ||
			(m.State == fleet.HostExpected && oracleRankOf(v, m, other).before(mine))) {
			return true
		}
	}

	return false
}

// oracleServable reports whether a member could be granted as far as the
// host itself goes: it waits with a need of the rollout that its agent
// allows and is neither held nor stale, or it is expected to report.
func oracleServable(f *simFacts, snap *simSnapshot, r *fleet.Rollout, m *fleet.RolloutHost, host *simHostFact) bool {
	switch m.State {
	case fleet.HostExpected:
		return oracleBlocks(f, snap, r, m, host)
	case fleet.HostPending:
	default:
		return false
	}

	if host.held() || host.status == nil || f.stale(host) {
		return false
	}

	return slices.ContainsFunc(host.status.Needs, func(n protocol.Need) bool {
		if !slices.Contains(host.status.Verbs, n.Kind.Verb()) {
			return false
		}

		if n.DirectiveID == "" {
			return needKey(&n) == r.Key
		}

		return slices.ContainsFunc(snap.directives, func(d *fleet.Directive) bool {
			return d.ID == n.DirectiveID && d.RolloutID == r.ID
		})
	})
}

// oracleBlocks reports whether an expected member is still waited for.
func oracleBlocks(f *simFacts, snap *simSnapshot, r *fleet.Rollout, m *fleet.RolloutHost, host *simHostFact) bool {
	if host.held() || host.status == nil || f.stale(host) ||
		f.now.Sub(later(r.CreatedAt, f.since)) > f.settings.HostStaleAfter.Std() {
		return false
	}

	switch r.Kind {
	case fleet.RolloutImage:
		return slices.Contains(host.status.Verbs, protocol.VerbRoll) &&
			slices.ContainsFunc(host.status.Containers, func(c protocol.Container) bool {
				return c.Managed && !c.Custom && c.Image == r.Image && c.Digest != r.Digest
			})
	case fleet.RolloutManual:
		return slices.ContainsFunc(snap.directives, func(d *fleet.Directive) bool {
			return d.RolloutID == r.ID && d.Host == m.Host && d.State == fleet.DirectivePending
		})
	default:
		return false
	}
}

// checkMembership: every host that reports an observed need for the cause of
// an open rollout is a member of it.
func checkMembership(f *simFacts, snap *simSnapshot) error {
	open := make(map[string]*fleet.Rollout, len(snap.rollouts))

	for _, r := range snap.rollouts {
		if !r.State.Terminal() {
			open[r.Key] = r
		}
	}

	for name, h := range f.hosts {
		if h.status == nil {
			continue
		}

		for i := range h.status.Needs {
			n := &h.status.Needs[i]
			key := ""

			switch {
			case n.DirectiveID != "":
			case n.Kind == protocol.NeedImage:
				key = imageKey(n.Image.Image, n.Image.ToDigest)
			case n.Kind == protocol.NeedPackages:
				key = prefixPackages + protocol.PackagesKey(n.Packages.Packages)
			case n.Kind == protocol.NeedReboot:
				key = keyReboot
			}

			if r := open[key]; r != nil && r.Host(name) == nil {
				return fmt.Errorf("host %s reports need %s but is no member of rollout %s (%s)", name, n.ID, r.ID, r.Key)
			}
		}
	}

	return nil
}
