package scheduler

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// budgetEpsilon absorbs floating point noise in percent arithmetic.
const budgetEpsilon = 1e-9

// Waiting reasons, in the order of docs/scheduler.md.
const (
	reasonHeld          = "held"
	reasonPaused        = "paused"
	reasonRolloutPaused = "rollout paused"
	reasonApproval      = "waiting for approval (policy manual)"
	reasonPolicyPaused  = "policy paused"
	reasonHaltedPrefix  = "rollout halted: "
	reasonVerb          = "verb not allowed by the agent"
	reasonGatePrefix    = "network gate: "
	reasonAfter         = "after "
	reasonAfterCanary   = "after canary "
	reasonAfterRepair   = "after repair of "
	reasonNext          = "next"
)

// Gate reasons.
const (
	gateBeacon   = "beacon node unreachable"
	gateLiveness = "liveness data stale"
	// gateTimingPrefix starts the reason of a gate that is closed because the
	// beacon node's chain timing cannot place the current epoch; the beacon
	// service's reason follows.
	gateTimingPrefix = "chain timing out of bounds: "
)

// Ranks of the waiting reasons. A higher rank is closer to being granted.
const (
	rankGrantable = iota
	rankHeld
	rankSilent
	rankPaused
	rankRolloutPaused
	rankApproval
	rankPolicyPaused
	rankHalted
	rankVerb
	rankGate
	rankOrder
	rankBudget
	rankHostLimit
	rankNext
)

// waitKinds maps a verdict's rank to the wait kind served in the plan.
var waitKinds = map[int]WaitKind{
	rankHeld:          WaitHeld,
	rankSilent:        WaitSilent,
	rankPaused:        WaitPaused,
	rankRolloutPaused: WaitRolloutPaused,
	rankApproval:      WaitApproval,
	rankPolicyPaused:  WaitPolicyPaused,
	rankHalted:        WaitHalted,
	rankVerb:          WaitVerb,
	rankGate:          WaitGate,
	rankOrder:         WaitOrder,
	rankBudget:        WaitBudget,
	rankHostLimit:     WaitHostLimit,
	rankNext:          WaitNext,
}

// verdict is why one host waits (rankGrantable: it does not).
type verdict struct {
	rank   int
	reason string
	// after names the host an order wait is for.
	after string
}

// budgetState is the availability budget while grants are being made.
type budgetState struct {
	total       uint64
	allowed     float64
	unavailable uint64
	leased      int
	maxLeased   int
	percent     float64
}

// budget computes the budget from the hosts' leases and liveness (or, when
// the beacon node does not serve liveness, from the agents' probes: see
// probeLive).
func (p *pass) budget() *budgetState {
	b := &budgetState{percent: p.set.MaxUnavailablePercent}

	for _, hv := range p.hosts {
		b.total += hv.weight

		if hv.lease != nil {
			b.unavailable += hv.weight
			b.leased++

			continue
		}

		b.unavailable += hv.weight - hv.live
	}

	b.allowed = b.percent * float64(b.total) / 100
	b.maxLeased = max(1, int(math.Floor(b.percent*float64(len(p.hosts))/100+budgetEpsilon)))

	return b
}

// fits checks the three grant conditions for a host that costs cost: it
// costs nothing, it stays inside the budget, or it goes alone.
func (b *budgetState) fits(cost uint64) bool {
	return cost == 0 ||
		float64(b.unavailable+cost) <= b.allowed+budgetEpsilon ||
		(b.unavailable == 0 && b.allowed > 0 && b.leased == 0)
}

// admit checks the three grant conditions and the host-count limit for a
// host that costs cost.
func (b *budgetState) admit(cost uint64) verdict {
	if !b.fits(cost) {
		used := 0.0
		if b.total > 0 {
			used = float64(b.unavailable) * 100 / float64(b.total)
		}

		return verdict{rank: rankBudget, reason: "budget " + percent(used) + " of " + percent(b.percent)}
	}

	if b.leased >= b.maxLeased {
		return verdict{rank: rankHostLimit, reason: fmt.Sprintf("host limit %d of %d", b.leased, b.maxLeased)}
	}

	return verdict{}
}

