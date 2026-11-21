package agent

import (
	"context"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/agent/registry"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of the calls of one cycle.
const (
	observeTimeout   = 30 * time.Second
	systemTimeout    = 30 * time.Second
	inventoryTimeout = 2 * time.Minute
	// soonSync is the longest wait after a cycle that answered something
	// new or took a lease.
	soonSync = time.Second
	// fallbackPolls is the lookup fallback in registry poll intervals,
	// until an answer names one.
	fallbackPolls = 3
)

// The cycle converts host.System into plan.System and plan.Tag into
// registry.Tag; these keep the twins identical.
var (
	_ = plan.System(host.System{})
	_ = registry.Tag(plan.Tag{})
)

// loop syncs until ctx ends: at link's interval, within soonSync after a
// cycle that answered something, and when woken.
func (a *agent) loop(ctx context.Context) {
	for ctx.Err() == nil {
		soon := a.cycle(ctx)

		wait := a.link.Interval(a.busy())
		if soon {
			wait = min(wait, a.cfg.Controller.LeaseSyncInterval, soonSync)
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
		case <-a.kick:
		case <-timer.C:
		}

		timer.Stop()
	}
}

// busy reports whether a lease, a roll waiting for its lookup or a local
// roll is open.
func (a *agent) busy() bool {
	return a.runner.Current() != nil || len(a.directives.pending) > 0 || a.rolls.Open()
}

// cycle observes, syncs and acts on the answer (tracker, directives, lease,
// inventory, in that order). It reports whether another sync should follow soon.
func (a *agent) cycle(ctx context.Context) bool {
	facts := a.observe(ctx)
	if ctx.Err() != nil {
		return false
	}

	files := a.files.Load()
	view := plan.Classify(&a.cfg.Policy, &facts, &files, a.tracker.Checks(), a.now())

	if facts.Fresh {
		tags := make(map[string]registry.Tag, len(view.Tags))
		for key, t := range view.Tags {
			tags[key] = registry.Tag(t)
		}

		a.tracker.Track(tags)
	}

	a.takeLocalRolls(&view)
	a.resolveRolls(&view)
	a.reconcileManual(&view)

	req := a.report(ctx, &view)

	resp, err := a.link.Sync(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}

		// link logs the episode; nothing of the answer is acted on.
		a.syncFailing = true
		a.tracker.SetFallback(a.link.Silent(fallbackPolls * a.cfg.Registry.PollInterval))
		a.publish(&view, req, nil)

		return false
	}

	a.syncFailing = false

	a.tracker.SetFallback(false)
	a.tracker.Relay(resp.Digests)
	a.tracker.Lookup(resp.Lookups)
	a.tracker.SetWindow(resp.Pull)
	a.logQueue(resp.Queue)

	soon := a.handleDirectives(resp.Directives, &view)

	if a.runner.Follow(resp.Lease, req.Operation) {
		soon = true
	}

	if resp.WantInventory {
		a.sendInventory(ctx)
	}

	a.publish(&view, req, resp)

	return soon
}

// observe reads the containers and the system state; a part that cannot be
// read keeps its last good value.
func (a *agent) observe(ctx context.Context) plan.Facts {
	cctx, cancel := context.WithTimeout(ctx, observeTimeout)
	containers, err := a.host.Containers(cctx)

	cancel()

	if err != nil {
		a.log.WithError(err).Warn("cannot list containers; reporting the last known state")

		a.facts.Fresh = false
	} else {
		a.facts.Containers, a.facts.Fresh = containers, true
	}

	cctx, cancel = context.WithTimeout(ctx, systemTimeout)
	sys, err := a.host.System(cctx)

	cancel()

	if err != nil {
		a.log.WithError(err).Warn("cannot read the system state; reporting the last known state")
	} else {
		s := plan.System(sys)
		a.facts.System = &s
	}

	return plan.Facts{
		Containers: slices.Clone(a.facts.Containers), Fresh: a.facts.Fresh, System: a.facts.System,
	}
}

// reconcileManual stores what plan.Reconcile makes of the manual needs and
// logs each dropped one.
func (a *agent) reconcileManual(view *plan.View) {
	out := plan.Reconcile(&a.cfg.Policy, view, a.state.Manual())
	if !out.Changed {
		return
	}

	dropped := make([]string, 0, len(out.Dropped))
	for id := range out.Dropped {
		dropped = append(dropped, id)
	}

	if err := a.state.ReconcileManual(out.Kept, dropped); err != nil {
		a.log.WithError(err).Error("cannot persist the manual needs")

		return
	}

	for id, reason := range out.Dropped {
		a.log.WithFields(logrus.Fields{"need": id, "reason": reason}).Warn("manual need dropped")
	}
}

