package lease

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Why a lease that has not started is never started.
const (
	reasonWithdrawn = "the controller no longer offered the lease before the operation started"
	reasonCancelled = "the lease was cancelled before the operation started"
)

// whyRevoked returns why a lease that has not started must not start, or "".
func whyRevoked(rec *Record) string {
	switch {
	case rec.Cancelled:
		return reasonCancelled
	case rec.Withdrawn:
		return reasonWithdrawn
	default:
		return ""
	}
}

// begin decides whether to act on a persisted lease and, if so, executes it.
// A lease that is not started ends done with every step refused.
func (o *operation) begin(ctx context.Context, rec *Record) error {
	if reason := whyRevoked(rec); reason != "" {
		return o.decline(reason)
	}

	if !o.now().Before(rec.Deadline) {
		return o.decline(fmt.Sprintf("lease deadline %s passed before the operation started",
			rec.Deadline.UTC().Format(time.RFC3339)))
	}

	if hold := o.hold(); hold.Active() {
		return o.decline(plan.HoldReason(hold))
	}

	lp, err := o.planUntil(ctx, rec)

	var revoked revokedError

	switch {
	case errors.Is(err, errInterrupted):
		return err
	case errors.As(err, &revoked):
		return o.decline(string(revoked))
	case err != nil:
		return o.decline(fmt.Sprintf("lease deadline passed before the host could be examined: %v", err))
	case lp.Decline != "":
		return o.decline(lp.Decline)
	case !o.now().Before(rec.Deadline):
		return o.decline("lease deadline passed while the operation was prepared")
	}

	if hold := o.hold(); hold.Active() {
		return o.decline(plan.HoldReason(hold))
	}

	// From the start write on the disruptive phases run to their end.
	if ctx.Err() != nil {
		return errInterrupted
	}

	if !o.start(&lp) {
		return nil
	}

	o.lp = lp

	if lp.Empty() {
		o.log.Info("nothing to do for this lease: every need is superseded or refused")

		return nil
	}

	o.log.WithFields(logrus.Fields{
		"reboot":   len(lp.Reboot) > 0,
		"packages": lp.Packages,
		"images":   len(lp.Recreates),
	}).Info("operation started")

	return o.execute(ctx)
}

// start writes the start of the operation and reports whether it started.
// Start, withdrawal and cancel exclude each other: whichever is written first
// stands. An empty plan ends the operation in the same write.
func (o *operation) start(lp *plan.LeasePlan) bool {
	first := protocol.PhaseApplying

	switch {
	case lp.Empty():
		first = protocol.PhaseDone
	case len(lp.Reboot) > 0:
		first = protocol.PhaseDraining
	}

	bootID := ""
	if o.view != nil && o.view.System != nil {
		bootID = o.view.System.BootID
	}

	revoked := ""

	err := o.r.st.update(func(c *content) {
		cur := c.Current
		if cur == nil {
			return
		}

		if revoked = whyRevoked(cur); revoked != "" {
			return
		}

		cur.Started = true
		cur.BootID = bootID
		cur.WasRunning = slices.Clone(lp.WasRunning)
		cur.Operation.Phase = first
		cur.Operation.StartedAt = o.now()
		cur.Operation.UpdatedAt = cur.Operation.StartedAt

		for i := range cur.Operation.Steps {
			if v, ok := lp.Verdicts[cur.Operation.Steps[i].NeedID]; ok {
				cur.Operation.Steps[i] = v
			}
		}
	})
	if err != nil {
		// Not persisted means not started: Follow wakes the goroutine again.
		o.log.WithError(err).Error("cannot persist the start of the operation; not acting")

		return false
	}

	if revoked != "" {
		_ = o.decline(revoked)

		return false
	}

	o.r.deps.Notify()

	return true
}

// revokedError ends planning: the lease was withdrawn or cancelled.
type revokedError string

func (e revokedError) Error() string { return string(e) }

// planUntil validates the lease, retrying errors as transient until the
// deadline; a withdrawal or cancel ends it with a revokedError.
func (o *operation) planUntil(ctx context.Context, rec *Record) (plan.LeasePlan, error) {
	warned := false

	for {
		lp, err := o.plan(ctx, rec)
		if err == nil {
			return lp, nil
		}

		if ctx.Err() != nil {
			return plan.LeasePlan{}, errInterrupted
		}

		if !o.now().Before(rec.Deadline) {
			return plan.LeasePlan{}, err
		}

		if cur := o.r.st.current(); cur != nil {
			if reason := whyRevoked(cur); reason != "" {
				return plan.LeasePlan{}, revokedError(reason)
			}
		}

		if !warned {
			warned = true

			o.log.WithError(err).WithField("deadline", rec.Deadline.UTC().Format(time.RFC3339)).
				Warn("cannot examine the host for the lease; trying again until its deadline")
		}

		if !sleep(ctx, o.r.poll) {
			return plan.LeasePlan{}, errInterrupted
		}
	}
}

// plan validates every granted need on a fresh view of the host and the
// open manual needs.
func (o *operation) plan(ctx context.Context, rec *Record) (plan.LeasePlan, error) {
	list, err := o.containers(ctx)
	if err != nil {
		return plan.LeasePlan{}, fmt.Errorf("cannot list containers: %w", err)
	}

	// The system state is needed only for a reboot or packages need;
	// Validate says so.
	sys, sysErr := o.system(ctx)
	view := o.classify(list, sys)

	lp, err := plan.Validate(o.r.deps.Policy, &rec.Lease, view, o.r.st.Manual())
	if err != nil {
		if sysErr != nil {
			return plan.LeasePlan{}, fmt.Errorf("cannot read the system state: %w", sysErr)
		}

		return plan.LeasePlan{}, err
	}

	return lp, nil
}

// decline ends an operation that never started: done, every step refused,
// manual needs left open. A failed write is retried on the next wake.
func (o *operation) decline(reason string) error {
	err := o.r.st.update(func(c *content) {
		cur := c.Current
		if cur == nil {
			return
		}

		now := o.now()
		cur.Operation.Phase = protocol.PhaseDone
		cur.Operation.StartedAt = now
		cur.Operation.UpdatedAt = now
		cur.Operation.Disrupted = false

		for i := range cur.Operation.Steps {
			cur.Operation.Steps[i].Status = protocol.StepRefused
			cur.Operation.Steps[i].Error = reason
		}
	})
	if err != nil {
		o.log.WithError(err).Error("cannot persist the refusal of the lease")

		return nil
	}

	o.log.WithField("reason", reason).Warn("lease not acted on")
	o.r.deps.Notify()

	return nil
}
