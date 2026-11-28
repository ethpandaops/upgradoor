package scheduler

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// defaultTickInterval is the reconcile period.
	defaultTickInterval = 2 * time.Second
	// livenessEpochs is the number of liveness epochs asked for.
	livenessEpochs = 6
)

// Store is the part of store.Store the scheduler uses.
type Store interface {
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	GetHost(ctx context.Context, name string) (*fleet.Host, error)
	DeleteHost(ctx context.Context, name string) error
	ListRollouts(ctx context.Context, filter store.RolloutFilter) ([]*fleet.Rollout, error)
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
	AbortedKeys(ctx context.Context) ([]string, error)
	ListLeases(ctx context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error)
	GetLease(ctx context.Context, id string) (*fleet.Lease, error)
	ListDirectives(ctx context.Context, filter store.DirectiveFilter) ([]*fleet.Directive, error)
	Commit(ctx context.Context, changes *store.Changes) error
	LoadSettings(ctx context.Context, defaults fleet.Settings) (fleet.Settings, error)
	SaveSettings(ctx context.Context, defaults, settings fleet.Settings) error
	DeployWindow(ctx context.Context) (fleet.DeployWindowState, error)
	ArmDeployWindow(ctx context.Context, at time.Time, entry *fleet.AuditEntry) (bool, error)
	AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error
}

var _ Store = store.Store(nil)

// Lookups is the part of discovery.Tracker the scheduler uses.
type Lookups interface {
	RefreshTag(ref string, since time.Time) bool
	LookedUp(ref string) (discovery.Lookup, bool)
}

var _ Lookups = discovery.Tracker(nil)

// Options configure the scheduler.
type Options struct {
	// Store holds all state. Required.
	Store Store
	// Health returns the beacon service's latest view. Without it the
	// network gate fails ("beacon node unreachable").
	Health func() fleet.NetworkHealth
	// Liveness returns the beacon service's per-host liveness of the newest
	// settled epochs, newest first. Without it no host's validators count as
	// attesting, and the gate stays closed while any host has validators.
	Liveness func(epochs int) []fleet.Liveness
	// Attested returns the beacon service's newest attested slots per host
	// (beacon.Service.AttestedSlots). Without it no host with validators
	// becomes ready by the readiness check.
	Attested func() map[string][]uint64
	// Defaults are the settings before the stored overrides.
	Defaults fleet.Settings
	// DeployWindow is the length of the initial deployment window; 0 is no
	// window.
	DeployWindow time.Duration
	// TickInterval is the reconcile period; 2s when zero.
	TickInterval time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// NewID returns a new unique ID; random when nil.
	NewID func() string
	// Lookups looks up the tags that hosts saw move on (TagMove). Without
	// it, a need the agent superseded is granted again at once.
	Lookups Lookups
	// Changed, when set, is called after a commit, after an audit entry was
	// appended (every action writes one) and after a reconcile whose plan
	// differs from the one before it, the time it was computed at aside. It
	// is how the API learns that the views it serves may have changed. It can
	// be called under the scheduler's lock: it must not block and must not
	// call back into the scheduler.
	Changed func()
}

