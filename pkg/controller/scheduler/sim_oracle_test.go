package scheduler

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The oracle holds the committed state of the scheduler against a small set
// of invariants, with the simulated world's own numbers. It is no second
// scheduler: it does not say who should have been granted, only what must
// never happen.
//
//  1. At every grant the availability budget holds by the world's count of
//     who attests, unless every rollout the lease serves ignores the budget.
//  2. At every such grant fewer hosts than the host limit hold a lease.
//  3. A host holds one open lease at most. No lease is granted to a host
//     that is held or stale or works on an operation without a lease, under
//     the global pause, or for a rollout that is closed, halted, paused or
//     not approved. Ended leases and closed rollouts stay as they ended.
//
// A host whose agent reports an operation that is not over while the host
// holds no lease counts as leased in 1, 2 and 4.
//  4. No such grant while the world's gate is closed, with one exception:
//     the gate is closed for participation or finality (not for want of
//     data), none of the host's validators attests and nothing is leased.
//
// 1, 2 and 4 hold per impact class, by the class of the containers the
// lease's needs are about as the host reported them: a node lease is what
// they say; a slot lease costs no validator weight; an independent lease
// counts for neither budget nor host limit and passes a closed gate.
//
//  7. At every grant of an independent lease fewer hosts than the
//     independent limit hold one.
//  5. After a lease failed, nothing is granted in the rollouts it served
//     until a person retries them (or they are closed).
//  6. A lease is released only after its agent reported the operation done.
//     (That an agent begins a step only under an open lease is checked where
//     the steps are seen: sim.checkStarts.)
//
// Restart equivalence and convergence after healing are the driver's
// (sim.reconcile, sim.converge).

// simHostFact is what the simulated world knows about one registered host.
// None of it comes from the scheduler.
type simHostFact struct {
	weight uint64
	// live is how many validators attested in the newest liveness epoch the
	// beacon service settled (the full weight without a row).
	live uint64
	// holdWanted is the operator's hold switch; offline says that the
	// operator's known-offline mark is in force.
	holdWanted bool
	offline    bool
	// lastSync and status are the agent's last sync as it sent it.
	lastSync time.Time
	status   *protocol.SyncRequest
	// reported is the agent's own record, per lease ID, of the last operation
	// it sent to the controller for the lease.
	reported map[string]*protocol.Operation
}

func (h *simHostFact) held() bool {
	return h.holdWanted || h.offline || (h.status != nil && h.status.Hold.Active())
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
	// publishedEpoch is the newest epoch with liveness rows in the store.
	publishedEpoch uint64
	havePublished  bool
	// epoch is the length of the epoch the beacon view was taken in, by the
	// simulated chain's own clock.
	epoch time.Duration
	// retried names the rollouts a person retried since the last
	// observation.
	retried []string
}

// stale: no sync for hostStaleAfter, counted from the controller's start at
// the earliest.
func (f *simFacts) stale(h *simHostFact) bool {
	return f.now.Sub(later(h.lastSync, f.since)) > f.settings.HostStaleAfter.Std()
}

// working: by its last sync the agent is in the middle of an operation. A
// host that is stale says nothing.
func (f *simFacts) working(h *simHostFact) bool {
	return !f.stale(h) && h.status != nil && h.status.Operation != nil &&
		!h.status.Operation.Phase.Terminal()
}

// simSnapshot is the committed scheduler state at one observation, in
// creation order.
type simSnapshot struct {
	rollouts []*fleet.Rollout
	leases   []*fleet.Lease
}

func (s *simSnapshot) rollout(id string) *fleet.Rollout {
	for _, r := range s.rollouts {
		if r.ID == id {
			return r
		}
	}

	return nil
}

// oracle is what the invariants need to remember between two observations.
type oracle struct {
	rollouts map[string]fleet.RolloutState
	leases   map[string]fleet.LeaseState
	// serves maps, per lease, each need to the rollout it is served for, as
	// the oracle first saw it.
	serves map[string]map[string]string
	// halted maps a rollout to the lease whose failure halted it, until a
	// person retries it.
	halted map[string]string
	// exceptions counts the grants that passed a closed gate; working the
	// observations of a host that works without a lease.
	exceptions, working int
}

