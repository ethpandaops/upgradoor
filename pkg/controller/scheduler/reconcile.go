package scheduler

import (
	"cmp"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// actorScheduler is the audit actor of the scheduler's own decisions. A
	// person can never carry the name: actorName rewrites it.
	actorScheduler = "scheduler"
	// leaseStartGrace is how long after a lease's expiry a sync without an
	// operation is not yet taken as proof that the agent did not start. The
	// agent may start until its own deadline, which lies up to one response
	// latency after the controller's, and a sync that was built before the
	// start can arrive after it.
	leaseStartGrace = time.Minute
)

// operatorSkipped reports whether a person skipped the member.
func operatorSkipped(m *fleet.RolloutHost) bool {
	return m.State == fleet.HostSkipped && m.SkippedBy != ""
}

// needKey is the key of the rollout an observed need belongs to; empty for
// the need of a manual directive, a local need and a group need (which
// belongs to its group's rollout when that holds it: assignGroupNeeds).
func needKey(n *protocol.Need) string {
	switch {
	case n.DirectiveID != "", isLocal(n), isGroupNeed(n):
		return ""
	case n.Kind == protocol.NeedPackages && n.Packages != nil:
		return prefixPackages + protocol.PackagesKey(n.Packages.Packages)
	case n.Kind == protocol.NeedReboot:
		return keyReboot
	default:
		return ""
	}
}

// World is everything one reconcile depends on. Reconcile does not change
// anything a World points at.
type World struct {
	Now time.Time
	// Since is when this scheduler started. An agent can only have been heard
	// from then on, so silence (a stale host, a silent lease, an expected host
	// that never reported) is measured from the later of the last sync and
	// Since: a controller outage is not the agents' silence. The zero value
	// measures from the last sync alone.
	Since    time.Time
	Settings fleet.Settings
	Health   fleet.NetworkHealth
	// Liveness holds the rows of the newest stored epochs (at least
	// readyEpochs + 2 of them when available).
	Liveness []fleet.Liveness
	// Hosts are all known hosts with their last sync.
	Hosts []*fleet.Host
	// Rollouts are the non-terminal rollouts. No decision reads a rollout
	// that is closed.
	Rollouts []*fleet.Rollout
	// Leases are the non-terminal leases.
	Leases []*fleet.Lease
	// Directives are the pending directives plus every directive that
	// belongs to a non-terminal rollout.
	Directives []*fleet.Directive
	// NewID returns a new unique ID.
	NewID func() string
}

// Outcome is the result of one reconcile: the rows to write and the plan.
type Outcome struct {
	// Rollouts, Leases and Directives are the new and changed rows.
	Rollouts   []*fleet.Rollout
	Leases     []*fleet.Lease
	Directives []*fleet.Directive
	// Broken are the broken marks of the hosts on which a lease failed or
	// was released in this pass, as they are from now on.
	Broken []store.HostBroken
	// Pulls are the pull windows that were granted, changed or ended.
	Pulls []store.HostPull
	Audit []fleet.AuditEntry
	Plan  *Plan
}

// Changes returns the outcome as one store transaction.
func (o *Outcome) Changes() *store.Changes {
	return &store.Changes{
		Rollouts:   o.Rollouts,
		Leases:     o.Leases,
		Directives: o.Directives,
		Broken:     o.Broken,
		Pulls:      o.Pulls,
		Audit:      o.Audit,
	}
}

// Reconcile applies every rule of docs/scheduler.md once. It is pure: no
// I/O, no clock, no randomness; the same World gives the same Outcome as
// long as NewID does.
func Reconcile(w *World) *Outcome {
	p := newPass(w)

	p.syncDirectives()
	p.advanceLeases()
	p.syncRollouts()

	plan := p.grant()

	pulls := p.pulls(plan)
	out := p.outcome(plan)
	out.Pulls = pulls

	return out
}