type service struct {
	log  logrus.FieldLogger
	opts Options
	// since is when this scheduler was created (the controller's start). It
	// is the World's Since: silence is never counted from before it.
	since time.Time

	// mu serialises reconciles and actions; it guards moves, the
	// Outcome.TagMoves of the last committed reconcile.
	mu    sync.Mutex
	moves []TagMove

	planMu sync.RWMutex
	plan   *Plan

	nudge chan struct{}

	lifeMu sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ Service = (*service)(nil)

// New returns a scheduler. Start runs it.
func New(log logrus.FieldLogger, opts Options) Service { //nolint:gocritic // hugeParam: Options is the constructor contract, copied once.
	if opts.TickInterval <= 0 {
		opts.TickInterval = defaultTickInterval
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.NewID == nil {
		opts.NewID = randomID
	}

	if opts.Health == nil {
		opts.Health = func() fleet.NetworkHealth { return fleet.NetworkHealth{} }
	}

	if opts.Liveness == nil {
		opts.Liveness = func(int) []fleet.Liveness { return nil }
	}

	if opts.Attested == nil {
		opts.Attested = func() map[string][]uint64 { return nil }
	}

	return &service{
		log:   log.WithField("package", "scheduler"),
		opts:  opts,
		since: opts.Now(),
		plan:  &Plan{Queue: []QueueEntry{}, Hosts: map[string]HostPlan{}, Gate: Gate{Reasons: []string{}}},
		nudge: make(chan struct{}, 1),
	}
}

func randomID() string {
	var b [8]byte

	// crypto/rand.Read never fails on supported platforms (it aborts the
	// program instead).
	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// Start runs the reconcile loop until Stop is called or ctx is cancelled.
func (s *service) Start(ctx context.Context) error {
	if s.opts.Store == nil {
		return errors.New("scheduler: no store configured")
	}

	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()

	if s.cancel != nil {
		return errors.New("scheduler: already started")
	}

	ctx, s.cancel = context.WithCancel(ctx)

	s.wg.Add(1)

	go s.run(ctx)

	return nil
}

// Stop ends the reconcile loop and waits for it.
func (s *service) Stop() error {
	s.lifeMu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.lifeMu.Unlock()

	if cancel != nil {
		cancel()
		s.wg.Wait()
	}

	return nil
}

func (s *service) run(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.opts.TickInterval)
	defer ticker.Stop()

	for {
		if err := s.reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log.WithError(err).Error("Reconcile failed")
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.nudge:
			// Nudges come in bursts (a rollout's syncs): they share one
			// reconcile instead of running them back to back.
			select {
			case <-ctx.Done():
				return
			case <-time.After(nudgeGap):
			}
		}
	}
}

// nudgeGap is how long a nudged reconcile waits for more nudges.
const nudgeGap = 250 * time.Millisecond

// Nudge asks for a reconcile soon.
func (s *service) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Plan returns the result of the last committed reconcile.
func (s *service) Plan() *Plan {
	s.planMu.RLock()
	defer s.planMu.RUnlock()

	return s.plan
}

// reconcile loads the world, reconciles, commits the changes in one
// transaction and then publishes the plan.
func (s *service) reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, err := s.loadWorld(ctx)
	if err != nil {
		return err
	}

	out := Reconcile(w)

	if changes := out.Changes(); !changes.Empty() {
		if err := s.write(ctx, changes); err != nil {
			return err
		}

		s.log.WithFields(logrus.Fields{
			keyRollouts:  len(changes.Rollouts),
			"leases":     len(changes.Leases),
			"directives": len(changes.Directives),
		}).Debug("Committed reconcile")
	}

	s.moves = out.TagMoves

	for _, ref := range out.Refresh {
		s.opts.Lookups.RefreshTag(ref, w.Now)
	}

	s.planMu.Lock()
	same := samePlan(s.plan, out.Plan)
	s.plan = out.Plan
	s.planMu.Unlock()

	if !same {
		s.changed()
	}

	return nil
}

// samePlan reports whether two plans say the same. When they were computed
// does not count: an idle scheduler computes the same plan on every tick.
func samePlan(a, b *Plan) bool {
	if a == nil || b == nil {
		return a == b
	}

	x, y := *a, *b
	x.ComputedAt, y.ComputedAt = time.Time{}, time.Time{}

	return reflect.DeepEqual(&x, &y)
}

// changed tells the listener, if there is one, that committed state or the
// plan changed.
func (s *service) changed() {
	if s.opts.Changed != nil {
		s.opts.Changed()
	}
}

