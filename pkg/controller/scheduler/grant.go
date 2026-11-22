package scheduler

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// budgetEpsilon absorbs floating point noise in percent arithmetic.
const budgetEpsilon = 1e-9

// Waiting reasons.
const (
	reasonHeld    = "held"
	reasonOffline = "known offline"
	// reasonWorking is why a host that works on an operation without a lease
	// is granted nothing.
	reasonWorking       = "working on an operation without a lease"
	reasonPaused        = "paused"
	reasonRolloutPaused = "rollout paused"
	// reasonResume ends the pause reasons that say why a rollout is paused.
	reasonResume = "; resume to continue"
	// reasonAborted starts the pause reason of a reboot or local rollout
	// that a person aborted (abortedReason).
	reasonAborted = "aborted"
	// reasonCanaryPause is the pause reason of a rollout that the scheduler
	// paused when it had its first success (pauseAfterCanary).
	reasonCanaryPause  = "paused after the canary" + reasonResume
	reasonApproval     = "waiting for approval (policy manual)"
	reasonHaltedPrefix = "rollout halted: "
	reasonVerb         = "verb not allowed by the agent"
	reasonPull         = "waiting to pull its image again after a failed pull"
	reasonGatePrefix   = "network gate: "
	reasonAfter        = "after "
	reasonAfterCanary  = "after canary "
)

// Gate reasons.
const (
	gateBeacon = "beacon node unreachable"
	// gateLiveness closes the gate while the beacon service has no settled
	// epoch of attestation data recent enough; its reason follows when it
	// gave one.
	gateLiveness = "attestation data missing or stale"
	// gateTimingPrefix starts the reason of a gate that is closed because the
	// beacon node's chain timing is invalid; the beacon service's reason
	// follows.
	gateTimingPrefix = "invalid chain timing: "
)

// Ranks of the waiting reasons. A higher rank is closer to being granted.
const (
	rankGrantable = iota
	rankHeld
	rankSilent
	rankWorking
	rankPaused
	rankRolloutPaused
	rankApproval
	rankHalted
	rankVerb
	rankPull
	rankGate
	rankOrder
	rankBudget
	rankHostLimit
)

// waitKinds maps a verdict's rank to the wait kind served in the plan.
var waitKinds = map[int]WaitKind{
	rankHeld:          WaitHeld,
	rankSilent:        WaitSilent,
	rankWorking:       WaitWorking,
	rankPaused:        WaitPaused,
	rankRolloutPaused: WaitRolloutPaused,
	rankApproval:      WaitApproval,
	rankHalted:        WaitHalted,
	rankVerb:          WaitVerb,
	rankPull:          WaitPull,
	rankGate:          WaitGate,
	rankOrder:         WaitOrder,
	rankBudget:        WaitBudget,
	rankHostLimit:     WaitHostLimit,
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
	// independent counts the hosts whose lease is about independent
	// containers alone; maxIndependent is their limit.
	independent    int
	maxIndependent int
	// offlineHosts and offlineWeight are the hosts marked known offline
	// that are not at work, left out of total and unavailable.
	offlineHosts  int
	offlineWeight uint64
}

// budget computes the budget from the hosts' leases and liveness. It counts
// the validators with a duty (hostView.duty): validators without one can
// neither go down nor widen what may. A host that works on an operation
// without a lease, and a charged host, count like one that holds a node
// lease. A node lease takes the host's validators out of the budget and a
// place in the host limit; an independent one takes a place in the
// independent limit.
func (p *pass) budget() *budgetState {
	b := &budgetState{percent: p.set.MaxUnavailablePercent}

	for _, hv := range p.hosts {
		if hv.offline && !hv.occupied() {
			b.offlineHosts++
			b.offlineWeight += hv.duty

			continue
		}

		b.total += hv.duty
		impact := hv.occupation()

		if impact == protocol.ImpactIndependent {
			b.independent++
		}

		// A charged host is down, whatever lighter work it is leased for.
		if hv.charged() {
			impact = protocol.ImpactNode
		}

		if impact == protocol.ImpactNode {
			b.unavailable += hv.duty
			b.leased++

			continue
		}

		b.unavailable += hv.duty - hv.live
	}

	b.allowed = b.percent * float64(b.total) / 100
	b.maxLeased = hostLimit(p.set.MaxLeasedPercent, len(p.hosts))
	b.maxIndependent = hostLimit(p.set.MaxIndependentPercent, len(p.hosts))

	return b
}

