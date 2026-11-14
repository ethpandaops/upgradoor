package scheduler

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// defaultTickInterval is the reconcile period.
	defaultTickInterval = 2 * time.Second
	// minLivenessEpochs is the least number of liveness epochs loaded.
	minLivenessEpochs = 6
)

// Store is the part of store.Store the scheduler uses.
type Store interface {
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	SetHostHold(ctx context.Context, name string, wanted bool, reason string) error
	DeleteHost(ctx context.Context, name string) error
	ListLiveness(ctx context.Context, epochs int) ([]fleet.Liveness, error)
	ListRollouts(ctx context.Context, filter store.RolloutFilter) ([]*fleet.Rollout, error)
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
	ListLeases(ctx context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error)
	GetLease(ctx context.Context, id string) (*fleet.Lease, error)
	ListDirectives(ctx context.Context, filter store.DirectiveFilter) ([]*fleet.Directive, error)
	Commit(ctx context.Context, changes *store.Changes) error
	LoadSettings(ctx context.Context, defaults fleet.Settings) (fleet.Settings, error)
	SaveSettings(ctx context.Context, defaults, settings fleet.Settings) error
	AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error
}

var _ Store = store.Store(nil)

// Options configure the scheduler.
type Options struct {
	// Store holds all state. Required.
	Store Store
	// Health returns the beacon service's latest view. Without it the
	// network gate fails ("beacon node unreachable").
	Health func() fleet.NetworkHealth
	// Defaults are the settings before the stored overrides.
	Defaults fleet.Settings
	// TickInterval is the reconcile period; 2s when zero.
	TickInterval time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// NewID returns a new unique ID; random when nil.
	NewID func() string
}

type service struct {
	log  logrus.FieldLogger
	opts Options

	// mu serialises reconciles and actions and guards hist.
	mu   sync.Mutex
	hist history

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

	return &service{
		log:   log.WithField("package", "scheduler"),
		opts:  opts,
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
		}
	}
}

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

	s.planMu.Lock()
	s.plan = out.Plan
	s.planMu.Unlock()

	return nil
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
		Settings: settings,
		Health:   s.opts.Health(),
		NewID:    s.opts.NewID,
	}

	if w.Hosts, err = st.ListHosts(ctx); err != nil {
		return nil, fmt.Errorf("scheduler: list hosts: %w", err)
	}

	epochs := max(int(settings.SoakEpochs)+3, minLivenessEpochs)
	if w.Liveness, err = st.ListLiveness(ctx, epochs); err != nil {
		return nil, fmt.Errorf("scheduler: list liveness: %w", err)
	}

	if w.Rollouts, err = st.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true}); err != nil {
		return nil, fmt.Errorf("scheduler: list open rollouts: %w", err)
	}

	if w.Closed, err = s.closedRollouts(ctx); err != nil {
		return nil, err
	}

	if w.Leases, err = st.ListLeases(ctx, store.LeaseFilter{OpenOnly: true}); err != nil {
		return nil, fmt.Errorf("scheduler: list open leases: %w", err)
	}

	if err := s.loadDirectives(ctx, w); err != nil {
		return nil, err
	}

	if err := s.loadClosedLeases(ctx, w); err != nil {
		return nil, err
	}

	return w, nil
}

// history is what the scheduler remembers of the rows that only it writes
// and that it would otherwise read in full on every tick: the latest closed
// rollout of every key and the answered directives of the open manual
// rollouts. It is read from the store once (after a start and after a failed
// write) and then follows every write. The caller holds service.mu.
type history struct {
	loaded bool
	// closed is the latest closed rollout per key. Manual rollouts are left
	// out: nothing follows them.
	closed map[string]*fleet.Rollout
	// settled holds, per open manual rollout, its directives that are no
	// longer pending, by ID. They never change again.
	settled map[string]map[string]*fleet.Directive
}

