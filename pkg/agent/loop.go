package agent

import (
	"context"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
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
)

// The cycle converts host.System into plan.System; this keeps the twins
// identical.
var _ = plan.System(host.System{})

// loop syncs until ctx ends: at link's interval, within soonSync after a
// cycle that answered something, and when woken.
func (a *agent) loop(ctx context.Context) {
	for ctx.Err() == nil {
		soon := a.cycle(ctx)

		wait := a.link.Interval(a.busy())
		if soon {
			wait = min(wait, a.cfg.Controller.LeaseSyncInterval, soonSync)
		}

		if until := a.untilFallback(); until > 0 {
			wait = min(wait, until)
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

// busy reports whether a lease, a roll waiting for its tag's digest or a local
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

	files := a.inputs()
	view := plan.Classify(&a.cfg.Policy, &facts, &files, a.tracker.Checks(), a.now())
	view.ActionsRan = a.ranActions()

	// Stale containers change nothing of what is followed.
	if facts.Fresh {
		a.tracker.Track(view.Tags)
		a.tags = followed(&view)
		a.layout(&view, &files)
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
		soon := a.fallback(&view, req.Needs)
		a.publish(&facts, &view, req, nil)

		return soon
	}

	a.syncFailing = false
	a.endFallback()

	a.tracker.SetDigests(resp.Digests)
	a.logQueue(resp.Queue)

	soon := a.handleDirectives(resp.Directives, &view)

	if a.runner.Follow(resp.Lease, req.Operation) {
		soon = true
	}

	if resp.WantInventory {
		a.sendInventory(ctx)
	}

	a.publish(&facts, &view, req, resp)

	return soon
}

// fallback serves the fallback's containers while the controller has been
// silent for fallback.after: their tags are pulled by the tracker and their
// needs get a lease of the agent's own. A need is tried once per silence. It
// reports whether a lease was taken.
func (a *agent) fallback(view *plan.View, needs []protocol.Need) bool {
	fb := &a.cfg.Policy.Fallback

	silent := a.now().Sub(a.lastAnswer)
	if !a.cfg.Policy.FallbackEnabled() || silent < fb.After {
		return false
	}

	if a.fallbackTried == nil {
		a.fallbackTried = make(map[string]struct{}, 4)

		a.log.WithFields(logrus.Fields{"silent": silent.Round(time.Second), "interval": fb.Interval}).
			Warn("the controller has been silent for fallback.after: " +
				"the fallback's containers follow their tags without it")
	}

	// Stale containers change nothing of what is followed.
	if view.Fresh {
		a.tracker.SetFallback(plan.FallbackTags(&a.cfg.Policy, view), fb.Interval)
	}

	open := plan.FallbackNeeds(&a.cfg.Policy, view, needs)
	open = slices.DeleteFunc(open, func(n protocol.Need) bool {
		_, tried := a.fallbackTried[n.ID]

		return tried
	})

	if !view.Fresh || !a.runner.Grant(open) {
		return false
	}

	ids := make([]string, 0, len(open))

	for i := range open {
		a.fallbackTried[open[i].ID] = struct{}{}
		ids = append(ids, open[i].ID)
	}

	a.log.WithField("needs", ids).Warn("fallback: serving needs without the controller")

	return true
}

// untilFallback is the time until the controller's silence reaches
// fallback.after, while syncs fail and the fallback has not begun: the sync
// backoff does not delay its start. Zero or less otherwise.
func (a *agent) untilFallback() time.Duration {
	fb := &a.cfg.Policy.Fallback
	if !a.syncFailing || !a.cfg.Policy.FallbackEnabled() || a.fallbackTried != nil {
		return 0
	}

	return fb.After - a.now().Sub(a.lastAnswer)
}

// endFallback notes an answer of the controller, which ends the fallback.
func (a *agent) endFallback() {
	a.lastAnswer = a.now()

	if a.fallbackTried == nil {
		return
	}

	a.fallbackTried = nil

	a.tracker.SetFallback(nil, 0)
	a.log.Info("the controller answers again: the fallback ended")
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

// layout brings the default lines of the overrides file up to date; Files
// logs a failure.
func (a *agent) layout(view *plan.View, files *plan.Overrides) {
	if files.Disabled || !files.Known {
		return
	}

	keep, drop := plan.OverrideKeys(view, files)
	_, _ = a.files.Layout(keep, drop)
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
		Verbs:           a.cfg.Policy.ReportedVerbs(),
		Roles:           a.cfg.Roles,
		Hold:            view.Hold,
		Containers:      slices.Clone(view.Containers),
		Node:            a.host.Probe(ctx),
		Needs:           plan.Needs(&a.cfg.Policy, view, manual),
		DirectiveAcks:   a.state.Acks(a.directives.repeated),
		Tags:            a.tags,
		Operation:       cur.Reported(),
		Actions:         plan.Actions(&a.cfg.Policy, view),
	}

	res := a.host.Resources(ctx, view.Containers)
	req.Resources = res.Host

	for i := range req.Containers {
		if stats, ok := res.Containers[req.Containers[i].Name]; ok {
			req.Containers[i].Stats = &stats
		}
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

// followed lists the tags of the view as written, sorted, at most
// protocol.MaxTags.
func followed(view *plan.View) []string {
	tags := make([]string, 0, len(view.Tags))
	for _, ref := range view.Tags {
		tags = append(tags, ref)
	}

	slices.Sort(tags)

	return tags[:min(len(tags), protocol.MaxTags)]
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
// socket serves, from one read of the lease and the manual needs, and the
// facts of view. resp is nil when the sync failed.
func (a *agent) publish(
	facts *plan.Facts, view *plan.View, req *protocol.SyncRequest, resp *protocol.SyncResponse,
) {
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
		Verbs: a.cfg.Policy.ReportedVerbs(), Hold: view.Hold,
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

		if a.fallbackTried != nil {
			status.SyncError = "the controller cannot be reached; " +
				"the fallback's containers follow their tags without it"
		}
	}

	a.mu.Lock()
	a.view, a.status, a.seen = view, status, facts
	a.mu.Unlock()
}