func newOracle() *oracle {
	return &oracle{
		rollouts: make(map[string]fleet.RolloutState, 16),
		leases:   make(map[string]fleet.LeaseState, 64),
		serves:   make(map[string]map[string]string, 64),
		halted:   make(map[string]string, 4),
	}
}

// observe checks one observation against the history and returns the first
// violated invariant.
func (o *oracle) observe(f *simFacts, snap *simSnapshot) error {
	for _, r := range snap.rollouts {
		if was, known := o.rollouts[r.ID]; known && was.Terminal() && was != r.State {
			return fmt.Errorf("rollout %s was %s and is %s", r.ID, was, r.State)
		}

		o.rollouts[r.ID] = r.State

		if r.State.Terminal() {
			delete(o.halted, r.ID)
		}
	}

	for _, id := range f.retried {
		delete(o.halted, id)
	}

	var (
		granted []*fleet.Lease
		// leased holds the class of every host that holds an open lease.
		leased = make(map[string]protocol.Impact, len(snap.leases))
	)

	for _, l := range snap.leases {
		was, known := o.leases[l.ID]
		o.leases[l.ID] = l.State

		switch {
		case !known:
			o.serves[l.ID] = maps.Clone(l.Rollouts)
			granted = append(granted, l)

			continue
		case was.Terminal() && was != l.State:
			return fmt.Errorf("lease %s of %s was %s and is %s", l.ID, l.Host, was, l.State)
		}

		if !l.State.Terminal() {
			if leased[l.Host] != "" {
				return fmt.Errorf("host %s holds two open leases", l.Host)
			}

			leased[l.Host] = oracleImpact(f.hosts[l.Host], l)
		}

		if l.State == was {
			continue
		}

		if err := o.checkEnd(f, snap, l); err != nil {
			return fmt.Errorf("lease %s of %s: %w", l.ID, l.Host, err)
		}
	}

	// Hosts that work without a lease take a slot like the leased ones.
	for name, h := range f.hosts {
		if leased[name] == "" && f.working(h) {
			leased[name] = protocol.ImpactNode
			o.working++
		}
	}

	// The gate is evaluated once per reconcile, before the first grant.
	gate := oracleGate(f, leased)

	for _, l := range granted {
		if err := o.checkGrant(f, snap, l, leased, gate); err != nil {
			return fmt.Errorf("grant of lease %s to %s: %w", l.ID, l.Host, err)
		}

		leased[l.Host] = oracleImpact(f.hosts[l.Host], l)
	}

	return nil
}

// oracleImpact is the class of a lease by the containers its needs are about,
// as the host last reported them: node for a need about no container or one
// the host does not report.
func oracleImpact(h *simHostFact, l *fleet.Lease) protocol.Impact {
	impact := protocol.ImpactIndependent

	for i := range l.Needs {
		impact = impact.Heavier(oracleNeedImpact(h, &l.Needs[i]))
	}

	return impact
}

// oracleNeedImpact is the class of one need by the container it is about.
func oracleNeedImpact(h *simHostFact, n *protocol.Need) protocol.Impact {
	name := n.Container()
	if h == nil || h.status == nil || name == "" {
		return protocol.ImpactNode
	}

	for _, rc := range h.status.Containers {
		if rc.Name == name && rc.Impact != "" {
			return rc.Impact
		}
	}

	return protocol.ImpactNode
}

// oracleCount counts the leased hosts of the classes.
func oracleCount(leased map[string]protocol.Impact, classes ...protocol.Impact) int {
	n := 0

	for _, impact := range leased {
		if slices.Contains(classes, impact) {
			n++
		}
	}

	return n
}

