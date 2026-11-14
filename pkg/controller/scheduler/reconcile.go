package scheduler

import (
	"cmp"
	"reflect"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// actorScheduler is the audit actor of the scheduler's own decisions.
	actorScheduler = "scheduler"
	// grantedByAdoption marks a lease created from an operation the agent
	// reported for a lease the controller did not know.
	grantedByAdoption = "adopted"
	// carriedPrefix starts the note on a pending member that had failed in
	// the rollout this one supersedes. Such members are served first.
	carriedPrefix = "failed in superseded rollout "
)

// World is everything one reconcile depends on. Reconcile does not change
// anything a World points at.
type World struct {
	Now      time.Time
	Settings fleet.Settings
	Health   fleet.NetworkHealth
	// Liveness holds the rows of the newest stored epochs (at least
	// soakEpochs + 2 of them when available).
	Liveness []fleet.Liveness
	// Hosts are all known hosts with their last sync.
	Hosts []*fleet.Host
	// Rollouts are the non-terminal rollouts.
	Rollouts []*fleet.Rollout
	// Closed are terminal rollouts, newest first. It must hold at least the
	// latest closed rollout of every key that hosts still report and of
	// every image reference and the packages kind.
	Closed []*fleet.Rollout
	// Leases are the non-terminal leases.
	Leases []*fleet.Lease
	// Directives are the pending directives plus every directive that
	// belongs to a non-terminal rollout.
	Directives []*fleet.Directive
	// ClosedLeases names the terminal leases among the lease IDs that hosts
	// report an operation for. An operation for a lease that is neither
	// open nor listed here is adopted.
	ClosedLeases map[string]bool
	// NewID returns a new unique ID.
	NewID func() string
}

// Outcome is the result of one reconcile: the rows to write and the plan.
type Outcome struct {
	// Rollouts, Leases and Directives are the new and changed rows.
	Rollouts   []*fleet.Rollout
	Leases     []*fleet.Lease
	Directives []*fleet.Directive
	Audit      []fleet.AuditEntry
	Plan       *Plan
}

// Changes returns the outcome as one store transaction.
func (o *Outcome) Changes() *store.Changes {
	return &store.Changes{
		Rollouts:   o.Rollouts,
		Leases:     o.Leases,
		Directives: o.Directives,
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
	p.adoptOperations()
	p.syncRollouts()

	plan := p.grant()

	return p.outcome(plan)
}

// hostView is one host with what the pass derives about it.
type hostView struct {
	h      *fleet.Host
	weight uint64
	// live is how many of the host's validators attested in the newest
	// stored liveness epoch; the full weight when there is no row.
	live      uint64
	liveKnown bool
	held      bool
	stale     bool
	// lease is the host's non-terminal lease.
	lease *fleet.Lease
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

// class is the ordering class of the host: StageNoValidators,
// StageBatches (any validator host) or StageBootnodes.
func (hv *hostView) class() fleet.Stage {
	switch {
	case hv.h.IsBootnode():
		return fleet.StageBootnodes
	case hv.weight == 0:
		return fleet.StageNoValidators
	default:
		return fleet.StageBatches
	}
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

	audit []fleet.AuditEntry

	// rows indexes liveness by host and epoch; newestEpoch is the newest
	// epoch with any row.
	rows        map[string]map[uint64]fleet.Liveness
	newestEpoch uint64
	haveRows    bool

	// needs holds the reported needs per rollout ID and host.
	needs map[string]map[string][]protocol.Need
	// carried maps hosts that had failed in a rollout superseded in this
	// pass to that rollout's ID, per successor group (image reference or
	// packages kind).
	carried map[string]map[string]string
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
		rows:           make(map[string]map[uint64]fleet.Liveness, len(w.Hosts)),
		needs:          make(map[string]map[string][]protocol.Need, len(w.Rollouts)+2),
		carried:        make(map[string]map[string]string, 2),
	}

	p.indexLiveness()

	for _, h := range w.Hosts {
		hv := &hostView{
			h:      h,
			weight: h.Weight(),
			held:   h.Held(),
			stale:  h.Stale(w.Now, w.Settings.HostStaleAfter.Std()),
		}
		hv.live, hv.liveKnown = p.liveOf(h.Name, hv.weight)

		p.hosts[h.Name] = hv
		p.names = append(p.names, h.Name)
	}

	slices.Sort(p.names)

	for _, r := range w.Rollouts {
		if r.State.Terminal() {
			continue
		}

		c := cloneRollout(r)
		p.rollouts = append(p.rollouts, c)
		p.rolloutByID[c.ID] = c
		p.origRollouts[c.ID] = r
	}

	sortRollouts(p.rollouts)

	for _, l := range w.Leases {
		if l.State.Terminal() {
			continue
		}

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
		p.directives = append(p.directives, &c)
		p.origDirectives[c.ID] = d
	}

	return p
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

	if state == fleet.HostDone || state == fleet.HostExpected {
		m.Error = ""
	}
}

// outcome collects what changed against the world.
func (p *pass) outcome(plan *Plan) *Outcome {
	out := &Outcome{Audit: p.audit, Plan: plan}

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
	c.EndedAt = cloneTime(l.EndedAt)
	c.Operation = cloneOperation(l.Operation)

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