// hostView is one host with what the pass derives about it.
type hostView struct {
	h      *fleet.Host
	weight uint64
	// live is how many of the host's validators attested in the newest
	// stored liveness epoch; the full weight when there is no row.
	live      uint64
	liveKnown bool
	// held: a hold, or a known-offline mark (offline), keeps the host from
	// every lease.
	held    bool
	offline bool
	// stale: no sync for hostStaleAfter, measured from World.Since at the
	// earliest.
	stale bool
	// lease is the host's non-terminal lease.
	lease *fleet.Lease
	// broken are the host's broken marks as the pass leaves them
	// (fleet.Host.Broken); never changed in place.
	broken map[string]string
}

func (hv *hostView) allows(verb protocol.Verb) bool {
	return hv.h.Status != nil && slices.Contains(hv.h.Status.Verbs, verb)
}

func (hv *hostView) needs() []protocol.Need {
	if hv.h.Status == nil {
		return nil
	}

	return hv.h.Status.Needs
}

// operation returns the operation of the host's last sync, or nil.
func (hv *hostView) operation() *protocol.Operation {
	if hv.h.Status == nil {
		return nil
	}

	return hv.h.Status.Operation
}

// working reports whether the host works on an operation that no open lease
// of it covers: its agent reports an operation that is not over, and the host
// holds no lease. That is a host whose lease this controller closed without
// the agent's word (it was silent for leaseTtl and is back), or never knew
// (the database was lost). It is down, or about to be, all the same: it
// counts as leased for the budget, the host limit and the gate, and is
// granted nothing until the operation is over. A terminal state of a lease is
// terminal: the lease is not opened again. A stale host reports nothing; what
// it said last is not counted.
func (hv *hostView) working() bool {
	op := hv.operation()

	return hv.lease == nil && !hv.stale && op != nil && !op.Phase.Terminal()
}

// occupied reports whether the host takes a slot: it holds a lease or is
// working without one.
func (hv *hostView) occupied() bool {
	return hv.lease != nil || hv.working()
}

// pass is the working state of one reconcile.
type pass struct {
	w   *World
	now time.Time
	set *fleet.Settings

	hosts map[string]*hostView
	names []string

	// rollouts are working copies of the open rollouts, oldest first, plus
	// the ones created in this pass.
	rollouts     []*fleet.Rollout
	rolloutByID  map[string]*fleet.Rollout
	origRollouts map[string]*fleet.Rollout

	leases     []*fleet.Lease
	origLeases map[string]*fleet.Lease

	directives     []*fleet.Directive
	origDirectives map[string]*fleet.Directive
	// byHost indexes the directives by host, and members the members of each
	// rollout by host: a pass looks both up per host, and a fleet has many.
	byHost  map[string][]*fleet.Directive
	members map[*fleet.Rollout]*memberIndex
	// peers is minPeers, once it was asked for.
	peers int

	audit []fleet.AuditEntry

	// rows indexes liveness by host and epoch; newestEpoch is the newest
	// epoch with any row.
	rows        map[string]map[uint64]fleet.Liveness
	newestEpoch uint64
	haveRows    bool

	// needs holds the reported needs per rollout ID and host.
	needs map[string]map[string][]protocol.Need
	// marked names the hosts whose broken marks this pass changed.
	marked map[string]bool
	// before is the budget before this pass grants anything; see
	// budgetBefore.
	before *budgetState
	// first maps a rollout to the lease whose release in this pass was its
	// first success (pauseAfterCanary).
	first map[string]*fleet.Lease
	// refs are the hosts that a host under a readiness check is compared with,
	// as the pass found them (references); medians their median peer count
	// per client, -1 where too few of them probe it (fleetMedian).
	refs    []*hostView
	medians map[string]int
}

// budgetBefore is the budget as the leases and the liveness stand before
// this pass grants anything. The canary is chosen against it.
func (p *pass) budgetBefore() *budgetState {
	if p.before == nil {
		p.before = p.budget()
	}

	return p.before
}