// take books a granted host.
func (b *budgetState) take(cost uint64) {
	b.unavailable += cost
	b.leased++
}

// percent formats a percentage with at most one decimal, rounded down.
func percent(v float64) string {
	return strconv.FormatFloat(math.Floor(v*10+budgetEpsilon)/10, 'f', -1, 64) + "%"
}

// gate evaluates the network gate.
func (p *pass) gate() Gate {
	g := Gate{Network: p.w.Health, Reasons: []string{}}
	n := &p.w.Health
	epoch := n.EpochDuration()

	switch {
	case n.TimingError != "":
		// The node answers, but what it says about slots and epochs cannot be
		// worked with: that is not a node that is unreachable.
		g.Reasons = append(g.Reasons, gateTimingPrefix+n.TimingError)
	case n.UpdatedAt.IsZero() || epoch <= 0 || p.now.Sub(n.UpdatedAt) > 2*epoch:
		g.Reasons = append(g.Reasons, gateBeacon)
	}

	if n.FinalityLagEpochs > p.set.MaxFinalityLagEpochs {
		g.Reasons = append(g.Reasons, fmt.Sprintf("finality lag %d epochs > %d",
			n.FinalityLagEpochs, p.set.MaxFinalityLagEpochs))
	}

	var tracked, live, total uint64

	for _, hv := range p.hosts {
		tracked += hv.weight

		// A lease takes out of the gate what it took offline: the validators
		// that were live when it was granted are left out on both sides, so a
		// rollout does not close the gate on itself. Those that were offline
		// already stay in, as not live: a lease must not hide them, least of
		// all a repair, which would open the gate for everybody else.
		if hv.lease == nil {
			total += hv.weight
			live += hv.live
		} else {
			total += hv.weight - min(hv.lease.Charged, hv.weight)
		}
	}

	switch {
	case tracked == 0:
	case n.LivenessUnsupported:
		// The beacon node does not serve liveness: there is no participation
		// to judge. Finality and the freshness of the view still gate, and
		// the budget counts unavailability from the agents' probes.
	case !n.LivenessKnown || !p.haveRows || n.CurrentEpoch > p.newestEpoch+2:
		g.Reasons = append(g.Reasons, gateLiveness)
	case total > 0:
		if share := float64(live) * 100 / float64(total); share < p.set.MinParticipationPercent {
			g.Reasons = append(g.Reasons, "participation "+percent(share)+" < "+
				percent(p.set.MinParticipationPercent))
		}
	}

	g.OK = len(g.Reasons) == 0

	return g
}

// policyVerdict is the pause, approval, policy and halt part of the waiting
// reasons of one rollout.
func (p *pass) policyVerdict(r *fleet.Rollout) verdict {
	policy := fleet.PolicyAuto

	switch r.Kind {
	case fleet.RolloutImage:
		policy = p.set.PolicyImage
	case fleet.RolloutPackages:
		policy = p.set.PolicyPackages
	case fleet.RolloutReboot:
		policy = p.set.PolicyReboot
	default:
	}

	switch {
	case r.Paused:
		return verdict{rank: rankRolloutPaused, reason: reasonRolloutPaused}
	case policy == fleet.PolicyManual && !r.Approved:
		return verdict{rank: rankApproval, reason: reasonApproval}
	case policy == fleet.PolicyPaused:
		return verdict{rank: rankPolicyPaused, reason: reasonPolicyPaused}
	case r.State != fleet.RolloutActive:
		return verdict{rank: rankHalted, reason: reasonHaltedPrefix + r.Reason}
	default:
		return verdict{}
	}
}

// orderVerdict applies the order inside one rollout to one host. A host that
// is broken for the rollout's group waits for nobody. Every other host waits
// for each member whose host is broken and that is still to be served, for a
// lease that is in flight for the rollout, and, until the rollout has its
// first success, for the canary.
func (p *pass) orderVerdict(r *fleet.Rollout, hv *hostView) verdict {
	if m := r.Host(hv.h.Name); m == nil || p.broken(r, m.Host) {
		return verdict{}
	}

	if m := p.brokenAwaited(r); m != nil {
		return verdict{rank: rankOrder, reason: reasonAfter + m.Host, after: m.Host}
	}

	// A lease of a closed rollout of the group that is still open is the
	// canary in flight: no second host goes down before it has ended.
	if host := p.inFlight(r); host != "" {
		return verdict{rank: rankOrder, reason: reasonAfter + host, after: host}
	}

	if r.Canary == "" || r.Canary == hv.h.Name || p.succeeded(r) {
		return verdict{}
	}

	return verdict{rank: rankOrder, reason: reasonAfterCanary + r.Canary, after: r.Canary}
}