// checkEnd looks at a lease that changed its state: one that was released
// must have been reported done by its agent, and one that failed halts the
// rollouts it served.
func (o *oracle) checkEnd(f *simFacts, snap *simSnapshot, l *fleet.Lease) error {
	var report *protocol.Operation
	if host := f.hosts[l.Host]; host != nil {
		report = host.reported[l.ID]
	}

	switch l.State {
	case fleet.LeaseReleased:
		if report == nil || report.Phase != protocol.PhaseDone {
			return errors.New("released, and its agent never reported the operation done")
		}
	case fleet.LeaseFailed:
		for _, id := range o.serves[l.ID] {
			r := snap.rollout(id)
			if r == nil || r.State.Terminal() || oracleSpared(report, o.serves[l.ID], id) {
				continue
			}

			if r.State != fleet.RolloutHalted {
				return fmt.Errorf("failed, and rollout %s (%s) it served is %s", r.ID, r.Key, r.State)
			}

			if o.halted[id] == "" {
				o.halted[id] = l.ID
			}
		}
	default:
	}

	return nil
}

// oracleSpared reports whether, by the agent's own report, every step the
// lease served for the rollout was done while another step failed: such a
// rollout is not stopped by the failure.
func oracleSpared(report *protocol.Operation, serves map[string]string, rollout string) bool {
	if report == nil || !slices.ContainsFunc(report.Steps, func(s protocol.Step) bool {
		return s.Status == protocol.StepFailed
	}) {
		return false
	}

	found := false

	for need, id := range serves {
		if id != rollout {
			continue
		}

		if !slices.ContainsFunc(report.Steps, func(s protocol.Step) bool {
			return s.NeedID == need && s.Status == protocol.StepDone
		}) {
			return false
		}

		found = true
	}

	return found
}

// oracleGate evaluates the network gate from the world. leased are the hosts
// that hold a lease or work without one; they are left out of the
// participation.
func oracleGate(f *simFacts, leased map[string]protocol.Impact) []string {
	var (
		reasons              []string
		n                    = &f.health
		tracked, total, live uint64
	)

	if n.UpdatedAt.IsZero() || f.now.Sub(n.UpdatedAt) > 2*f.epoch {
		reasons = append(reasons, gateBeacon)
	}

	if n.FinalityLagEpochs > f.settings.MaxFinalityLagEpochs {
		reasons = append(reasons, "finality lag")
	}

	for name, h := range f.hosts {
		tracked += h.weight

		// A host marked known offline is left out, like one at work.
		if leased[name] == "" && !h.offline {
			total += h.weight
			live += h.live
		}
	}

	switch {
	case tracked == 0:
	case !n.LivenessKnown || !f.havePublished || n.CurrentEpoch > f.publishedEpoch+2:
		reasons = append(reasons, gateLiveness)
	case total > 0 && float64(live)*100/float64(total) < f.settings.MinParticipationPercent:
		reasons = append(reasons, "participation")
	}

	return reasons
}

// oracleBudget checks the three grant conditions (the host costs nothing, it
// stays inside the budget, or it goes alone) and the host limit for one host,
// against the world's own count of unavailable weight.
func oracleBudget(f *simFacts, leased map[string]protocol.Impact, host string, impact protocol.Impact) error {
	if impact == protocol.ImpactIndependent {
		limit := max(1, int(math.Floor(f.settings.MaxIndependentPercent*float64(len(f.hosts))/100+budgetEpsilon)))
		if n := oracleCount(leased, protocol.ImpactIndependent); n >= limit {
			return fmt.Errorf("independent limit: %d hosts already hold an independent lease, at most %d of %d may",
				n, limit, len(f.hosts))
		}

		return nil
	}

	var total, unavailable uint64

	for name, h := range f.hosts {
		// A host marked known offline that holds no lease is not counted.
		if h.offline && leased[name] == "" {
			continue
		}

		total += h.weight

		if leased[name] == protocol.ImpactNode {
			unavailable += h.weight
		} else {
			unavailable += h.weight - h.live
		}
	}

	percent := f.settings.MaxUnavailablePercent
	allowed := percent * float64(total) / 100
	others := oracleCount(leased, protocol.ImpactNode, protocol.ImpactSlot)

	cost := f.hosts[host].live
	if impact != protocol.ImpactNode {
		cost = 0
	}

	fits := cost == 0 ||
		float64(unavailable+cost) <= allowed+budgetEpsilon ||
		(unavailable == 0 && allowed > 0 && others == 0)
	if !fits {
		return fmt.Errorf("over budget: cost %d with %d of %d already unavailable, %v allowed and %d other leases",
			cost, unavailable, total, allowed, others)
	}

	limit := max(1, int(math.Floor(percent*float64(len(f.hosts))/100+budgetEpsilon)))
	if others >= limit {
		return fmt.Errorf("host limit: %d hosts already hold a node or slot lease, at most %d of %d may",
			others, limit, len(f.hosts))
	}

	return nil
}

