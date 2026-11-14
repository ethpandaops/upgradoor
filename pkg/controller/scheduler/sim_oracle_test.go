package scheduler

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// simHostFact is what the simulated world knows about one registered host.
// None of it comes from the scheduler.
type simHostFact struct {
	weight   uint64
	bootnode bool
	// live is how many validators attested in the newest liveness epoch the
	// beacon service stored (the full weight without a row).
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
}

func (h *simHostFact) class() fleet.Stage {
	switch {
	case h.bootnode:
		return fleet.StageBootnodes
	case h.weight == 0:
		return fleet.StageNoValidators
	default:
		return fleet.StageBatches
	}
}

func (h *simHostFact) held() bool {
	return h.holdWanted || (h.status != nil && h.status.Hold.Active())
}

// simFacts is the state of the simulated world at one observation.
type simFacts struct {
	now      time.Time
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
	// fresh is true when a reconcile has just been committed against
	// exactly these facts.
	fresh bool
}

// simSnapshot is the committed scheduler state at one observation, in
// creation order.
type simSnapshot struct {
	rollouts   []*fleet.Rollout
	leases     []*fleet.Lease
	directives []*fleet.Directive
}

func (s *simSnapshot) rollout(id string) *fleet.Rollout {
	for _, r := range s.rollouts {
		if r.ID == id {
			return r
		}
	}

	return nil
}

// oracle checks the invariants of docs/scheduler.md on a history of
// observations. It keeps its own books: which leases and rollouts it has
// seen in which state, and which rollouts a failed lease halted.
type oracle struct {
	rollouts map[string]fleet.RolloutState
	leases   map[string]fleet.LeaseState
	// halted maps a rollout to the lease whose failure halted it.
	halted map[string]string
}

func newOracle() *oracle {
	return &oracle{
		rollouts: make(map[string]fleet.RolloutState, 16),
		leases:   make(map[string]fleet.LeaseState, 64),
		halted:   make(map[string]string, 4),
	}
}

// observe checks one observation against the history and returns the first
// violated invariant.
func (o *oracle) observe(f *simFacts, snap *simSnapshot) error {
	if err := o.checkStates(snap); err != nil {
		return err
	}

	for _, id := range f.retried {
		delete(o.halted, id)
	}

	var (
		granted []*fleet.Lease
		leased  = make(map[string]bool, len(snap.leases))
	)

	for _, l := range snap.leases {
		was, known := o.leases[l.ID]

		switch {
		case !known && l.GrantedBy == grantedByAdoption:
			if !l.State.Terminal() {
				leased[l.Host] = true
			}
		case !known:
			granted = append(granted, l)
		case !l.State.Terminal():
			leased[l.Host] = true
		}

		if l.State != was {
			if err := o.checkLeaseEnd(f, snap, l); err != nil {
				return err
			}
		}
	}

	if err := o.checkHalts(snap); err != nil {
		return err
	}

	// The gate is evaluated once per reconcile, before the first grant.
	gate := oracleGate(f, leased)

	for _, l := range granted {
		if err := o.checkGrant(f, snap, l, leased, gate); err != nil {
			return fmt.Errorf("grant of lease %s to %s: %w", l.ID, l.Host, err)
		}

		leased[l.Host] = true
	}

	for _, l := range snap.leases {
		o.leases[l.ID] = l.State
	}

	for _, r := range snap.rollouts {
		o.rollouts[r.ID] = r.State
	}

	if f.fresh {
		return checkMembership(f, snap)
	}

	return nil
}

