package lease

import (
	"context"
	"fmt"
	"slices"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// reasonActionUnknown fails an action step that ran while the agent
// restarted: the command may still run or have ended either way.
const reasonActionUnknown = "outcome unknown: the agent restarted while the action ran"

// runActions runs the granted actions in order. A run is not interrupted by
// ctx; its own timeout ends it.
func (o *operation) runActions(ctx context.Context) {
	for _, run := range o.lp.Actions {
		if o.stopped() {
			return
		}

		o.runAction(ctx, run)
	}
}

// runAction runs one action by the argv of agent.yaml, then rescans the
// packages so the next sync reports what the run changed.
func (o *operation) runAction(ctx context.Context, run plan.ActionRun) {
	ids := []string{run.ID}
	log := o.log.WithField("action", run.Name)

	a, ok := o.r.deps.Policy.Actions[run.Name]
	if !ok {
		o.setSteps(ids, protocol.StepRefused, fmt.Sprintf("unknown action %q", run.Name))

		return
	}

	if !o.proceed(ids) || !o.markDisrupted(ctx, ids) {
		return
	}

	o.setPhase(protocol.PhaseAction)
	log.WithField("timeout", a.Timeout).Info("running action")

	res := o.r.deps.Host.RunAction(ctx, a.Command, a.Timeout)

	cctx, cancel := context.WithTimeout(ctx, rescanTimeout)
	sys, err := o.r.deps.Host.Rescan(cctx)

	cancel()

	switch {
	case err != nil:
		log.WithError(err).Warn("cannot rescan the packages after the action")
	case o.r.deps.ActionRan != nil:
		o.r.deps.ActionRan(run.Name, sys.InventoryHash)
	}

	status, reason := protocol.StepDone, ""

	switch {
	case res.TimedOut:
		status, reason = protocol.StepFailed, fmt.Sprintf("action %s timed out after %s", run.Name, a.Timeout)
	case res.ExitCode != 0:
		status, reason = protocol.StepFailed, fmt.Sprintf("action %s exited with code %d", run.Name, res.ExitCode)
	}

	log = log.WithFields(logrus.Fields{"exitCode": res.ExitCode, "timedOut": res.TimedOut})

	if status == protocol.StepFailed {
		o.fail(reason)
	} else {
		log.Info("action done")
	}

	o.setActionStep(run.ID, status, reason, &res)
}

// setActionStep records the outcome of an action step with its result.
func (o *operation) setActionStep(id string, status protocol.StepStatus, reason string, res *protocol.ActionResult) {
	o.apply(func(c *content) {
		markSteps(c.Current, []string{id}, status, reason)

		for i := range c.Current.Operation.Steps {
			if s := &c.Current.Operation.Steps[i]; s.NeedID == id {
				r := *res
				s.Action = &r
			}
		}
	})
}

// failUnknownActions fails the action steps that ran when the agent
// stopped, with their outcome unknown.
func (o *operation) failUnknownActions(rec *Record) {
	ids := stepIDs(rec, protocol.StepRunning)
	if len(ids) == 0 {
		return
	}

	o.fail(reasonActionUnknown)

	for _, id := range ids {
		if slices.ContainsFunc(rec.Lease.Needs, func(n protocol.Need) bool {
			return n.ID == id && n.Kind == protocol.NeedAction
		}) {
			o.setActionStep(id, protocol.StepFailed, reasonActionUnknown,
				&protocol.ActionResult{ExitCode: -1, Unknown: true})
		}
	}
}
