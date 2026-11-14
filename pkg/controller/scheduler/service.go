package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	// closedRolloutHistory is how many of the newest rollouts are looked at
	// for the "aborted stays closed" rule and the failed-host carry-over.
	closedRolloutHistory = 500
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

	// mu serialises reconciles and actions.
	mu sync.Mutex

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
		if err := s.opts.Store.Commit(ctx, changes); err != nil {
			return fmt.Errorf("scheduler: commit: %w", err)
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

	recent, err := st.ListRollouts(ctx, store.RolloutFilter{Limit: closedRolloutHistory})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list recent rollouts: %w", err)
	}

	for _, r := range recent {
		if r.State.Terminal() {
			w.Closed = append(w.Closed, r)
		}
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

// loadDirectives keeps the pending directives and those of open rollouts.
func (s *service) loadDirectives(ctx context.Context, w *World) error {
	all, err := s.opts.Store.ListDirectives(ctx, store.DirectiveFilter{})
	if err != nil {
		return fmt.Errorf("scheduler: list directives: %w", err)
	}

	open := make(map[string]bool, len(w.Rollouts))
	for _, r := range w.Rollouts {
		open[r.ID] = true
	}

	for _, d := range all {
		if d.State == fleet.DirectivePending || open[d.RolloutID] {
			w.Directives = append(w.Directives, d)
		}
	}

	return nil
}

// loadClosedLeases finds out which of the leases that hosts report an
// operation for are known and closed; the others are adopted.
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

		_, err := s.opts.Store.GetLease(ctx, id)

		switch {
		case err == nil:
			if w.ClosedLeases == nil {
				w.ClosedLeases = make(map[string]bool, 2)
			}

			w.ClosedLeases[id] = true
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

// entry builds an audit entry for an action.
func (s *service) entry(actor, action, target string, detail map[string]any) fleet.AuditEntry {
	if actor == "" {
		actor = "anonymous"
	}

	return fleet.AuditEntry{
		Time: s.opts.Now(), Actor: actor, Action: action, Target: target, Detail: detail,
	}
}

func (s *service) appendAudit(ctx context.Context, entries ...fleet.AuditEntry) error {
	if err := s.opts.Store.AppendAudit(ctx, entries...); err != nil {
		return fmt.Errorf("scheduler: append audit: %w", err)
	}

	return nil
}