// loadWorld reads everything one reconcile depends on.
func (s *service) loadWorld(ctx context.Context) (*World, error) {
	st := s.opts.Store

	settings, err := st.LoadSettings(ctx, s.opts.Defaults)
	if err != nil {
		return nil, fmt.Errorf("scheduler: load settings: %w", err)
	}

	w := &World{
		Now:      s.opts.Now(),
		Since:    s.since,
		Settings: settings,
		Health:   s.opts.Health(),
		NewID:    s.opts.NewID,
		TagMoves: s.moves,
	}

	if s.opts.Lookups != nil {
		w.LookedUp = s.opts.Lookups.LookedUp
	}

	if w.DeployWindow, err = st.DeployWindow(ctx); err != nil {
		return nil, fmt.Errorf("scheduler: load deploy window: %w", err)
	}

	w.DeployWindow.Length = s.opts.DeployWindow

	if w.Hosts, err = st.ListHosts(ctx); err != nil {
		return nil, fmt.Errorf("scheduler: list hosts: %w", err)
	}

	w.Liveness = s.opts.Liveness(livenessEpochs)
	w.Attested = s.opts.Attested()

	if w.Rollouts, err = st.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true}); err != nil {
		return nil, fmt.Errorf("scheduler: list open rollouts: %w", err)
	}

	if w.Leases, err = st.ListLeases(ctx, store.LeaseFilter{OpenOnly: true}); err != nil {
		return nil, fmt.Errorf("scheduler: list open leases: %w", err)
	}

	if err := s.loadAborted(ctx, w); err != nil {
		return nil, err
	}

	if err := s.loadDirectives(ctx, w); err != nil {
		return nil, err
	}

	return w, nil
}

// write commits changes and tells the listener.
func (s *service) write(ctx context.Context, changes *store.Changes) error {
	stampAuthorization(ctx, changes.Audit)

	if err := s.opts.Store.Commit(ctx, changes); err != nil {
		return fmt.Errorf("scheduler: commit: %w", err)
	}

	s.logEvents(changes.Audit)
	s.changed()

	return nil
}

// loggedEvents are the audit actions that are logged at info as well, with
// their message.
var loggedEvents = map[string]string{
	"lease.granted":     "Lease granted",
	"lease.released":    "Lease released",
	"lease.failed":      "Lease failed",
	"rollout.created":   "Rollout opened",
	"rollout.halted":    "Rollout halted",
	"rollout.completed": "Rollout completed",
	"rollout.dropped":   "Rollout dropped",
	"action.abort":      "Rollout aborted",
}

// loggedDetail are the detail keys a logged event carries.
var loggedDetail = []string{keyLease, keyRollouts, keyKey, keyTitle, keyHost, keyReason}

// logEvents logs the committed audit entries of loggedEvents, one line each.
func (s *service) logEvents(entries []fleet.AuditEntry) {
	for i := range entries {
		e := &entries[i]

		msg, ok := loggedEvents[e.Action]
		if !ok {
			continue
		}

		fields := logrus.Fields{"target": e.Target, "actor": e.Actor}

		for _, k := range loggedDetail {
			if v, ok := e.Detail[k]; ok && v != "" {
				fields[k] = v
			}
		}

		s.log.WithFields(fields).Info(msg)
	}
}

// loadAborted gives the world the keys of the aborted rollouts and the
// aborted rollouts that an open lease still serves.
func (s *service) loadAborted(ctx context.Context, w *World) error {
	keys, err := s.opts.Store.AbortedKeys(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: aborted keys: %w", err)
	}

	w.AbortedKeys = make(map[string]bool, len(keys))
	for _, k := range keys {
		w.AbortedKeys[k] = true
	}

	known := make(map[string]bool, len(w.Rollouts))
	for _, r := range w.Rollouts {
		known[r.ID] = true
	}

	for _, l := range w.Leases {
		for _, id := range l.Rollouts {
			if known[id] {
				continue
			}

			known[id] = true

			r, err := s.opts.Store.GetRollout(ctx, id)

			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return fmt.Errorf("scheduler: get rollout %s: %w", id, err)
			case r.State == fleet.RolloutAborted:
				w.Rollouts = append(w.Rollouts, r)
			}
		}
	}

	return nil
}