// pullFailed reports whether the last lease could not pull the image of any
// of the needs.
func pullFailed(needs []protocol.Need) bool {
	return slices.ContainsFunc(needs, func(n protocol.Need) bool { return n.PullError != "" })
}

// hostLimit is a share of the hosts, at least one.
func hostLimit(percent float64, hosts int) int {
	return max(1, int(math.Floor(percent*float64(hosts)/100+budgetEpsilon)))
}

// admitIndependent checks the independent limit for a host whose lease is
// about independent containers alone.
func (b *budgetState) admitIndependent() verdict {
	if b.independent >= b.maxIndependent {
		return verdict{rank: rankHostLimit, reason: fmt.Sprintf("independent limit %d of %d",
			b.independent, b.maxIndependent)}
	}

	return verdict{}
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

// take books a granted host by the class of its lease.
func (b *budgetState) take(cost uint64, impact protocol.Impact) {
	b.unavailable += cost

	if impact == protocol.ImpactIndependent {
		b.independent++

		return
	}

	b.leased++
}

// percent formats a percentage with at most one decimal, rounded down.
func percent(v float64) string {
	return strconv.FormatFloat(math.Floor(v*10+budgetEpsilon)/10, 'f', -1, 64) + "%"
}

// fallbackEpoch is the epoch length the age of the beacon view is measured in
// while the view's own timing places no epoch: 32 slots of 12 s.
const fallbackEpoch = 32 * 12 * time.Second

// viewStaleAfter is how old the beacon view may be before the node counts as
// unreachable: two epochs.
func viewStaleAfter(n *fleet.NetworkHealth) time.Duration {
	epoch := n.EpochDuration()
	if epoch <= 0 {
		epoch = fallbackEpoch
	}

	return 2 * epoch
}

// gate evaluates the network gate. trusted says that the gate has the data it
// judges by: it is open, or closed for what the network does (finality,
// participation) and not because the beacon view, its timing or the liveness
// data is missing or stale.
func (p *pass) gate() (g Gate, trusted bool) {
	g = Gate{Network: p.w.Health, Reasons: []string{}}
	n := &p.w.Health
	epoch := n.EpochDuration()
	trusted = true

	switch {
	case n.UpdatedAt.IsZero() || p.now.Sub(n.UpdatedAt) > viewStaleAfter(n):
		// A node that does not answer is unreachable, whatever it said about
		// its timing while it still answered.
		g.Reasons = append(g.Reasons, gateBeacon)
		trusted = false
	case n.TimingError != "":
		// The node answers, but what it says about slots and epochs cannot be
		// worked with: that is not a node that is unreachable.
		g.Reasons = append(g.Reasons, gateTimingPrefix+n.TimingError)
		trusted = false
	case epoch <= 0:
		g.Reasons = append(g.Reasons, gateBeacon)
		trusted = false
	}

	if n.FinalityLagEpochs > p.set.MaxFinalityLagEpochs {
		g.Reasons = append(g.Reasons, fmt.Sprintf("finality lag %d epochs > %d",
			n.FinalityLagEpochs, p.set.MaxFinalityLagEpochs))
	}

	// Participation is judged among the hosts that are not occupied: a host
	// that holds a lease, works without one or is charged is down because of
	// an operation, so a rollout does not close the gate on itself. It is the
	// share of the validators with a duty that attested.
	var tracked, live, total uint64

	for _, hv := range p.hosts {
		tracked += hv.weight

		if !hv.occupied() && !hv.offline {
			total += hv.duty
			live += hv.live
		}
	}

	switch {
	case tracked == 0:
	case !n.LivenessKnown || !p.haveRows || n.CurrentEpoch > p.newestEpoch+2:
		reason := gateLiveness
		if n.LivenessError != "" {
			reason += ": " + n.LivenessError
		}

		g.Reasons = append(g.Reasons, reason)
		trusted = false
	case total > 0:
		if share := float64(live) * 100 / float64(total); share < p.set.MinParticipationPercent {
			g.Reasons = append(g.Reasons, "participation "+percent(share)+" < "+
				percent(p.set.MinParticipationPercent))
		}
	}

	g.OK = len(g.Reasons) == 0

	return g, trusted
}

// policyVerdict is the pause, approval and halt part of the waiting reasons
// of one rollout.
func (p *pass) policyVerdict(r *fleet.Rollout) verdict {
	switch {
	case r.Paused && r.State == fleet.RolloutActive:
		// A rollout that is halted says so, paused or not: the failure is what
		// a person must look at first. Its pause shows once it is retried.
		return verdict{rank: rankRolloutPaused, reason: cmpOr(r.PauseReason, reasonRolloutPaused)}
	case rolloutPolicy(p.set, r) == fleet.PolicyManual && !r.Approved:
		return verdict{rank: rankApproval, reason: reasonApproval}
	case r.State != fleet.RolloutActive:
		return verdict{rank: rankHalted, reason: reasonHaltedPrefix + r.Reason}
	default:
		return verdict{}
	}
}

// nodeCost is what a lease of the class costs the budget: the host's live
// validators for node work, nothing for lighter work.
func nodeCost(hv *hostView, impact protocol.Impact) uint64 {
	if impact == protocol.ImpactNode {
		return hv.live
	}

	return 0
}

// part is the share of one rollout in a lease.
type part struct {
	r     *fleet.Rollout
	needs []protocol.Need
}

// granter makes the grants of one pass.
type granter struct {
	p    *pass
	gate Gate
	// gateTrusted: the gate judges by data it has (pass.gate).
	gateTrusted bool
	budget      *budgetState
	// order holds the order of each rollout a host was considered in.
	order map[string]*rolloutOrder
}

// rolloutOrder is what the order inside one rollout rests on while grants are
// made: neither changes with a grant, so each is found once per pass.
type rolloutOrder struct {
	// broken is the first member whose host is broken and that is still to be
	// served, or nil.
	broken *fleet.RolloutHost
	// succeeded: the rollout has had its first success.
	succeeded bool
}

// orderVerdict applies the order inside one rollout to one of its members. A
// host that is broken for the rollout's group waits for nobody. Every other
// host waits for each member whose host is broken and that is still to be
// served, and, until the rollout has its first success, for the canary.
func (g *granter) orderVerdict(r *fleet.Rollout, hv *hostView) verdict {
	if g.p.broken(r, hv.h.Name) {
		return verdict{}
	}

	order := g.order[r.ID]
	if order == nil {
		order = &rolloutOrder{broken: g.p.brokenAwaited(r), succeeded: g.p.succeeded(r)}
		g.order[r.ID] = order
	}

	if order.broken != nil {
		return verdict{rank: rankOrder, reason: reasonAfter + order.broken.Host, after: order.broken.Host}
	}

	if r.Canary == "" || r.Canary == hv.h.Name || order.succeeded {
		return verdict{}
	}

	return verdict{rank: rankOrder, reason: reasonAfterCanary + r.Canary, after: r.Canary}
}

// evaluate decides whether the host's needs in one rollout are grantable
// now, the budget aside. excepted says that they are only because the closed
// gate lets this host pass.
func (g *granter) evaluate(r *fleet.Rollout, hv *hostView) (v verdict, granted []protocol.Need, excepted bool) {
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

	// A lease pulls its images first. After a failed pull the agent marks
	// the need until the pull may be tried again; a group waits for all of
	// its needs.
	if pullFailed(needs) {
		return verdict{rank: rankPull, reason: reasonPull}, nil, false
	}

	// Independent containers touch neither the node nor the network: the
	// gate does not hold them back.
	if !r.IgnoreBudget && !g.gate.OK && needsImpact(hv.h, needs) != protocol.ImpactIndependent {
		if !g.passesClosedGate(hv) {
			return verdict{rank: rankGate, reason: reasonGatePrefix + g.gate.Reasons[0]}, nil, false
		}

		excepted = true
	}

	// A forced rollout has no order, and a host that passes the closed gate
	// does not wait for its place in one: the members before it cannot be
	// granted while the gate is closed.
	if r.Force || excepted {
		return verdict{}, needs, excepted
	}

	// A need that waits for its place in the order is not granted, whatever
	// else the host is leased for: the canary and the broken hosts prove the
	// change first.
	if v = g.orderVerdict(r, hv); v.rank != rankGrantable {
		return v, nil, false
	}

	return verdict{}, needs, false
}

// passesClosedGate is the one exception of the network gate. While the gate
// is closed for what the network does (finality, participation; not for
// missing or stale data), a host with validators of which none attested in
// the newest liveness epoch may be granted when nothing at all is leased:
// serving it cannot make the network worse and may be what brings it back.
// Without the exception a host that keeps participation below the gate would
// keep its own fix away for ever. Nothing is leased next to it, so one host
// goes at a time. A host none of whose validators has a duty does not pass:
// it keeps nobody's participation down.
func (g *granter) passesClosedGate(hv *hostView) bool {
	return g.gateTrusted && g.admitting(hv).leased == 0 && hv.duty > 0 && hv.liveKnown && hv.live == 0
}

// admitting is the budget a grant to the host is checked against. A charged
// host already counts with its full weight and a place in the host limit:
// it is checked as if it were not charged, so that the charge does not keep
// its own repair away, and its lease charges it as usual.
func (g *granter) admitting(hv *hostView) *budgetState {
	if !hv.charged() {
		return g.budget
	}

	b := *g.budget
	b.unavailable -= hv.live
	b.leased--

	return &b
}

// consider grants the host a lease for everything grantable, or says what
// it waits for. When the wait is about one rollout (its pause, approval,
// halt, order, gate or verb), that rollout's ID is returned too; a hold, the
// global pause and the budget are about none.
func (g *granter) consider(hv *hostView, rollouts []*fleet.Rollout) (v verdict, about string) {
	switch {
	case hv.offline:
		return verdict{rank: rankHeld, reason: reasonOffline}, ""
	case hv.held:
		return verdict{rank: rankHeld, reason: reasonHeld}, ""
	case hv.stale:
		return verdict{rank: rankSilent, reason: reasonAgentSilent}, ""
	case hv.working():
		return verdict{rank: rankWorking, reason: reasonWorking}, ""
	case g.p.set.Paused:
		return verdict{rank: rankPaused, reason: reasonPaused}, ""
	}

	var (
		best                  verdict
		normal, exempt, indep []part
		excepted              bool
		impact                = protocol.ImpactIndependent
	)

	for _, r := range rollouts {
		rv, needs, viaException := g.evaluate(r, hv)
		excepted = excepted || viaException

		switch {
		case rv.rank != rankGrantable:
			if rv.rank > best.rank {
				best, about = rv, r.ID
			}
		case r.IgnoreBudget:
			exempt = append(exempt, part{r, needs})
		case needsImpact(hv.h, needs) == protocol.ImpactIndependent:
			indep = append(indep, part{r, needs})
		default:
			normal = append(normal, part{r, needs})
			impact = impact.Heavier(needsImpact(hv.h, needs))
		}
	}

	if len(normal) > 0 {
		if bv := g.admitting(hv).admit(nodeCost(hv, impact)); bv.rank != rankGrantable {
			best, about, normal, excepted = bv, "", nil, false
		}
	}

	// Work on independent containers alone has a limit of its own; next to
	// other work it rides along with the lease that takes the host anyway.
	if len(indep) > 0 && len(normal) == 0 && len(exempt) == 0 {
		if bv := g.budget.admitIndependent(); bv.rank != rankGrantable {
			if bv.rank > best.rank {
				best, about = bv, ""
			}

			indep = nil
		}
	}

	parts := slices.Concat(exempt, normal, indep)
	if len(parts) == 0 {
		return best, about
	}

	g.lease(hv, parts, excepted)

	return verdict{}, ""
}

// lease creates the lease for one host and books it. excepted: the grant
// passed the closed gate through its exception.
func (g *granter) lease(hv *hostView, parts []part, excepted bool) {
	p := g.p
	host := hv.h.Name

	l := &fleet.Lease{
		ID:        p.w.NewID(),
		Host:      host,
		State:     fleet.LeaseGranted,
		Rollouts:  make(map[string]string, len(parts)),
		Weight:    hv.weight,
		Force:     true,
		GrantedBy: parts[0].r.CreatedBy,
		GrantedAt: p.now,
		ExpiresAt: p.now.Add(p.set.LeaseTTL.Std()),
	}

	ids := make([]string, 0, len(parts))
	ignoreBudget := false
	impact := protocol.ImpactIndependent

	for _, pt := range parts {
		impact = impact.Heavier(needsImpact(hv.h, pt.needs))
	}

	l.Impact = impact
	l.Charged = nodeCost(hv, impact)

	for _, pt := range parts {
		r := pt.r
		m := p.member(r, host)

		for _, n := range pt.needs {
			l.Needs = append(l.Needs, n)
			l.Rollouts[n.ID] = r.ID
		}

		m.LeaseID = l.ID
		m.NeedID = pt.needs[0].ID
		p.setMember(m, fleet.HostLeased)

		l.Force = l.Force && r.Force
		ignoreBudget = ignoreBudget || r.IgnoreBudget
		ids = append(ids, r.ID)

		if !directed(r) || r.CreatedBy != l.GrantedBy {
			l.GrantedBy = actorScheduler
		}
	}

	// A charged host counts as node work already, and a lease does not
	// change that; only its place in the independent limit is new.
	switch {
	case !hv.charged():
		g.budget.take(l.Charged, impact)
	case impact == protocol.ImpactIndependent:
		g.budget.independent++
	default:
	}

	hv.lease = l
	p.leases = append(p.leases, l)

	p.record("lease.granted", host, map[string]any{
		keyLease:       l.ID,
		keyRollouts:    ids,
		"cost":         l.Charged,
		"impact":       l.Impact,
		"unavailable":  g.budget.unavailable,
		"allowed":      g.budget.allowed,
		keyForce:       l.Force,
		"ignoreBudget": ignoreBudget,
		"closedGate":   excepted,
	})
}

// waiting is one host that waits in an open rollout.
type waiting struct {
	hv       *hostView
	rollouts []*fleet.Rollout
}

// servingOrder lists the hosts that wait in an open rollout and hold no
// lease: what hosts ask for themselves (local rollouts) first, then the
// oldest rollout first, inside a rollout by stage (broken hosts, the canary,
// the rest), then ascending weight and name.
func (p *pass) servingOrder() []*waiting {
	var (
		order  []*waiting
		byHost = make(map[string]*waiting, len(p.hosts))
	)

	sortRollouts(p.rollouts)

	rollouts := slices.Clone(p.rollouts)
	slices.SortStableFunc(rollouts, func(a, b *fleet.Rollout) int {
		return cmp.Compare(localFirst(a), localFirst(b))
	})

	for _, r := range rollouts {
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

// localFirst ranks the local rollouts before the others.
func localFirst(r *fleet.Rollout) int {
	if r.Kind == fleet.RolloutLocal {
		return 0
	}

	return 1
}

// grant makes the grants of this pass and builds the plan.
func (p *pass) grant() *Plan {
	g := &granter{p: p, budget: p.budget(), order: make(map[string]*rolloutOrder, len(p.rollouts))}
	g.gate, g.gateTrusted = p.gate()

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
		IndependentHosts:  g.budget.independent,
		MaxIndependent:    g.budget.maxIndependent,
		OfflineHosts:      g.budget.offlineHosts,
		OfflineWeight:     g.budget.offlineWeight,
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
		case hv.working():
			// At work without a lease: it is down like a host that holds one.
			hp.Slot = SlotLeased
		case hv.offline:
			hp.Slot = SlotOffline
		case hv.held:
			hp.Slot = SlotHeld
		case hv.stale:
			hp.Slot = SlotStale
		case failed[name]:
			hp.Slot = SlotFailed
		case hv.charged():
			// Not proven back after a failed lease: down like a leased host.
			hp.Slot = SlotLeased
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