// newerRollout orders rollouts by creation, then end, then ID.
func newerRollout(a, b *fleet.Rollout) int {
	var endA, endB time.Time

	if a.EndedAt != nil {
		endA = *a.EndedAt
	}

	if b.EndedAt != nil {
		endB = *b.EndedAt
	}

	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), endA.Compare(endB), cmp.Compare(a.ID, b.ID))
}

// closedRollouts returns the latest closed rollout of every key, newest
// first. The caller holds mu.
func (s *service) closedRollouts(ctx context.Context) ([]*fleet.Rollout, error) {
	if err := s.loadHistory(ctx); err != nil {
		return nil, err
	}

	out := make([]*fleet.Rollout, 0, len(s.hist.closed))
	for _, r := range s.hist.closed {
		out = append(out, r)
	}

	slices.SortFunc(out, func(a, b *fleet.Rollout) int { return newerRollout(b, a) })

	return out, nil
}

// noteRollout takes in a rollout that was read or written.
func (h *history) noteRollout(r *fleet.Rollout) {
	if !r.State.Terminal() {
		return
	}

	delete(h.settled, r.ID)

	if r.Kind == fleet.RolloutManual {
		return
	}

	if cur := h.closed[r.Key]; cur == nil || newerRollout(r, cur) >= 0 {
		h.closed[r.Key] = cloneRollout(r)
	}
}

// noteDirective takes in an answered directive of an open manual rollout.
func (h *history) noteDirective(d *fleet.Directive) {
	byID, ok := h.settled[d.RolloutID]
	if !ok || d.State == fleet.DirectivePending {
		return
	}

	c := *d
	byID[d.ID] = &c
}

// follow takes in a committed write.
func (h *history) follow(changes *store.Changes) {
	if !h.loaded {
		return
	}

	for _, r := range changes.Rollouts {
		h.noteRollout(r)
	}

	for _, d := range changes.Directives {
		h.noteDirective(d)
	}
}

// write commits changes and keeps the history in step. After a failed write
// the history is read again: the store may or may not have applied it.
func (s *service) write(ctx context.Context, changes *store.Changes) error {
	if err := s.opts.Store.Commit(ctx, changes); err != nil {
		s.hist = history{}

		return fmt.Errorf("scheduler: commit: %w", err)
	}

	s.hist.follow(changes)

	return nil
}

// loadHistory reads the closed rollouts once. The caller holds mu.
func (s *service) loadHistory(ctx context.Context) error {
	if s.hist.loaded {
		return nil
	}

	all, err := s.opts.Store.ListRollouts(ctx, store.RolloutFilter{})
	if err != nil {
		return fmt.Errorf("scheduler: list rollouts: %w", err)
	}

	s.hist = history{
		loaded:  true,
		closed:  make(map[string]*fleet.Rollout, len(all)),
		settled: make(map[string]map[string]*fleet.Directive, 2),
	}

	for _, r := range all {
		s.hist.noteRollout(r)
	}

	return nil
}

// loadDirectives gives the world the pending directives and the answered
// ones of the open manual rollouts, oldest first. Only the pending ones are
// read on every tick; the answered ones of a manual rollout are read once,
// when the rollout is first seen. The caller holds mu and has loaded the
// history.
func (s *service) loadDirectives(ctx context.Context, w *World) error {
	unread := false
	open := make(map[string]bool, len(w.Rollouts))

	for _, r := range w.Rollouts {
		if r.Kind != fleet.RolloutManual || r.State.Terminal() {
			continue
		}

		open[r.ID] = true

		if _, ok := s.hist.settled[r.ID]; !ok {
			s.hist.settled[r.ID] = make(map[string]*fleet.Directive, len(r.Hosts))
			unread = true
		}
	}

	for id := range s.hist.settled {
		if !open[id] {
			delete(s.hist.settled, id)
		}
	}

	found, err := s.opts.Store.ListDirectives(ctx, store.DirectiveFilter{PendingOnly: !unread})
	if err != nil {
		s.hist = history{}

		return fmt.Errorf("scheduler: list directives: %w", err)
	}

	for _, d := range found {
		if d.State == fleet.DirectivePending {
			w.Directives = append(w.Directives, d)

			continue
		}

		s.hist.noteDirective(d)
	}

	for _, byID := range s.hist.settled {
		for _, d := range byID {
			c := *d
			w.Directives = append(w.Directives, &c)
		}
	}

	slices.SortFunc(w.Directives, func(a, b *fleet.Directive) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})

	return nil
}