// report is the sync request; needs and operation come from one read of the
// state file.
func (a *agent) report(ctx context.Context, view *plan.View) *protocol.SyncRequest {
	manual, cur := a.state.ManualAndCurrent()
	req := &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    a.version,
		SentAt:          a.now().UTC(),
		Verbs:           append([]protocol.Verb{}, a.cfg.Policy.Verbs...),
		Hold:            view.Hold,
		Containers:      slices.Clone(view.Containers),
		Node:            a.host.Probe(ctx),
		Needs:           plan.Needs(&a.cfg.Policy, view, manual),
		DirectiveAcks:   a.state.Acks(a.directives.repeated),
		Pulling:         a.tracker.Pulling(),
		Validators:      a.validators(ctx),
		Operation:       cur.Reported(),
	}

	if sys := view.System; sys != nil {
		req.BootID = sys.BootID
		req.UptimeSeconds = int64(sys.Uptime / time.Second)
		req.OS = sys.OS
		req.Pending = sys.Pending
		req.InventoryHash = sys.InventoryHash
	}

	return req
}

// validators reads the validator range and logs it when it changed.
func (a *agent) validators(ctx context.Context) *protocol.Validators {
	v := a.host.Validators(ctx)

	switch {
	case v == nil && a.lastValidators == nil:
	case v != nil && a.lastValidators != nil && *v == *a.lastValidators:
	case v == nil:
		a.log.Info("no validator range configured any more; reporting no validators")
	case v.State == protocol.ValidatorsInvalid:
		a.log.WithFields(logrus.Fields{"source": v.Source, "error": v.Error}).
			Error("cannot read the validator range; reporting it as unknown, not as none")
	case v.State == protocol.ValidatorsMissing:
		a.log.WithField("marker", a.cfg.Host.Validators.MarkerFile).
			Warn("the validator key range marker is missing (keys being reshuffled?); " +
				"reporting the range as unknown, not as none")
	default:
		a.log.WithFields(logrus.Fields{"source": v.Source, "start": v.Start, "end": v.End}).
			Info("validator range")
	}

	a.lastValidators = v

	return v
}

// sendInventory uploads the inventory, collected within inventoryTimeout.
func (a *agent) sendInventory(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, a.inventoryTimeout)
	inv, err := a.host.Inventory(cctx)

	cancel()

	if err != nil {
		a.log.WithError(err).Warn("cannot collect the inventory")

		return
	}

	if err := a.link.Inventory(ctx, inv); err != nil {
		a.log.WithError(err).Warn("inventory upload failed")

		return
	}

	a.log.WithFields(logrus.Fields{
		"hash": inv.Hash, "packages": len(inv.Packages), "upgradable": len(inv.Upgradable),
	}).Info("inventory uploaded")
}

// logQueue logs the queue position and waiting reason when they change.
func (a *agent) logQueue(q *protocol.Queue) {
	var cur protocol.Queue
	if q != nil {
		cur = *q
	}

	if cur == a.queue {
		return
	}

	a.queue = cur

	if q == nil {
		a.log.Info("no longer queued")

		return
	}

	a.log.WithFields(logrus.Fields{"position": q.Position, "reason": q.Reason}).
		Info("waiting for a slot")
}

// publish brings the local rolls up to date and stores the status the
// socket serves, from one read of the lease and the manual needs. resp is
// nil when the sync failed.
func (a *agent) publish(view *plan.View, req *protocol.SyncRequest, resp *protocol.SyncResponse) {
	manual, cur := a.state.ManualAndCurrent()

	progress := local.Progress{Hold: view.Hold, Answered: resp != nil, Open: make([]string, 0, len(manual))}
	for i := range manual {
		progress.Open = append(progress.Open, manual[i].ID)
	}

	if cur != nil {
		op := cur.Operation
		progress.Lease, progress.Started, progress.Operation = cur.Lease.ID, cur.Started, &op
	}

	if resp != nil {
		progress.Queue, progress.Ended = resp.Queue, resp.Ended
	}

	a.rolls.Follow(&progress)

	status := local.Status{
		Host: a.hostName, Network: a.network, Version: a.version, At: a.now().UTC(),
		Verbs: slices.Clone(a.cfg.Policy.Verbs), Hold: view.Hold,
		Containers: local.ContainersOf(view),
		Needs:      plan.CloneNeeds(req.Needs), Lease: progress.Lease, Operation: progress.Operation,
		OverridesFile: a.files.Path(), OverridesError: view.OverridesError,
	}

	if a.queue != (protocol.Queue{}) {
		q := a.queue
		status.Queue = &q
	}

	if a.syncFailing {
		status.SyncError = "the controller cannot be reached; nothing is granted until it can"
	}

	a.mu.Lock()
	a.view, a.status = view, status
	a.mu.Unlock()
}