// checkStates: terminal states are terminal, nothing disappears, one
// non-terminal rollout per key and one non-terminal lease per host.
func (o *oracle) checkStates(snap *simSnapshot) error {
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
			return fmt.Errorf("lease %s left terminal state %s for %s", l.ID, was, l.State)
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

// checkLeaseEnd looks at a lease that changed state. A failed lease halts
// the rollouts it served; a released or expired one must agree with what the
// agent really did.
func (o *oracle) checkLeaseEnd(f *simFacts, snap *simSnapshot, l *fleet.Lease) error {
	host := f.hosts[l.Host]

	switch l.State {
	case fleet.LeaseFailed:
		o.haltedBy(snap, l)
	case fleet.LeaseExpired:
		if host != nil && host.disrupted[l.ID] {
			return fmt.Errorf("lease %s of %s expired as if it cost nothing, but the agent stopped containers for it",
				l.ID, l.Host)
		}
	case fleet.LeaseReleased:
		if host == nil {
			return fmt.Errorf("lease %s released for unknown host %s", l.ID, l.Host)
		}

		return checkRelease(f, l, host)
	default:
	}

	return nil
}

// haltedBy records the rollouts a failed lease halts: all it served, except
// those whose own steps are done while another step failed.
func (o *oracle) haltedBy(snap *simSnapshot, l *fleet.Lease) {
	stepFailed := l.Operation != nil && slices.ContainsFunc(l.Operation.Steps,
		func(s protocol.Step) bool { return s.Status == protocol.StepFailed })

	done := make(map[string]bool, len(l.Rollouts))
	for _, id := range l.Rollouts {
		done[id] = stepFailed
	}

	for needID, id := range l.Rollouts {
		if l.Operation == nil || stepStatus(l.Operation, needID) != protocol.StepDone {
			done[id] = false
		}
	}

	for id, spared := range done {
		if r := snap.rollout(id); r != nil && !r.State.Terminal() && !spared && r.Host(l.Host) != nil {
			o.halted[id] = l.ID
		}
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

// checkRelease: a lease is released only after the agent finished an
// operation that the world has seen recover.
func checkRelease(f *simFacts, l *fleet.Lease, host *simHostFact) error {
	doneAt, ok := host.doneAt[l.ID]
	if !ok {
		return fmt.Errorf("lease %s of %s released although the agent never finished it", l.ID, l.Host)
	}

	if l.Force {
		return nil
	}

	soak := f.settings.SoakEpochs
	epoch := f.health.EpochDuration()

	if host.weight == 0 || !f.health.LivenessKnown {
		if host.status == nil || !host.lastSync.After(doneAt) || !host.status.Node.Synced() {
			return fmt.Errorf("lease %s of %s released although its probes do not report synced after the operation "+
				"(done at %s, last sync %s, synced %v)", l.ID, l.Host, doneAt.Format(time.TimeOnly),
				host.lastSync.Format(time.TimeOnly), host.status != nil && host.status.Node.Synced())
		}

		return nil
	}

	want := max(uint64(math.Floor(f.settings.MinParticipationPercent*float64(host.weight)/100+budgetEpsilon)), 1)

	for i := range soak {
		if !f.havePublished || f.publishedEpoch < i {
			return fmt.Errorf("lease %s of %s released with fewer than %d liveness epochs", l.ID, l.Host, soak)
		}

		e := f.publishedEpoch - i
		live, ok := f.rows[e][l.Host]

		switch {
		case f.health.GenesisTime.Add(time.Duration(e) * epoch).Before(doneAt):
			return fmt.Errorf("lease %s of %s released after %d of %d complete epochs since the agent was done",
				l.ID, l.Host, i, soak)
		case !ok || live < want:
			return fmt.Errorf("lease %s of %s released although %d of %d validators attested in epoch %d (need %d)",
				l.ID, l.Host, live, host.weight, e, want)
		}
	}

	return nil
}

// oracleGate evaluates the network gate from the world. leased are the hosts
// holding a lease; their validators are left out of the participation.
func oracleGate(f *simFacts, leased map[string]bool) []string {
	var (
		reasons              []string
		n                    = &f.health
		epoch                = n.EpochDuration()
		tracked, total, live uint64
	)

	if n.UpdatedAt.IsZero() || epoch <= 0 || f.now.Sub(n.UpdatedAt) > 2*epoch {
		reasons = append(reasons, "beacon node unreachable")
	}

	if n.FinalityLagEpochs > f.settings.MaxFinalityLagEpochs {
		reasons = append(reasons, "finality lag")
	}

	for name, h := range f.hosts {
		tracked += h.weight

		if !leased[name] {
			total += h.weight
			live += h.live
		}
	}

	switch {
	case tracked == 0:
	case !n.LivenessKnown || !f.havePublished || n.CurrentEpoch > f.publishedEpoch+2:
		reasons = append(reasons, "liveness data stale")
	case total > 0 && float64(live)*100/float64(total) < f.settings.MinParticipationPercent:
		reasons = append(reasons, "participation")
	}

	return reasons
}

// oracleBudget checks the three grant conditions and the host-count limit
// for one host against the world's own count of unavailable weight.
func oracleBudget(f *simFacts, leased map[string]bool, host string) error {
	var total, unavailable uint64

	for name, h := range f.hosts {
		total += h.weight

		if leased[name] {
			unavailable += h.weight
		} else {
			unavailable += h.weight - h.live
		}
	}

	percent := f.settings.MaxUnavailablePercent
	allowed := percent * float64(total) / 100
	cost := f.hosts[host].live

	fits := cost == 0 ||
		float64(unavailable+cost) <= allowed+budgetEpsilon ||
		(unavailable == 0 && allowed > 0 && len(leased) == 0)
	if !fits {
		return fmt.Errorf("over budget: cost %d with %d of %d already unavailable, %v allowed and %d other leases",
			cost, unavailable, total, allowed, len(leased))
	}

	limit := max(1, int(math.Floor(percent*float64(len(f.hosts))/100+budgetEpsilon)))
	if len(leased) >= limit {
		return fmt.Errorf("host limit: %d hosts already hold a lease, at most %d of %d may", len(leased), limit, len(f.hosts))
	}

	return nil
}

// checkGrant checks one new lease. leased are the hosts that held a lease
// when it was granted; gate are the gate's failures at that reconcile.
func (o *oracle) checkGrant(
	f *simFacts, snap *simSnapshot, l *fleet.Lease, leased map[string]bool, gate []string,
) error {
	host := f.hosts[l.Host]

	switch {
	case l.State != fleet.LeaseGranted:
		return fmt.Errorf("new lease is %s, not granted", l.State)
	case host == nil || host.status == nil:
		return errors.New("the host is unknown or has never synced")
	case leased[l.Host]:
		return errors.New("the host already holds a lease")
	case host.held():
		return errors.New("the host is held")
	case f.now.Sub(host.lastSync) > f.settings.HostStaleAfter.Std():
		return fmt.Errorf("the host is stale: last sync %s ago", f.now.Sub(host.lastSync))
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

		r := snap.rollout(l.Rollouts[n.ID])
		if r == nil {
			return fmt.Errorf("need %s is served for no rollout", n.ID)
		}

		if slices.Contains(rollouts, r) {
			continue
		}

		rollouts = append(rollouts, r)
		exempt = exempt && r.IgnoreBudget

		if err := o.checkRolloutGrant(f, snap, r, l, host); err != nil {
			return fmt.Errorf("rollout %s (%s): %w", r.ID, r.Key, err)
		}
	}

	if exempt {
		return nil
	}

	if len(gate) > 0 {
		return fmt.Errorf("the network gate fails: %v", gate)
	}

	return oracleBudget(f, leased, l.Host)
}

// checkRolloutGrant checks that a rollout may be granted to a host: state,
// pause, policy, halt and ordering.
func (o *oracle) checkRolloutGrant(
	f *simFacts, snap *simSnapshot, r *fleet.Rollout, l *fleet.Lease, host *simHostFact,
) error {
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
	case policy == fleet.PolicyPaused:
		return errors.New("the policy is paused")
	case policy == fleet.PolicyManual && !r.Approved:
		return errors.New("the policy is manual and nobody approved the rollout")
	case m == nil || m.State != fleet.HostLeased || m.LeaseID != l.ID:
		return errors.New("the host is not a member leased by this lease")
	case r.Force:
		return nil
	}

	return checkOrdering(f, snap, r, l.Host, host)
}

// checkOrdering: every member of an earlier stage is settled or a
// non-blocking expected host; the canary and bootnode stages hold one lease
// at a time; the canary goes before every other validator host.
func checkOrdering(f *simFacts, snap *simSnapshot, r *fleet.Rollout, name string, host *simHostFact) error {
	class := host.class()

	for i := range r.Hosts {
		m := &r.Hosts[i]

		other := f.hosts[m.Host]
		if m.Host == name || other == nil {
			continue
		}

		switch {
		case other.class() < class:
			if m.State.Settled() || (m.State == fleet.HostExpected && !oracleBlocks(f, snap, r, m, other)) {
				continue
			}

			return fmt.Errorf("stage %d host granted while %s of stage %d is %s", class, m.Host, other.class(), m.State)
		case other.class() != class || m.State != fleet.HostLeased:
		case class == fleet.StageBootnodes:
			return fmt.Errorf("bootnode granted while bootnode %s holds a lease", m.Host)
		case class == fleet.StageBatches && r.Canary == name:
			return fmt.Errorf("canary granted while validator host %s holds a lease", m.Host)
		}
	}

	if class != fleet.StageBatches || r.Canary == name {
		return nil
	}

	if r.Canary == "" {
		return errors.New("validator host granted without a canary on record")
	}

	if c := r.Host(r.Canary); c == nil || c.State != fleet.HostDone {
		return fmt.Errorf("validator host granted before canary %s is done", r.Canary)
	}

	return nil
}

// oracleBlocks reports whether an expected member holds back later stages.
func oracleBlocks(f *simFacts, snap *simSnapshot, r *fleet.Rollout, m *fleet.RolloutHost, host *simHostFact) bool {
	stale := f.settings.HostStaleAfter.Std()
	if host.held() || host.status == nil || f.now.Sub(host.lastSync) > stale || f.now.Sub(r.CreatedAt) > stale {
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