// checkGrant checks one new lease. leased are the hosts that held a lease
// before it; gate are the reasons the world's gate is closed for.
func (o *oracle) checkGrant(
	f *simFacts, snap *simSnapshot, l *fleet.Lease, leased map[string]protocol.Impact, gate []string,
) error {
	host := f.hosts[l.Host]
	impact := oracleImpact(host, l)

	switch {
	case l.State != fleet.LeaseGranted:
		return fmt.Errorf("new lease is %s, not granted", l.State)
	case host == nil || host.status == nil:
		return errors.New("the host is unknown or has never synced")
	case leased[l.Host] != "":
		return errors.New("the host already holds a lease, or works on an operation without one")
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

	if l.Impact != impact {
		return fmt.Errorf("the lease is of class %q, its needs are %q", l.Impact, impact)
	}

	// bound is the class of the needs whose rollouts do not ignore the
	// budget: they alone answer to the gate and the limits.
	exempt, anyExempt := true, false
	bound := protocol.ImpactIndependent

	for i := range l.Needs {
		r := snap.rollout(l.Rollouts[l.Needs[i].ID])
		if r == nil {
			return fmt.Errorf("need %s is served for no rollout", l.Needs[i].ID)
		}

		if err := o.checkRollout(f, r, l); err != nil {
			return fmt.Errorf("rollout %s (%s): %w", r.ID, r.Key, err)
		}

		anyExempt = anyExempt || r.IgnoreBudget

		if !r.IgnoreBudget {
			exempt = false
			bound = bound.Heavier(oracleNeedImpact(host, &l.Needs[i]))
		}
	}

	// Independent work rides along with other work that takes the host
	// anyway; it needs a place of its own only in a lease of its own.
	if exempt || (bound == protocol.ImpactIndependent && (anyExempt || impact != protocol.ImpactIndependent)) {
		return nil
	}

	if len(gate) > 0 && bound != protocol.ImpactIndependent {
		// The one exception: the gate is closed for what the network does, not
		// for want of data, none of the host's validators attests, and nothing
		// at all is leased.
		if slices.Contains(gate, gateBeacon) || slices.Contains(gate, gateLiveness) {
			return fmt.Errorf("the network gate is closed for want of data: %v", gate)
		}

		if host.weight == 0 || host.live != 0 {
			return fmt.Errorf("the network gate is closed (%v) and %d of the host's %d validators attest",
				gate, host.live, host.weight)
		}

		if n := oracleCount(leased, protocol.ImpactNode, protocol.ImpactSlot); n > 0 {
			return fmt.Errorf("the network gate is closed (%v) and %d hosts are leased already", gate, n)
		}

		o.exceptions++
	}

	return oracleBudget(f, leased, l.Host, bound)
}

// checkRollout checks that a rollout may be granted in: it is open, not
// halted, not paused and approved where its policy asks for that, and the
// host is its member under this lease.
func (o *oracle) checkRollout(f *simFacts, r *fleet.Rollout, l *fleet.Lease) error {
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

	m := r.Host(l.Host)

	switch {
	case r.State != fleet.RolloutActive:
		return fmt.Errorf("the rollout is %s", r.State)
	case o.halted[r.ID] != "":
		return fmt.Errorf("lease %s failed in it and nobody retried it", o.halted[r.ID])
	case r.Paused:
		return errors.New("the rollout is paused")
	case policy == fleet.PolicyManual && !r.Approved:
		return errors.New("the policy is manual and nobody approved the rollout")
	case m == nil || m.State != fleet.HostLeased || m.LeaseID != l.ID:
		return errors.New("the host is not a member leased by this lease")
	default:
		return nil
	}
}