// loadDirectives gives the world the pending directives and every directive
// of the open manual rollouts, answered or not, oldest first.
func (s *service) loadDirectives(ctx context.Context, w *World) error {
	st := s.opts.Store

	found, err := st.ListDirectives(ctx, store.DirectiveFilter{PendingOnly: true})
	if err != nil {
		return fmt.Errorf("scheduler: list pending directives: %w", err)
	}

	manual := make([]string, 0, len(w.Rollouts))

	for _, r := range w.Rollouts {
		if r.Directed() {
			manual = append(manual, r.ID)
		}
	}

	if len(manual) > 0 {
		theirs, err := st.ListDirectives(ctx, store.DirectiveFilter{Rollouts: manual})
		if err != nil {
			return fmt.Errorf("scheduler: list directives of the manual rollouts: %w", err)
		}

		for _, d := range theirs {
			if d.State != fleet.DirectivePending {
				found = append(found, d)
			}
		}
	}

	slices.SortFunc(found, func(a, b *fleet.Directive) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})

	w.Directives = found

	return nil
}

// Assignment returns the scheduler's part of a sync response for the host
// from committed state only.
func (s *service) Assignment(ctx context.Context, host string) (*Assignment, error) {
	st := s.opts.Store
	a := &Assignment{}

	leases, err := st.ListLeases(ctx, store.LeaseFilter{OpenOnly: true, Host: host})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list leases of %s: %w", host, err)
	}

	if len(leases) > 0 {
		if a.Lease, err = s.offer(ctx, leases[0]); err != nil {
			return nil, err
		}
	}

	directives, err := st.ListDirectives(ctx, store.DirectiveFilter{Host: host, PendingOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list directives of %s: %w", host, err)
	}

	for _, d := range directives {
		a.Directives = append(a.Directives, d.Directive)
	}

	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}

	a.LookupInterval = settings.LookupInterval.Std()

	if len(leases) == 0 {
		// One row by the index on (host, granted_at): the newest lease.
		last, err := st.ListLeases(ctx, store.LeaseFilter{Host: host, Limit: 1})
		if err != nil {
			return nil, fmt.Errorf("scheduler: newest lease of %s: %w", host, err)
		}

		if len(last) > 0 && last[0].State.Terminal() {
			a.Ended = &protocol.EndedLease{
				ID: last[0].ID, State: string(last[0].State), Reason: last[0].Error,
			}
		}

		for _, q := range s.Plan().Queue {
			if q.Host == host {
				a.Queue = &protocol.Queue{Position: q.Position, Reason: q.Reason}

				break
			}
		}
	}

	return a, nil
}

// offer is a lease as a sync response states it, or nil for a lease that is
// not offered.
//
// A lease that a person ended before the agent reported an operation for it
// is no longer offered: an agent that has not started it must not, and the
// agent that holds it unstarted declines it when it is no longer offered. One
// that reports an operation for it has it, whatever the scheduler has made of
// that report so far: to it the lease is restated, with the mark, so that it
// keeps a finished operation until the scheduler has taken it in.
func (s *service) offer(ctx context.Context, l *fleet.Lease) (*protocol.Lease, error) {
	if l.Recheck {
		return nil, nil //nolint:nilnil // a recheck is the controller's alone.
	}

	if l.Cancelled != nil && l.State == fleet.LeaseGranted {
		reported, err := s.reportsOperation(ctx, l.Host, l.ID)
		if err != nil || !reported {
			return nil, err
		}
	}

	offer := &protocol.Lease{
		ID:         l.ID,
		GrantedAt:  l.GrantedAt,
		ExpiresAt:  l.ExpiresAt,
		TTLSeconds: max(int64(l.ExpiresAt.Sub(s.opts.Now())/time.Second), 0),
		Needs:      append([]protocol.Need{}, l.Needs...),
		Cancel:     l.Cancelled != nil,
	}

	return offer, nil
}