// loadClosedLeases looks up the leases that hosts report an operation for
// and that are not open: the known ones are closed, the others are adopted.
func (s *service) loadClosedLeases(ctx context.Context, w *World) error {
	open := make(map[string]bool, len(w.Leases))
	for _, l := range w.Leases {
		open[l.ID] = true
	}

	for _, h := range w.Hosts {
		if h.Status == nil || h.Status.Operation == nil {
			continue
		}

		id := h.Status.Operation.LeaseID
		if id == "" || open[id] {
			continue
		}

		l, err := s.opts.Store.GetLease(ctx, id)

		switch {
		case err == nil:
			if w.ClosedLeases == nil {
				w.ClosedLeases = make(map[string]*fleet.Lease, 2)
			}

			w.ClosedLeases[id] = l
		case errors.Is(err, store.ErrNotFound):
		default:
			return fmt.Errorf("scheduler: get lease %s: %w", id, err)
		}
	}

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

	now := s.opts.Now()

	for _, l := range leases {
		if l.State.Terminal() || l.Host != host {
			continue
		}

		a.Lease = &protocol.Lease{
			ID:         l.ID,
			GrantedAt:  l.GrantedAt,
			ExpiresAt:  l.ExpiresAt,
			TTLSeconds: max(int64(l.ExpiresAt.Sub(now)/time.Second), 0),
			Needs:      []protocol.Need{},
		}

		// An adopted lease is restated without needs. The agent that still
		// works on it knows the lease and ignores the restatement; one that
		// finished and forgot it (it saw a response without the lease after
		// the database was lost) must not run the same needs a second time.
		// The marker is a reserved name that no actor can carry (actorName).
		if l.GrantedBy != grantedByAdoption {
			a.Lease.Needs = append(a.Lease.Needs, l.Needs...)
		}

		break
	}

	directives, err := st.ListDirectives(ctx, store.DirectiveFilter{Host: host, PendingOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list directives of %s: %w", host, err)
	}

	for _, d := range directives {
		if d.State == fleet.DirectivePending && d.Host == host {
			a.Directives = append(a.Directives, d.Directive)
		}
	}

	if a.Lease == nil {
		for _, q := range s.Plan().Queue {
			if q.Host == host {
				a.Queue = &protocol.Queue{Position: q.Position, Reason: q.Reason}

				break
			}
		}
	}

	return a, nil
}

// Settings returns the effective settings.
func (s *service) Settings(ctx context.Context) (fleet.Settings, error) {
	settings, err := s.opts.Store.LoadSettings(ctx, s.opts.Defaults)
	if err != nil {
		return fleet.Settings{}, fmt.Errorf("scheduler: load settings: %w", err)
	}

	return settings, nil
}

// UpdateSettings validates, stores and audits new settings.
func (s *service) UpdateSettings(ctx context.Context, actor string, settings fleet.Settings) error { //nolint:gocritic // hugeParam: the signature is the Service contract.
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAction, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.saveSettings(ctx, actor, "settings.update", &settings, ""); err != nil {
		return err
	}

	s.Nudge()

	return nil
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

// actorName is the name an actor is recorded under. The scheduler's own
// names ("scheduler" and everything starting with "scheduler:") are
// reserved: a person or token that carries one is recorded as "user:<name>",
// so nothing a person does can pass for a decision of the scheduler.
func actorName(actor string) string {
	switch {
	case actor == "":
		return "anonymous"
	case actor == actorScheduler || strings.HasPrefix(actor, reservedActorPrefix):
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
	if err := s.opts.Store.AppendAudit(ctx, entries...); err != nil {
		return fmt.Errorf("scheduler: append audit: %w", err)
	}

	return nil
}
