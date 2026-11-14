package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Deps are the host-facing parts of the agent.
type Deps struct {
	// Runtime, Registry and System are required.
	Runtime  ContainerRuntime
	Registry Registry
	System   System
	// Prober checks the local nodes; nil reports no node health.
	Prober Prober
	// Now is the clock (default time.Now).
	Now func() time.Time
	// HTTPClient talks to the controller (default: a client with a 30s
	// timeout).
	HTTPClient *http.Client
	// Version is the agent version reported to the controller (default
	// "dev").
	Version string
}

// service is the agent: a sync loop, an image watcher and, while a lease is
// open, one operation.
type service struct {
	log     logrus.FieldLogger
	cfg     *Config
	deps    Deps
	now     func() time.Time
	version string

	client  *controllerClient
	holds   *holds
	watcher *imageWatcher
	// store is opened by Start.
	store *stateStore

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// kick asks the loop for a sync now (an operation changed phase).
	kick chan struct{}
	// inventoryTimeout bounds one System.Inventory call.
	inventoryTimeout time.Duration
	// opActive is true while the operation goroutine runs.
	opActive atomic.Bool

	// The fields below belong to the loop goroutine.

	// lastContainers and lastSys are the last good observations.
	lastContainers []protocol.Container
	lastSys        *SystemStatus
	// repeated are the directive IDs of the last response.
	repeated     []string
	pendingRolls map[string]pendingRoll
	// lastQueue and ignoredLease only keep the log quiet.
	lastQueue    protocol.Queue
	ignoredLease string
	syncFailing  bool
}

var _ Service = (*service)(nil)

// New returns the agent. Start opens the state directory and begins to
// observe and sync.
//
//nolint:gocritic // hugeParam: Deps is passed by value once, at construction
func New(
	log logrus.FieldLogger, cfg *Config, creds *identity.Credentials, deps Deps,
) (Service, error) {
	switch {
	case log == nil:
		return nil, errors.New("agent: no logger")
	case cfg == nil:
		return nil, errors.New("agent: no configuration")
	case creds == nil:
		return nil, errors.New("agent: no credentials")
	case deps.Runtime == nil || deps.Registry == nil || deps.System == nil:
		return nil, errors.New("agent: container runtime, registry and system are required")
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("agent: configuration: %w", err)
	}

	if cfg.StateDir == "" || cfg.Controller.SyncInterval <= 0 ||
		cfg.Controller.LeaseSyncInterval <= 0 || cfg.Registry.PollInterval <= 0 ||
		cfg.Verify.Window <= 0 || cfg.Verify.Timeout <= 0 {
		return nil, errors.New("agent: configuration without defaults (call ApplyDefaults)")
	}

	if deps.Now == nil {
		deps.Now = time.Now
	}

	if deps.Version == "" {
		deps.Version = "dev"
	}

	log = log.WithField("package", "agent")

	if claims, err := identity.UnverifiedClaims(creds.Certificate()); err == nil {
		log = log.WithFields(logrus.Fields{"host": claims.Subject, "network": claims.Network})
	}

	return &service{
		log:     log,
		cfg:     cfg,
		deps:    deps,
		now:     deps.Now,
		version: deps.Version,
		client:  newControllerClient(cfg.Controller.URL, creds, deps.HTTPClient, deps.Now),
		holds:   newHolds(cfg),
		// Without the roll verb the tags are still looked up, for the report,
		// but nothing is pulled: this host never rolls, and a pulled image
		// would be picked up by whoever recreates the container next.
		watcher: newImageWatcher(
			log, deps.Runtime, deps.Registry, cfg.Registry.PollInterval, deps.Now,
			cfg.Allows(protocol.VerbRoll)),
		kick:             make(chan struct{}, 1),
		inventoryTimeout: inventoryTimeout,
		pendingRolls:     make(map[string]pendingRoll, 4),
	}, nil
}

// Start opens the state file and starts the sync loop and the image
// watcher. An operation found in the state file is continued. The loop and
// the watcher stop when ctx ends or Stop is called; an operation first
// finishes its disruptive steps (see Stop, which waits for it).
func (s *service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("agent: already started")
	}

	store, err := openStateStore(s.log, s.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	s.store = store
	s.started = true

	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.log.WithFields(logrus.Fields{
		"controller": s.cfg.Controller.URL,
		"verbs":      s.cfg.Verbs,
		"containers": len(s.cfg.Containers),
		"version":    s.version,
	}).Info("agent started")

	s.wg.Go(func() { s.watcher.run(runCtx) })
	s.wg.Go(func() { s.loop(runCtx) })

	return nil
}