func newPass(w *World) *pass {
	p := &pass{
		w:              w,
		now:            w.Now,
		set:            &w.Settings,
		hosts:          make(map[string]*hostView, len(w.Hosts)),
		names:          make([]string, 0, len(w.Hosts)),
		rollouts:       make([]*fleet.Rollout, 0, len(w.Rollouts)+2),
		rolloutByID:    make(map[string]*fleet.Rollout, len(w.Rollouts)+2),
		origRollouts:   make(map[string]*fleet.Rollout, len(w.Rollouts)),
		leases:         make([]*fleet.Lease, 0, len(w.Leases)+2),
		origLeases:     make(map[string]*fleet.Lease, len(w.Leases)),
		directives:     make([]*fleet.Directive, 0, len(w.Directives)+2),
		origDirectives: make(map[string]*fleet.Directive, len(w.Directives)),
		byHost:         make(map[string][]*fleet.Directive, len(w.Directives)),
		members:        make(map[*fleet.Rollout]*memberIndex, len(w.Rollouts)+2),
		medians:        make(map[string]int, 2),
		rows:           make(map[string]map[uint64]fleet.Liveness, len(w.Hosts)),
		needs:          make(map[string]map[string][]protocol.Need, len(w.Rollouts)+2),
		marked:         make(map[string]bool, 2),
		first:          make(map[string]*fleet.Lease, 1),
	}

	p.indexLiveness()

	for _, h := range w.Hosts {
		hv := &hostView{
			h:       h,
			weight:  h.Weight(),
			broken:  h.Broken,
			held:    h.Held() || h.KnownOffline(w.Now),
			offline: h.KnownOffline(w.Now),
			stale:   hostStale(h, w.Now, w.Since, w.Settings.HostStaleAfter.Std()),
		}

		hv.live, hv.liveKnown = p.liveOf(h.Name, hv.weight)

		p.hosts[h.Name] = hv
		p.names = append(p.names, h.Name)
	}

	slices.Sort(p.names)

	for _, r := range w.Rollouts {
		c := cloneRollout(r)
		p.rollouts = append(p.rollouts, c)
		p.rolloutByID[c.ID] = c
		p.origRollouts[c.ID] = r
	}

	sortRollouts(p.rollouts)

	for _, l := range w.Leases {
		c := cloneLease(l)
		p.leases = append(p.leases, c)
		p.origLeases[c.ID] = l
	}

	slices.SortFunc(p.leases, func(a, b *fleet.Lease) int {
		return cmp.Or(a.GrantedAt.Compare(b.GrantedAt), cmp.Compare(a.ID, b.ID))
	})

	for _, l := range p.leases {
		if hv := p.hosts[l.Host]; hv != nil && hv.lease == nil {
			hv.lease = l
		}
	}

	for _, d := range w.Directives {
		c := *d
		p.addDirective(&c)
		p.origDirectives[c.ID] = d
	}

	return p
}

// addDirective takes a directive into the pass.
func (p *pass) addDirective(d *fleet.Directive) {
	p.directives = append(p.directives, d)
	p.byHost[d.Host] = append(p.byHost[d.Host], d)
}

// memberIndex maps the hosts of a rollout to their place in its member list,
// as long as the list has the length it was built for.
type memberIndex struct {
	size   int
	byHost map[string]int
}

// member returns the member entry of a host in a rollout, or nil. It is
// Rollout.Host without the scan of the member list.
func (p *pass) member(r *fleet.Rollout, host string) *fleet.RolloutHost {
	idx := p.members[r]
	if idx == nil || idx.size != len(r.Hosts) {
		idx = &memberIndex{size: len(r.Hosts), byHost: make(map[string]int, len(r.Hosts))}

		for i := range r.Hosts {
			idx.byHost[r.Hosts[i].Host] = i
		}

		p.members[r] = idx
	}

	if i, ok := idx.byHost[host]; ok {
		return &r.Hosts[i]
	}

	return nil
}

func (p *pass) indexLiveness() {
	for _, row := range p.w.Liveness {
		byEpoch := p.rows[row.Host]
		if byEpoch == nil {
			byEpoch = make(map[uint64]fleet.Liveness, 4)
			p.rows[row.Host] = byEpoch
		}

		byEpoch[row.Epoch] = row

		if !p.haveRows || row.Epoch > p.newestEpoch {
			p.newestEpoch = row.Epoch
			p.haveRows = true
		}
	}
}