// part is the share of one rollout in a lease.
type part struct {
	r     *fleet.Rollout
	needs []protocol.Need
}

// granter makes the grants of one pass.
type granter struct {
	p      *pass
	gate   Gate
	budget *budgetState
	// forced names the force rollouts that were granted a host in this
	// pass; they get one host per reconcile.
	forced map[string]bool
	// repairHost is the host of a lease that was granted through the repair
	// exception of the gate and was open when the pass began, or was granted
	// in it. While it is set no further repair is granted: each repair has
	// ended, however it ended, a reconcile before the next begins.
	repairHost string
}

// evaluate decides whether the host's needs in one rollout are grantable
// now, the budget aside. repair says that they are only because the closed
// gate lets the repair of this host pass.
func (g *granter) evaluate(r *fleet.Rollout, hv *hostView) (v verdict, granted []protocol.Need, repair bool) {
	if v = g.p.policyVerdict(r); v.rank != rankGrantable {
		return v, nil, false
	}

	reported := g.p.needs[r.ID][hv.h.Name]
	needs := make([]protocol.Need, 0, len(reported))

	for _, n := range reported {
		if hv.allows(n.Kind.Verb()) {
			needs = append(needs, n)
		}
	}

	if len(needs) == 0 {
		return verdict{rank: rankVerb, reason: reasonVerb}, nil, false
	}

	if !r.IgnoreBudget && !g.gate.OK {
		switch {
		case !g.repairable(hv):
			return verdict{rank: rankGate, reason: reasonGatePrefix + g.gate.Reasons[0]}, nil, false
		case g.repairHost != "":
			// Its repair would pass the gate, but one repair at a time.
			return verdict{rank: rankOrder, reason: reasonAfterRepair + g.repairHost, after: g.repairHost}, nil, false
		}

		repair = true
	}

	switch {
	case r.Force && g.forced[r.ID]:
		return verdict{rank: rankNext, reason: reasonNext}, nil, false
	case r.Force, repair:
		// A forced rollout has no order, and a repair does not wait for its
		// place in one.
		return verdict{}, needs, repair
	}

	if v = g.p.orderVerdict(r, hv); v.rank != rankGrantable {
		return v, nil, false
	}

	return verdict{}, needs, false
}

// repairable reports whether the repair exception of a closed gate applies
// to the host, the one-at-a-time rule aside. The host has validators and none of them attests, so
// serving it cannot make the network worse and may be what brings it back;
// without the exception a host that keeps participation below the gate would
// keep its own repair away for ever. The exception is bounded:
//
//   - it needs data it can trust: a gate that is closed because the beacon
//     view, its chain timing or the liveness data is missing passes nobody;
//   - one at a time: nobody passes while a lease that was granted through the
//     exception is open, in whatever rollout, nor in the pass in which such a
//     lease ended (repairHost; the caller checks it);
//   - the host must be established as offline (see offline).
func (g *granter) repairable(hv *hostView) bool {
	if slices.ContainsFunc(g.gate.Reasons, func(reason string) bool {
		return reason == gateBeacon || reason == gateLiveness || strings.HasPrefix(reason, gateTimingPrefix)
	}) {
		return false
	}

	return g.p.offline(hv)
}