// Stop ends the loop and the watcher and waits for them and for a running
// operation.
//
// An operation is not cut short in its draining and applying phases: a
// recreate or a package upgrade in flight, the steps after it and the reboot
// are carried through first, each bounded by its own timeout, and Stop
// returns only then. That can take as long as the stop timeouts of the
// containers plus a package upgrade, so the unit's TimeoutStopSec should
// allow for it; a process killed before that resumes the operation as
// failed. An operation that is waiting (for the reboot, for containers to
// start or to stay up) ends at once, stays in the state file and is
// continued by the next Start.
func (s *service) Stop() error {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel == nil {
		return nil
	}

	cancel()

	if s.opActive.Load() {
		s.log.Info("stopping: waiting for the open operation to finish its disruptive steps")
	}

	s.wg.Wait()

	s.log.Info("agent stopped")

	return nil
}

// loop syncs until ctx ends: every SyncInterval when idle, every
// LeaseSyncInterval while a lease is open, and at once when the operation
// changes phase or a response brought something to answer.
func (s *service) loop(ctx context.Context) {
	for ctx.Err() == nil {
		s.ensureOperation(ctx)

		soon := s.cycle(ctx)
		wait := s.interval()

		if soon {
			wait = min(wait, s.cfg.Controller.LeaseSyncInterval, time.Second)
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
		case <-s.kick:
		case <-timer.C:
		}

		timer.Stop()
	}
}

// interval is the time to the next regular sync.
func (s *service) interval() time.Duration {
	if s.store.Snapshot().Current != nil || len(s.pendingRolls) > 0 {
		return s.cfg.Controller.LeaseSyncInterval
	}

	return s.cfg.Controller.SyncInterval
}

// cycle observes the host and syncs once. It reports whether another sync
// should follow soon (a directive to acknowledge, a lease taken).
func (s *service) cycle(ctx context.Context) bool {
	obs := s.observe(ctx)
	if ctx.Err() != nil {
		return false
	}

	s.resolvePendingRolls(obs)
	s.reconcileManual(obs)

	st := s.store.Snapshot()
	req := s.buildRequest(obs, &st)

	resp, err := s.client.Sync(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}

		// The controller being away is normal; the next tick tries again.
		if !s.syncFailing {
			s.log.WithError(err).Warn("sync failed; retrying on every tick")
		} else {
			s.log.WithError(err).Debug("sync failed")
		}

		s.syncFailing = true

		return false
	}

	if s.syncFailing {
		s.log.Info("sync works again")

		s.syncFailing = false
	}

	s.logQueue(resp.Queue)

	soon := s.handleDirectives(resp.Directives, obs)

	if s.handleLease(ctx, resp.Lease, req.Operation) {
		soon = true
	}

	if resp.WantInventory {
		s.sendInventory(ctx)
	}

	return soon
}

// observe looks at the host. A part that cannot be read is logged and the
// last good value is reported instead.
func (s *service) observe(ctx context.Context) *observation {
	obs := &observation{hold: s.holds.Read()}

	cctx, cancel := context.WithTimeout(ctx, observeTimeout)
	containers, err := s.deps.Runtime.Observe(cctx)

	cancel()

	if err != nil {
		s.log.WithError(err).Warn("cannot list containers; reporting the last known state")

		containers = slices.Clone(s.lastContainers)
	} else {
		obs.containersFresh = true
		s.lastContainers = slices.Clone(containers)
	}

	tags := enrichContainers(s.cfg, containers, s.watcher.Check)
	obs.containers = containers

	if obs.containersFresh {
		s.watcher.Track(tags)
	}

	if s.deps.Prober != nil {
		obs.node = s.deps.Prober.Probe(ctx)
	}

	cctx, cancel = context.WithTimeout(ctx, statusTimeout)
	sys, err := s.deps.System.Status(cctx)

	cancel()

	switch {
	case err != nil:
		s.log.WithError(err).Warn("cannot read the system state; reporting the last known state")
	case sys != nil:
		s.lastSys = sys
	}

	obs.sys = s.lastSys

	return obs
}