// liveOf returns the host's attesting validators in the newest stored epoch.
// A host without a row counts as fully live.
func (p *pass) liveOf(host string, weight uint64) (live uint64, known bool) {
	if !p.haveRows {
		return weight, false
	}

	row, ok := p.rows[host][p.newestEpoch]
	if !ok {
		return weight, false
	}

	return min(row.Live, weight), true
}

// later returns the later of two times.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

// hostStale reports whether the host's agent has been silent for longer than
// h.StaleAfter(staleAfter), as far as a scheduler that started at since can
// tell.
func hostStale(h *fleet.Host, now, since time.Time, staleAfter time.Duration) bool {
	return now.Sub(later(h.LastSeen, since)) > h.StaleAfter(staleAfter)
}

// sortRollouts orders rollouts oldest first.
func sortRollouts(rs []*fleet.Rollout) {
	slices.SortStableFunc(rs, func(a, b *fleet.Rollout) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
}

// record appends an audit entry by the scheduler.
func (p *pass) record(action, target string, detail map[string]any) {
	p.audit = append(p.audit, fleet.AuditEntry{
		Time:   p.now,
		Actor:  actorScheduler,
		Action: action,
		Target: target,
		Detail: detail,
	})
}

// setMember moves a member to a state.
func (p *pass) setMember(m *fleet.RolloutHost, state fleet.HostState) {
	if m.State == state {
		return
	}

	m.State = state
	m.UpdatedAt = p.now
	m.SkippedBy = ""

	if state == fleet.HostDone || state == fleet.HostExpected {
		m.Error = ""
	}
}

// outcome collects what changed against the world.
func (p *pass) outcome(plan *Plan) *Outcome {
	out := &Outcome{Audit: p.audit, Plan: plan}

	for _, name := range p.names {
		if p.marked[name] && !maps.Equal(p.hosts[name].broken, p.hosts[name].h.Broken) {
			out.Broken = append(out.Broken, store.HostBroken{Host: name, Marks: p.hosts[name].broken})
		}
	}

	for _, r := range p.rollouts {
		orig := p.origRollouts[r.ID]
		if orig != nil && reflect.DeepEqual(orig, r) {
			continue
		}

		r.UpdatedAt = p.now
		out.Rollouts = append(out.Rollouts, r)
	}

	for _, l := range p.leases {
		if orig := p.origLeases[l.ID]; orig != nil && reflect.DeepEqual(orig, l) {
			continue
		}

		out.Leases = append(out.Leases, l)
	}

	for _, d := range p.directives {
		if orig := p.origDirectives[d.ID]; orig != nil && *orig == *d {
			continue
		}

		d.UpdatedAt = p.now
		out.Directives = append(out.Directives, d)
	}

	return out
}

func cloneRollout(r *fleet.Rollout) *fleet.Rollout {
	c := *r
	c.Hosts = slices.Clone(r.Hosts)
	c.Packages = slices.Clone(r.Packages)
	c.EndedAt = cloneTime(r.EndedAt)

	return &c
}

func cloneLease(l *fleet.Lease) *fleet.Lease {
	c := *l
	c.Needs = slices.Clone(l.Needs)
	c.AgentDoneAt = cloneTime(l.AgentDoneAt)
	c.SyncedSince = cloneTime(l.SyncedSince)
	c.SyncedUntil = cloneTime(l.SyncedUntil)
	c.EndedAt = cloneTime(l.EndedAt)
	c.Operation = cloneOperation(l.Operation)
	c.PeerChecks = slices.Clone(l.PeerChecks)

	if l.Cancelled != nil {
		cancelled := *l.Cancelled
		c.Cancelled = &cancelled
	}

	if l.Rollouts != nil {
		c.Rollouts = make(map[string]string, len(l.Rollouts))
		for k, v := range l.Rollouts {
			c.Rollouts[k] = v
		}
	}

	return &c
}

func cloneOperation(op *protocol.Operation) *protocol.Operation {
	if op == nil {
		return nil
	}

	c := *op
	c.Steps = slices.Clone(op.Steps)

	return &c
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}

	c := *t

	return &c
}