// offline reports whether the host is established as offline: it has
// validators, holds no lease, and none of its validators attested in the two
// newest stored liveness epochs, both of which began (strictly) after the
// host's last lease ended and after this database first saw the host. One epoch without
// attestations can be a glitch of the whole network, and a host that has just
// been rolled or rebooted (by any rollout, successfully or not) is still on
// its way back: neither is a host to repair.
func (p *pass) offline(hv *hostView) bool {
	if hv.weight == 0 || hv.lease != nil || p.w.Health.LivenessUnsupported || !p.haveRows || p.newestEpoch == 0 {
		return false
	}

	// An epoch that cannot be placed in time establishes nothing.
	began, placed := p.w.Health.EpochStart(p.newestEpoch - 1)
	if !placed {
		return false
	}

	byEpoch := p.rows[hv.h.Name]

	for _, e := range []uint64{p.newestEpoch, p.newestEpoch - 1} {
		if row, ok := byEpoch[e]; !ok || row.Live != 0 {
			return false
		}
	}

	return began.After(later(p.lastLeaseEnd(hv.h.Name), hv.h.FirstSeen))
}

// consider grants the host a lease for everything grantable, or says what
// it waits for. When the wait is about one rollout (its pause, approval,
// policy, halt, order, gate or verb), that rollout's ID is returned too; a
// hold, the global pause and the budget are about none.
func (g *granter) consider(hv *hostView, rollouts []*fleet.Rollout) (v verdict, about string) {
	switch {
	case hv.held:
		return verdict{rank: rankHeld, reason: reasonHeld}, ""
	case hv.stale:
		return verdict{rank: rankSilent, reason: reasonAgentSilent}, ""
	case g.p.set.Paused:
		return verdict{rank: rankPaused, reason: reasonPaused}, ""
	}

	var (
		best           verdict
		normal, exempt []part
		repair         bool
	)

	for _, r := range rollouts {
		rv, needs, viaRepair := g.evaluate(r, hv)
		repair = repair || viaRepair

		switch {
		case rv.rank != rankGrantable:
			if rv.rank > best.rank {
				best, about = rv, r.ID
			}
		case r.IgnoreBudget:
			exempt = append(exempt, part{r, needs})
		default:
			normal = append(normal, part{r, needs})
		}
	}

	if len(normal) > 0 {
		if bv := g.budget.admit(hv.live); bv.rank != rankGrantable {
			best, about, normal, repair = bv, "", nil, false
		}
	}

	parts := slices.Concat(exempt, normal)
	if len(parts) == 0 {
		return best, about
	}

	g.lease(hv, parts, repair)

	return verdict{}, ""
}

// lease creates the lease for one host and books it. repair: the grant
// passed a closed gate through the repair exception.
func (g *granter) lease(hv *hostView, parts []part, repair bool) {
	p := g.p
	host := hv.h.Name

	l := &fleet.Lease{
		ID:        p.w.NewID(),
		Host:      host,
		State:     fleet.LeaseGranted,
		Rollouts:  make(map[string]string, len(parts)),
		Weight:    hv.weight,
		Charged:   hv.live,
		Force:     true,
		Repair:    repair,
		GrantedBy: parts[0].r.CreatedBy,
		GrantedAt: p.now,
		ExpiresAt: p.now.Add(p.set.LeaseTTL.Std()),
	}

	ids := make([]string, 0, len(parts))
	ignoreBudget := false

	for _, pt := range parts {
		r := pt.r
		m := r.Host(host)

		for _, n := range pt.needs {
			l.Needs = append(l.Needs, n)
			l.Rollouts[n.ID] = r.ID
		}

		m.LeaseID = l.ID
		m.NeedID = pt.needs[0].ID
		p.setMember(m, fleet.HostLeased)

		if r.Force {
			g.forced[r.ID] = true
		}

		l.Force = l.Force && r.Force
		ignoreBudget = ignoreBudget || r.IgnoreBudget
		ids = append(ids, r.ID)

		if r.Kind != fleet.RolloutManual || r.CreatedBy != l.GrantedBy {
			l.GrantedBy = actorScheduler
		}
	}

	g.budget.take(hv.live)

	if repair {
		g.repairHost = host
	}

	hv.lease = l
	p.leases = append(p.leases, l)

	p.record("lease.granted", host, map[string]any{
		keyLease:       l.ID,
		keyRollouts:    ids,
		"cost":         l.Charged,
		"unavailable":  g.budget.unavailable,
		"allowed":      g.budget.allowed,
		keyForce:       l.Force,
		"ignoreBudget": ignoreBudget,
		"repair":       l.Repair,
	})
}