// reconcileManual drops manual needs that stopped qualifying and lets
// manual image needs follow their tag.
func (s *service) reconcileManual(obs *observation) {
	st := s.store.Snapshot()
	if len(st.ManualNeeds) == 0 {
		return
	}

	kept, dropped, changed := reconcileManualNeeds(s.cfg, obs, st.ManualNeeds)
	if !changed {
		return
	}

	// The operation goroutine may close manual needs at the same time, so
	// the change is applied to whatever is stored now.
	err := s.store.Update(func(st *persistedState) {
		next := make([]protocol.Need, 0, len(st.ManualNeeds))

		for _, n := range st.ManualNeeds {
			if _, drop := dropped[n.ID]; drop {
				continue
			}

			if i := slices.IndexFunc(kept, func(k protocol.Need) bool { return k.ID == n.ID }); i >= 0 {
				n = kept[i]
			}

			next = append(next, n)
		}

		st.ManualNeeds = next
	})
	if err != nil {
		s.log.WithError(err).Error("cannot persist the manual needs")

		return
	}

	for id, reason := range dropped {
		s.log.WithFields(logrus.Fields{"need": id, "reason": reason}).
			Warn("manual need dropped")
	}
}

// buildRequest restates the complete state of the host.
func (s *service) buildRequest(obs *observation, st *persistedState) *protocol.SyncRequest {
	req := &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    s.version,
		SentAt:          s.now().UTC(),
		Verbs:           append([]protocol.Verb{}, s.cfg.Verbs...),
		Hold:            obs.hold,
		Containers:      append([]protocol.Container{}, obs.containers...),
		Node:            obs.node,
		Needs:           computeNeeds(s.cfg, obs, st.ManualNeeds),
		DirectiveAcks:   s.directiveAcks(st),
	}

	if sys := obs.sys; sys != nil {
		req.BootID = sys.BootID
		req.UptimeSeconds = int64(sys.Uptime / time.Second)
		req.OS = sys.OS
		req.Pending = sys.Pending
		req.InventoryHash = sys.InventoryHash
	}

	// A lease that is persisted but not yet decided on is not an operation.
	if cur := st.Current; cur != nil && (cur.Started || cur.Operation.Phase.Terminal()) {
		op := cur.Operation
		req.Operation = &op
	}

	return req
}

// handleLease reacts to the lease of a sync response (or its absence).
// reported is the operation the request of that response carried. It
// reports whether a new lease was taken.
func (s *service) handleLease(
	ctx context.Context, lease *protocol.Lease, reported *protocol.Operation,
) bool {
	if cur := s.store.Snapshot().Current; cur != nil {
		if lease != nil && lease.ID == cur.Lease.ID {
			return false
		}

		if !cur.Operation.Phase.Terminal() {
			// The controller no longer holds the lease, and nothing was
			// touched for it yet: it must not be started any more.
			if !cur.Started {
				s.withdrawLease(cur.Lease.ID)

				return false
			}

			// Once started, finish; a new lease has to wait.
			if lease != nil && s.ignoredLease != lease.ID {
				s.ignoredLease = lease.ID

				s.log.WithFields(logrus.Fields{"lease": lease.ID, "current": cur.Lease.ID}).
					Warn("another lease offered while an operation is open; finishing first")
			}

			return false
		}

		// The controller no longer restates the lease. Forget the terminal
		// operation, but only once the controller has been told about it.
		if reported == nil || reported.LeaseID != cur.Lease.ID || !reported.Phase.Terminal() {
			return false
		}

		err := s.store.Update(func(st *persistedState) { st.Current = nil })
		if err != nil {
			s.log.WithError(err).Error("cannot drop the finished operation")

			return false
		}

		s.log.WithFields(logrus.Fields{
			"lease": cur.Lease.ID, "phase": cur.Operation.Phase,
		}).Info("lease released by the controller; operation forgotten")
	}

	if lease == nil {
		return false
	}

	return s.acceptLease(ctx, lease)
}

// withdrawLease marks the persisted lease as one the controller no longer
// holds, unless the operation has started in the meantime. The operation
// declines a withdrawn lease instead of starting it; the refusal is reported,
// forgotten, and only then is another lease taken.
func (s *service) withdrawLease(id string) {
	withdrawn := false

	err := s.store.Update(func(st *persistedState) {
		cur := st.Current
		if cur == nil || cur.Lease.ID != id || cur.Started || cur.Withdrawn {
			return
		}

		cur.Withdrawn = true
		withdrawn = true
	})
	if err != nil {
		s.log.WithError(err).WithField("lease", id).Error("cannot persist that the lease was withdrawn")

		return
	}

	if withdrawn {
		s.log.WithField("lease", id).
			Warn("the controller no longer offers the lease; it will not be started")
	}
}

