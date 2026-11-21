package agent

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/needs"
	"github.com/ethpandaops/upgradoor/pkg/agent/operation"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of the single calls of a cycle.
const (
	observeTimeout = 30 * time.Second
	statusTimeout  = 30 * time.Second
	// inventoryTimeout bounds the collection of the package inventory.
	inventoryTimeout = 2 * time.Minute
)

// loop syncs until ctx ends: when the controller's last answer asked, kept
// within MinSyncInterval and MaxSyncInterval; without such an answer every
// SyncInterval when idle and every LeaseSyncInterval while a lease is open;
// and at once when the operation changes phase or a response brought
// something to answer.
func (s *service) loop(ctx context.Context) {
	for ctx.Err() == nil {
		s.ensureOperation(ctx)

		soon := s.cycle(ctx)
		wait := s.client.Interval(s.busy())

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

// busy reports whether a lease or a roll is open: the loop then syncs at the
// pace of a lease until the controller says otherwise.
func (s *service) busy() bool {
	return s.store.Snapshot().Current != nil || s.directives.Pending() || s.local.RollsOpen()
}

// cycle observes the host and syncs once. It reports whether another sync
// should follow soon (a directive to acknowledge, a lease taken).
func (s *service) cycle(ctx context.Context) bool {
	obs := s.observe(ctx)
	if ctx.Err() != nil {
		return false
	}

	// The rolls asked for on the host since the last cycle wait for the
	// registry lookup and the pull of their image like a roll directive.
	requests := s.local.TakeRequests()

	for i := range requests {
		roll := &requests[i]
		if reason := s.directives.AwaitLocalRoll(roll.ID, roll.Container, obs); reason != "" {
			s.local.SetRoll(roll.ID, local.RollRejected, reason)
		}
	}

	s.directives.Resolve(obs)
	s.reconcileManual(obs)

	st := s.store.Snapshot()
	req := s.buildRequest(obs, &st)

	resp, err := s.client.Sync(ctx, req)
	if err != nil {
		s.local.Follow(&st, nil)
		s.local.Publish(obs, req, &st, s.lastQueue, s.syncFailing)

		if ctx.Err() != nil {
			return false
		}

		// An answer that is not the controller's is no answer: nothing of
		// it is acted on, and for everything else it is a controller that
		// cannot be reached. It is said once per episode, and louder than an
		// outage, because it is either a misconfigured key or somebody else
		// answering under the controller's name.
		refused := errors.Is(err, identity.ErrBadAnswer)

		// The controller being away is normal; the next tick tries again.
		switch {
		case refused && !s.answerRefused:
			s.log.WithError(err).Error("refused the controller's answer: it does not carry " +
				"a valid signature of a configured controller key. Acting on nothing of " +
				"it; retrying on every tick")
		case !refused && !s.syncFailing:
			s.log.WithError(err).Warn("sync failed; retrying on every tick")
		default:
			s.log.WithError(err).Debug("sync failed")
		}

		s.syncFailing = true
		s.answerRefused = refused

		// Not heard from the controller for a while: the tags are looked
		// up on the agent's own, slow schedule.
		if s.lastAnswer.IsZero() || s.now().Sub(s.lastAnswer) > s.lookupFallback {
			s.watcher.SetFallback(true)
		}

		return false
	}

	if s.syncFailing {
		s.log.Info("sync works again")

		s.syncFailing = false
		s.answerRefused = false
	}

	s.lastAnswer = s.now()

	if resp.LookupFallbackSeconds > 0 {
		s.lookupFallback = time.Duration(resp.LookupFallbackSeconds) * time.Second
	}

	// The controller coordinates the lookups: the tags this host is to look
	// up, what other hosts saw, and what this host may pull.
	s.watcher.SetFallback(false)
	s.watcher.Relay(resp.Digests)
	s.watcher.Lookup(resp.Lookups)
	s.watcher.SetWindow(resp.Pull)

	s.logQueue(resp.Queue)
	s.local.Follow(&st, resp)
	s.local.Publish(obs, req, &st, s.lastQueue, s.syncFailing)

	soon := s.directives.Handle(resp.Directives, obs)

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
func (s *service) observe(ctx context.Context) *needs.Observation {
	obs := &needs.Observation{Hold: s.overrides.Hold()}

	cctx, cancel := context.WithTimeout(ctx, observeTimeout)
	containers, err := s.deps.Runtime.Observe(cctx)

	cancel()

	if err != nil {
		s.log.WithError(err).Warn("cannot list containers; reporting the last known state")

		containers = slices.Clone(s.lastContainers)
	} else {
		obs.ContainersFresh = true
		s.lastContainers = slices.Clone(containers)
	}

	tags, targets := needs.Enrich(s.cfg, containers, s.overrides.Refresh(), s.watcher.Check)
	obs.Containers = containers
	obs.Targets = targets
	obs.Check = s.watcher.Check

	if obs.ContainersFresh {
		s.watcher.Track(tags)
	}

	if s.deps.Prober != nil {
		obs.Node = s.deps.Prober.Probe(ctx)
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

	obs.Sys = s.lastSys

	return obs
}

// reconcileManual drops manual needs that stopped qualifying and lets
// manual image needs follow their tag.
func (s *service) reconcileManual(obs *needs.Observation) {
	st := s.store.Snapshot()
	if len(st.ManualNeeds) == 0 {
		return
	}

	kept, dropped, changed := needs.Reconcile(s.cfg, obs, st.ManualNeeds)
	if !changed {
		return
	}

	// The operation goroutine may close manual needs at the same time, so
	// the change is applied to whatever is stored now.
	err := s.store.Update(func(st *journal.State) {
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
func (s *service) buildRequest(obs *needs.Observation, st *journal.State) *protocol.SyncRequest {
	req := &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    s.version,
		SentAt:          s.now().UTC(),
		Verbs:           append([]protocol.Verb{}, s.cfg.Verbs...),
		Hold:            obs.Hold,
		Containers:      slices.Clone(obs.Containers),
		Node:            obs.Node,
		Needs:           needs.Compute(s.cfg, obs, st.ManualNeeds),
		DirectiveAcks:   s.directives.Acks(st),
		Pulling:         s.watcher.Pulling(),
		Validators:      s.validators(),
	}

	if sys := obs.Sys; sys != nil {
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

// validators reads the host's validator range for a sync and logs it when it
// differs from the one reported last.
func (s *service) validators() *protocol.Validators {
	v := needs.Validators(&s.cfg.Validators)

	switch {
	case v == nil && s.lastValidators == nil:
	case v != nil && s.lastValidators != nil && *v == *s.lastValidators:
	case v == nil:
		s.log.Info("no validator range configured any more; reporting no validators")
	case v.State == protocol.ValidatorsInvalid:
		s.log.WithFields(logrus.Fields{"source": v.Source, "error": v.Error}).
			Error("cannot read the validator range; reporting it as unknown, not as none")
	case v.State == protocol.ValidatorsMissing:
		s.log.WithField("marker", s.cfg.Validators.MarkerFile).
			Warn("the validator key range marker is missing (keys being reshuffled?); " +
				"reporting the range as unknown, not as none")
	default:
		s.log.WithFields(logrus.Fields{"source": v.Source, "start": v.Start, "end": v.End}).
			Info("validator range")
	}

	s.lastValidators = v

	return v
}

// handleLease reacts to the lease of a sync response (or its absence).
// reported is the operation the request of that response carried. It
// reports whether a new lease was taken.
func (s *service) handleLease(
	ctx context.Context, lease *protocol.Lease, reported *protocol.Operation,
) bool {
	if cur := s.store.Snapshot().Current; cur != nil {
		if lease != nil && lease.ID == cur.Lease.ID {
			if lease.Cancel {
				s.cancelLease(cur.Lease.ID)
			}

			return false
		}

		if !cur.Operation.Phase.Terminal() {
			// The controller no longer offers the lease, and nothing was
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

		err := s.store.Update(func(st *journal.State) { st.Current = nil })
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

	err := s.store.Update(func(st *journal.State) {
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

// cancelLease marks the persisted lease as cancelled by a person. An
// operation that has not started declines it; one that has started begins no
// further step (operation.proceed). The mark is kept in memory even when it
// cannot be written, so that a cancel is never lost to a full disk while the
// agent runs; the controller restates it on every sync.
func (s *service) cancelLease(id string) {
	cancelled := false

	err := s.store.Apply(func(st *journal.State) {
		cur := st.Current
		if cur == nil || cur.Lease.ID != id || cur.Cancelled || cur.Operation.Phase.Terminal() {
			return
		}

		cur.Cancelled = true
		cur.Lease.Cancel = true
		cancelled = true
	})

	if !cancelled {
		return
	}

	if err != nil {
		s.log.WithError(err).WithField("lease", id).
			Error("cannot persist that the lease was cancelled; honouring it all the same")
	}

	s.log.WithField("lease", id).
		Warn("the lease was cancelled; no further step of it is begun")
}

// acceptLease persists a new lease with its deadline and only then starts
// the operation.
func (s *service) acceptLease(ctx context.Context, lease *protocol.Lease) bool {
	log := s.log.WithField("lease", lease.ID)

	if lease.ID == "" {
		log.Warn("ignoring a lease without an ID")

		return false
	}

	if lease.Cancel {
		// Nothing was done for it, and nothing will be.
		if s.ignoredLease != lease.ID {
			s.ignoredLease = lease.ID

			log.Warn("ignoring a lease that was cancelled before the agent had it")
		}

		return false
	}

	rec := &journal.Record{
		Lease:    *lease,
		Deadline: leaseDeadline(lease, s.now()),
		Operation: protocol.Operation{
			LeaseID: lease.ID,
			Steps:   make([]protocol.Step, 0, len(lease.Needs)),
		},
	}
	rec.Lease.Needs = needs.Clone(lease.Needs)

	for i := range lease.Needs {
		rec.Operation.Steps = append(rec.Operation.Steps, protocol.Step{
			NeedID: lease.Needs[i].ID, Status: protocol.StepPending,
		})
	}

	if err := s.store.Update(func(st *journal.State) { st.Current = rec }); err != nil {
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

	op := operation.New(&operation.Deps{
		Log:      s.log.WithField("component", "operation"),
		Config:   s.cfg,
		Runtime:  s.deps.Runtime,
		Registry: s.deps.Registry,
		System:   s.deps.System,
		Holds:    s.overrides,
		Store:    s.store,
		Now:      s.now,
		Poll:     s.pollInterval(),
		Notify:   s.requestSync,
		Audit:    s.audit,
	})

	s.wg.Go(func() {
		defer s.opActive.Store(false)

		op.Run(ctx)
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

	if err != nil {
		s.log.WithError(err).Warn("cannot collect the inventory")

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
