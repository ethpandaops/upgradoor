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
	reasonAfterNoVals   = "after hosts without validators"
	reasonAfterVals     = "after validator hosts"
	reasonAfterCanary   = "after canary "
	reasonAfterBootnode = "after bootnode "
	reasonNext          = "next"
)

// Gate reasons.
const (
	gateBeacon   = "beacon node unreachable"
	gateLiveness = "liveness data stale"
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

// verdict is why one host waits (rankGrantable: it does not).
type verdict struct {
	rank   int
	reason string
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

// admit checks the three grant conditions and the host-count limit for a
// host that costs cost.
func (b *budgetState) admit(cost uint64) verdict {
	fits := cost == 0 ||
		float64(b.unavailable+cost) <= b.allowed+budgetEpsilon ||
		(b.unavailable == 0 && b.allowed > 0 && b.leased == 0)

	if !fits {
		used := 0.0
		if b.total > 0 {
			used = float64(b.unavailable) * 100 / float64(b.total)
		}

		return verdict{rankBudget, "budget " + percent(used) + " of " + percent(b.percent)}
	}

	if b.leased >= b.maxLeased {
		return verdict{rankHostLimit, fmt.Sprintf("host limit %d of %d", b.leased, b.maxLeased)}
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

	if n.UpdatedAt.IsZero() || epoch <= 0 || p.now.Sub(n.UpdatedAt) > 2*epoch {
		g.Reasons = append(g.Reasons, gateBeacon)
	}

	if n.FinalityLagEpochs > p.set.MaxFinalityLagEpochs {
		g.Reasons = append(g.Reasons, fmt.Sprintf("finality lag %d epochs > %d",
			n.FinalityLagEpochs, p.set.MaxFinalityLagEpochs))
	}

	var tracked, live, total uint64

	for _, hv := range p.hosts {
		tracked += hv.weight

		// Validators of hosts holding a lease are left out on both sides:
		// a rollout must not close the gate on itself.
		if hv.lease == nil {
			total += hv.weight
			live += hv.live
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
		return verdict{rankRolloutPaused, reasonRolloutPaused}
	case policy == fleet.PolicyManual && !r.Approved:
		return verdict{rankApproval, reasonApproval}
	case policy == fleet.PolicyPaused:
		return verdict{rankPolicyPaused, reasonPolicyPaused}
	case r.State != fleet.RolloutActive:
		return verdict{rankHalted, reasonHaltedPrefix + r.Reason}
	default:
		return verdict{}
	}
}

// memberClass is the ordering class of a member; for a forgotten host it
// comes from the stored stage.
func (p *pass) memberClass(m *fleet.RolloutHost) fleet.Stage {
	if hv := p.hosts[m.Host]; hv != nil {
		return hv.class()
	}

	if m.Stage == fleet.StageCanary {
		return fleet.StageBatches
	}

	return m.Stage
}

// orderVerdict applies the stage ordering of one rollout to one host.
func (p *pass) orderVerdict(r *fleet.Rollout, hv *hostView) verdict {
	var (
		class        = hv.class()
		noVals, vals bool
		bootnode     string
	)

	for i := range r.Hosts {
		m := &r.Hosts[i]
		if m.Host == hv.h.Name || !p.blocks(r, m) {
			continue
		}

		// A host carried over as failed that is still expected to report is
		// served before the hosts of its own and of later stages.
		if m.State == fleet.HostExpected && strings.HasPrefix(m.Error, carriedPrefix) &&
			p.memberClass(m) <= class {
			return verdict{rankOrder, reasonAfter + m.Host}
		}

		switch p.memberClass(m) {
		case fleet.StageNoValidators:
			noVals = true
		case fleet.StageBootnodes:
			if m.State == fleet.HostLeased {
				bootnode = m.Host
			}
		default:
			vals = true
		}
	}

	switch {
	case class == fleet.StageNoValidators:
		return verdict{}
	case noVals:
		return verdict{rankOrder, reasonAfterNoVals}
	case class == fleet.StageBatches:
		if r.Canary == "" || r.Canary == hv.h.Name {
			return verdict{}
		}

		if c := r.Host(r.Canary); c != nil && c.State == fleet.HostDone {
			return verdict{}
		}

		return verdict{rankOrder, reasonAfterCanary + r.Canary}
	case vals:
		return verdict{rankOrder, reasonAfterVals}
	case bootnode != "":
		return verdict{rankOrder, reasonAfterBootnode + bootnode}
	default:
		return verdict{}
	}
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
}

// evaluate decides whether the host's needs in one rollout are grantable
// now, the budget aside.
func (g *granter) evaluate(r *fleet.Rollout, hv *hostView) (verdict, []protocol.Need) {
	if v := g.p.policyVerdict(r); v.rank != rankGrantable {
		return v, nil
	}

	reported := g.p.needs[r.ID][hv.h.Name]
	needs := make([]protocol.Need, 0, len(reported))

	for _, n := range reported {
		if hv.allows(n.Kind.Verb()) {
			needs = append(needs, n)
		}
	}

	switch {
	case len(needs) == 0:
		return verdict{rankVerb, reasonVerb}, nil
	case !r.IgnoreBudget && !g.gate.OK && !g.repairs(hv):
		return verdict{rankGate, reasonGatePrefix + g.gate.Reasons[0]}, nil
	case r.Force && g.forced[r.ID]:
		return verdict{rankNext, reasonNext}, nil
	case r.Force:
		return verdict{}, needs
	}

	if v := g.p.orderVerdict(r, hv); v.rank != rankGrantable {
		return v, nil
	}

	return verdict{}, needs
}

// repairs reports whether a grant to the host passes a failing gate: the host
// has validators and none of them is attesting, so serving it cannot make
// the network worse and may be what brings it back. Without this a host that
// a rollout broke would keep participation below the gate and the gate would
// keep the repair away for ever. It needs data it can trust: a gate that
// fails because the beacon view or the liveness data is missing passes
// nobody.
func (g *granter) repairs(hv *hostView) bool {
	if hv.weight == 0 || hv.live != 0 {
		return false
	}

	return !slices.Contains(g.gate.Reasons, gateBeacon) &&
		!slices.Contains(g.gate.Reasons, gateLiveness)
}

// consider grants the host a lease for everything grantable, or says what
// it waits for.
func (g *granter) consider(hv *hostView, rollouts []*fleet.Rollout) verdict {
	switch {
	case hv.held:
		return verdict{rankHeld, reasonHeld}
	case hv.stale:
		return verdict{rankSilent, reasonAgentSilent}
	case g.p.set.Paused:
		return verdict{rankPaused, reasonPaused}
	}

	var (
		best           verdict
		normal, exempt []part
	)

	for _, r := range rollouts {
		v, needs := g.evaluate(r, hv)

		switch {
		case v.rank != rankGrantable:
			if v.rank > best.rank {
				best = v
			}
		case r.IgnoreBudget:
			exempt = append(exempt, part{r, needs})
		default:
			normal = append(normal, part{r, needs})
		}
	}

	if len(normal) > 0 {
		if v := g.budget.admit(hv.live); v.rank != rankGrantable {
			best, normal = v, nil
		}
	}

	parts := slices.Concat(exempt, normal)
	if len(parts) == 0 {
		return best
	}

	g.lease(hv, parts)

	return verdict{}
}

// lease creates the lease for one host and books it.
func (g *granter) lease(hv *hostView, parts []part) {
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

		switch {
		case r.Force:
			g.forced[r.ID] = true
		case hv.class() == fleet.StageBatches && r.Canary == "":
			r.Canary = host
			m.Stage = fleet.StageCanary
		}

		l.Force = l.Force && r.Force
		ignoreBudget = ignoreBudget || r.IgnoreBudget
		ids = append(ids, r.ID)

		if r.Kind != fleet.RolloutManual || r.CreatedBy != l.GrantedBy {
			l.GrantedBy = actorScheduler
		}
	}

	g.budget.take(hv.live)

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
	})
}

// waiting is one host that waits in an open rollout.
type waiting struct {
	hv       *hostView
	rollouts []*fleet.Rollout
}

// servingOrder lists the hosts that wait in an open rollout and hold no
// lease: oldest rollout first, inside a rollout by stage, then hosts carried
// over as failed, hosts that are fully down, ascending weight and name.
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
				cmp.Compare(serveStage(r, ha), serveStage(r, hb)),
				cmp.Compare(serveLate(a, ha), serveLate(b, hb)),
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

func serveStage(r *fleet.Rollout, hv *hostView) fleet.Stage {
	class := hv.class()
	if class == fleet.StageBatches && r.Canary == hv.h.Name {
		return fleet.StageCanary
	}

	return class
}

// serveLate ranks hosts inside a stage: 0 for hosts carried over as failed,
// 1 for hosts whose validators are all down, 2 for the rest.
func serveLate(m *fleet.RolloutHost, hv *hostView) int {
	switch {
	case strings.HasPrefix(m.Error, carriedPrefix):
		return 0
	case hv.weight > 0 && hv.live == 0:
		return 1
	default:
		return 2
	}
}

// grant makes the grants of this pass and builds the plan.
func (p *pass) grant() *Plan {
	g := &granter{
		p:      p,
		gate:   p.gate(),
		budget: p.budget(),
		forced: make(map[string]bool, 2),
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
		v := g.consider(w.hv, w.rollouts)
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