// acceptLease persists a new lease with its deadline and only then starts
// the operation.
func (s *service) acceptLease(ctx context.Context, lease *protocol.Lease) bool {
	log := s.log.WithField("lease", lease.ID)

	if lease.ID == "" {
		log.Warn("ignoring a lease without an ID")

		return false
	}

	now := s.now()
	rec := &operationRecord{
		Lease:      *lease,
		ReceivedAt: now,
		Deadline:   leaseDeadline(lease, now),
		Operation: protocol.Operation{
			LeaseID: lease.ID,
			Steps:   make([]protocol.Step, 0, len(lease.Needs)),
		},
	}
	rec.Lease.Needs = cloneNeeds(lease.Needs)

	for i := range lease.Needs {
		rec.Operation.Steps = append(rec.Operation.Steps, protocol.Step{
			NeedID: lease.Needs[i].ID, Status: protocol.StepPending,
		})
	}

	if err := s.store.Update(func(st *persistedState) { st.Current = rec }); err != nil {
		// Not persisted, not acted on. The controller restates the lease.
		log.WithError(err).Error("cannot persist the lease; not acting on it")

		return false
	}

	log.WithFields(logrus.Fields{
		"needs":    len(lease.Needs),
		"deadline": rec.Deadline.UTC().Format(time.RFC3339),
	}).Info("lease received")

	s.ensureOperation(ctx)

	return true
}

// leaseDeadline is the earlier of the lease's absolute expiry and its
// remaining lifetime counted from the receipt, which tolerates a clock that
// is off in either direction.
func leaseDeadline(lease *protocol.Lease, received time.Time) time.Time {
	deadline := received.Add(time.Duration(max(lease.TTLSeconds, 0)) * time.Second)

	if !lease.ExpiresAt.IsZero() && lease.ExpiresAt.Before(deadline) {
		deadline = lease.ExpiresAt
	}

	return deadline
}

// ensureOperation starts the operation goroutine when the state file holds
// a lease that is not terminal and nothing works on it.
func (s *service) ensureOperation(ctx context.Context) {
	cur := s.store.Snapshot().Current
	if cur == nil || cur.Operation.Phase.Terminal() || ctx.Err() != nil {
		return
	}

	if !s.opActive.CompareAndSwap(false, true) {
		return
	}

	op := &operation{
		log:         s.log.WithField("component", "operation"),
		cfg:         s.cfg,
		runtime:     s.deps.Runtime,
		registry:    s.deps.Registry,
		system:      s.deps.System,
		holds:       s.holds,
		store:       s.store,
		now:         s.now,
		poll:        s.pollInterval(),
		rebootGrace: defaultRebootGrace,
		notify:      s.requestSync,
	}

	s.wg.Go(func() {
		defer s.opActive.Store(false)

		op.run(ctx)
	})
}

// pollInterval is the pause between looks while an operation waits.
func (s *service) pollInterval() time.Duration {
	return min(max(s.cfg.Verify.Window/5, 5*time.Millisecond), time.Second)
}

// requestSync asks the loop for a sync now.
func (s *service) requestSync() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// sendInventory uploads the package inventory the controller asked for.
func (s *service) sendInventory(ctx context.Context) {
	// A package query that hangs must not stall the sync loop.
	cctx, cancel := context.WithTimeout(ctx, s.inventoryTimeout)
	inv, err := s.deps.System.Inventory(cctx)

	cancel()

	switch {
	case err != nil:
		s.log.WithError(err).Warn("cannot collect the inventory")

		return
	case inv == nil:
		s.log.Warn("the system returned no inventory; nothing uploaded")

		return
	}

	if err := s.client.Inventory(ctx, inv); err != nil {
		s.log.WithError(err).Warn("inventory upload failed")

		return
	}

	s.log.WithFields(logrus.Fields{
		"hash": inv.Hash, "packages": len(inv.Packages), "upgradable": len(inv.Upgradable),
	}).Info("inventory uploaded")
}

// logQueue logs the queue position and waiting reason when they change.
func (s *service) logQueue(q *protocol.Queue) {
	var cur protocol.Queue
	if q != nil {
		cur = *q
	}

	if cur == s.lastQueue {
		return
	}

	s.lastQueue = cur

	if q == nil {
		s.log.Info("no longer queued")

		return
	}

	s.log.WithFields(logrus.Fields{"position": q.Position, "reason": q.Reason}).
		Info("waiting for a slot")
}