// waiting is one host that waits in an open rollout.
type waiting struct {
	hv       *hostView
	rollouts []*fleet.Rollout
}

// servingOrder lists the hosts that wait in an open rollout and hold no
// lease: oldest rollout first, inside a rollout by stage (broken hosts,
// the canary, the rest), then ascending weight and name.
func (p *pass) servingOrder() []*waiting {
	var (
		order  []*waiting
		byHost = make(map[string]*waiting, len(p.hosts))
	)

	sortRollouts(p.rollouts)

	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		members := make([]*fleet.RolloutHost, 0, len(r.Hosts))

		for i := range r.Hosts {
			m := &r.Hosts[i]
			if hv := p.hosts[m.Host]; m.State == fleet.HostPending && hv != nil && hv.lease == nil {
				members = append(members, m)
			}
		}

		slices.SortFunc(members, func(a, b *fleet.RolloutHost) int {
			ha, hb := p.hosts[a.Host], p.hosts[b.Host]

			return cmp.Or(
				cmp.Compare(a.Stage, b.Stage),
				cmp.Compare(ha.weight, hb.weight),
				cmp.Compare(a.Host, b.Host),
			)
		})

		for _, m := range members {
			w := byHost[m.Host]
			if w == nil {
				w = &waiting{hv: p.hosts[m.Host]}
				byHost[m.Host] = w
				order = append(order, w)
			}

			w.rollouts = append(w.rollouts, r)
		}
	}

	return order
}

// grant makes the grants of this pass and builds the plan.
func (p *pass) grant() *Plan {
	g := &granter{
		p:      p,
		gate:   p.gate(),
		budget: p.budget(),
		forced: make(map[string]bool, 2),
	}

	// Every lease the pass knows was open when it began.
	for _, l := range p.leases {
		if l.Repair {
			g.repairHost = l.Host

			break
		}
	}

	plan := &Plan{
		ComputedAt: p.now,
		Paused:     p.set.Paused,
		Gate:       g.gate,
		Queue:      []QueueEntry{},
		Hosts:      make(map[string]HostPlan, len(p.hosts)),
	}

	reasons := make(map[string]string, len(p.hosts))

	for _, w := range p.servingOrder() {
		v, about := g.consider(w.hv, w.rollouts)
		if v.rank == rankGrantable {
			continue
		}

		ids := make([]string, 0, len(w.rollouts))
		for _, r := range w.rollouts {
			ids = append(ids, r.ID)
		}

		reasons[w.hv.h.Name] = v.reason
		plan.Queue = append(plan.Queue, QueueEntry{
			Host:     w.hv.h.Name,
			Position: len(plan.Queue) + 1,
			Reason:   v.reason,
			Wait:     waitKinds[v.rank],
			After:    v.after,
			Rollout:  about,
			Rollouts: ids,
		})
	}

	plan.Budget = Budget{
		TotalWeight:       g.budget.total,
		AllowedWeight:     g.budget.allowed,
		UnavailableWeight: g.budget.unavailable,
		LeasedHosts:       g.budget.leased,
		MaxLeasedHosts:    g.budget.maxLeased,
	}

	failed := make(map[string]bool, len(p.hosts))

	for _, r := range p.rollouts {
		for i := range r.Hosts {
			if !r.State.Terminal() && r.Hosts[i].State == fleet.HostFailed {
				failed[r.Hosts[i].Host] = true
			}
		}
	}

	for name, hv := range p.hosts {
		hp := HostPlan{Slot: SlotIdle, Weight: hv.weight, Live: hv.live, LiveKnown: hv.liveKnown}
		reason, queued := reasons[name]

		switch {
		case hv.lease != nil:
			hp.LeaseID = hv.lease.ID
			hp.Slot = SlotLeased

			if hv.lease.State == fleet.LeaseVerifying {
				hp.Slot = SlotVerifying
			}
		case hv.held:
			hp.Slot = SlotHeld
		case hv.stale:
			hp.Slot = SlotStale
		case failed[name]:
			hp.Slot = SlotFailed
		case queued:
			hp.Slot = SlotQueued
		}

		if hv.lease == nil {
			hp.Reason = reason
		}

		plan.Hosts[name] = hp
	}

	return plan
}