// reportsOperation reports whether the host's last sync carries an operation
// for the lease.
func (s *service) reportsOperation(ctx context.Context, host, lease string) (bool, error) {
	h, err := s.opts.Store.GetHost(ctx, host)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("scheduler: get host %s: %w", host, err)
	}

	return h.Status != nil && h.Status.Operation != nil && h.Status.Operation.LeaseID == lease, nil
}

// Settings returns the effective settings.
func (s *service) Settings(ctx context.Context) (fleet.Settings, error) {
	settings, err := s.opts.Store.LoadSettings(ctx, s.opts.Defaults)
	if err != nil {
		return fleet.Settings{}, fmt.Errorf("scheduler: load settings: %w", err)
	}

	return settings, nil
}

// PatchSettings changes the settings in one step under the lock.
func (s *service) PatchSettings(
	ctx context.Context, actor string, apply func(settings *fleet.Settings) error,
) (fleet.Settings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.Settings(ctx)
	if err != nil {
		return fleet.Settings{}, false, err
	}

	next := current
	next.PolicyClients = current.PolicyClients.Clone()
	next.PolicyKinds = maps.Clone(current.PolicyKinds)
	next.ActionPolicies = maps.Clone(current.ActionPolicies)

	if err := apply(&next); err != nil {
		return current, false, err
	}

	if err := next.Validate(); err != nil {
		return current, false, fmt.Errorf("%w: %w", ErrInvalidAction, err)
	}

	// Nothing named, or what is named is what is there: no write, no entry.
	if len(next.PolicyClients) == 0 {
		next.PolicyClients = nil
	}

	if len(next.PolicyKinds) == 0 {
		next.PolicyKinds = nil
	}

	if len(next.ActionPolicies) == 0 {
		next.ActionPolicies = nil
	}

	if next.Equal(&current) {
		return current, false, nil
	}

	if err := s.saveSettings(ctx, actor, "settings.update", &next, ""); err != nil {
		return current, false, err
	}

	s.Nudge()

	return next, true, nil
}

// saveSettings stores settings and audits the change with before and after.
// The caller holds mu.
func (s *service) saveSettings(
	ctx context.Context, actor, action string, settings *fleet.Settings, reason string,
) error {
	before, err := s.Settings(ctx)
	if err != nil {
		return err
	}

	if err := s.opts.Store.SaveSettings(ctx, s.opts.Defaults, *settings); err != nil {
		return fmt.Errorf("scheduler: save settings: %w", err)
	}

	detail := map[string]any{"before": before, "after": *settings}
	if reason != "" {
		detail[keyReason] = reason
	}

	if err := s.appendAudit(ctx, s.entry(actor, action, "settings", detail)); err != nil {
		// The store has no transaction for settings plus audit. A change
		// that could not be audited is taken back.
		if undo := s.opts.Store.SaveSettings(ctx, s.opts.Defaults, before); undo != nil {
			return errors.Join(err, fmt.Errorf("scheduler: restore settings: %w", undo))
		}

		return err
	}

	return nil
}

// actorName is the name an actor is recorded under. The scheduler's own name
// is reserved: a person or token that carries it is recorded as
// "user:scheduler", so nothing a person does can pass for a decision of the
// scheduler.
func actorName(actor string) string {
	switch actor {
	case "":
		return "anonymous"
	case actorScheduler:
		return "user:" + actor
	default:
		return actor
	}
}

// entry builds an audit entry for an action.
func (s *service) entry(actor, action, target string, detail map[string]any) fleet.AuditEntry {
	return fleet.AuditEntry{
		Time: s.opts.Now(), Actor: actorName(actor), Action: action, Target: target, Detail: detail,
	}
}

func (s *service) appendAudit(ctx context.Context, entries ...fleet.AuditEntry) error {
	stampAuthorization(ctx, entries)

	if err := s.opts.Store.AppendAudit(ctx, entries...); err != nil {
		return fmt.Errorf("scheduler: append audit: %w", err)
	}

	s.changed()

	return nil
}
