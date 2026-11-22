package lease

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// startTimeout bounds the start of one container.
const startTimeout = 2 * time.Minute

// reasonStepCancelled is the error of a step not begun because of a cancel.
const reasonStepCancelled = "cancelled: the lease was ended before this step had begun"

// bringBack runs the starting and verifying phases of a disruptive operation
// and ends it; it also runs after a failure or cancel.
func (o *operation) bringBack(ctx context.Context) error {
	rec := o.r.st.current()
	if rec == nil {
		return errInterrupted
	}

	if rec.Operation.Disrupted {
		// verify.timeout bounds starting and verifying together.
		deadline := o.now().Add(o.r.cfg.Timeout)

		if err := o.startContainers(ctx, rec.WasRunning, deadline); err != nil {
			return err
		}

		if !o.failed && len(rec.Touched) > 0 {
			if err := o.verify(ctx, rec.Touched, deadline); err != nil {
				return err
			}
		}
	}

	o.finish()

	return nil
}

// startContainers starts the containers that ran before the operation,
// execution client first, retrying until deadline. A pass is not cut short
// when ctx ends; only the wait for another pass is.
func (o *operation) startContainers(ctx context.Context, names []string, deadline time.Time) error {
	o.setPhase(protocol.PhaseStarting)

	work := context.WithoutCancel(ctx)

	for {
		// A resumed operation needs the classified kinds for the order.
		if o.view == nil {
			if list, err := o.containers(work); err == nil {
				o.classify(list, nil)
			}
		}

		var failure string

		for _, name := range plan.StartOrder(names, o.classified()) {
			cctx, cancel := context.WithTimeout(work, startTimeout)
			err := o.r.deps.Host.Start(cctx, name)

			cancel()

			if err != nil && failure == "" {
				failure = fmt.Sprintf("start container %s: %v", name, err)
			}
		}

		switch {
		case failure == "":
			return nil
		case ctx.Err() != nil:
			return errInterrupted
		case !o.now().Before(deadline):
			o.fail(failure)

			return nil
		}

		o.log.WithField("error", failure).Debug("containers not started yet; trying again")

		if !sleep(ctx, o.r.poll) {
			return errInterrupted
		}
	}
}

// finish makes the operation terminal and closes the manual needs it served
// in the same write.
func (o *operation) finish() {
	phase := protocol.PhaseDone
	if o.failed {
		phase = protocol.PhaseFailed
	}

	var rec *Record

	o.apply(func(c *content) {
		cur := c.Current
		cur.Operation.Phase = phase

		for i := range cur.Operation.Steps {
			s := &cur.Operation.Steps[i]

			switch {
			case s.Status != protocol.StepPending:
			case o.cancelled && !o.failed:
				// After a failure a pending step reads "not attempted", cancel or not.
				s.Status = protocol.StepCancelled
				s.Error = reasonStepCancelled

				if o.holdReason != "" {
					s.Error = "cancelled: " + o.holdReason
				}
			case phase == protocol.PhaseFailed && s.Error == "":
				s.Error = "not attempted: " + cur.Operation.Error
			}
		}

		closeServed(c)

		rec = cur.clone()
	})

	if rec == nil {
		return
	}

	for i := range rec.Lease.Needs {
		n := &rec.Lease.Needs[i]

		step := protocol.Step{NeedID: n.ID}
		if j := slices.IndexFunc(rec.Operation.Steps, func(s protocol.Step) bool { return s.NeedID == n.ID }); j >= 0 {
			step = rec.Operation.Steps[j]
		}

		o.r.deps.Audit.LeaseResult(rec.Lease.ID, n, &step)
	}

	log := o.log.WithFields(logrus.Fields{
		"phase":     phase,
		"disrupted": rec.Operation.Disrupted,
		"took":      o.now().Sub(rec.Operation.StartedAt).Round(time.Second),
	})

	switch {
	case o.failed:
		log.WithField("error", rec.Operation.Error).Error("operation failed")
	case o.holdReason != "":
		log.WithField("reason", o.holdReason).
			Warn("operation ended: the host is on local hold, the remaining steps were not begun")
	case o.cancelled:
		log.Warn("operation ended: the lease was cancelled, the remaining steps were not begun")
	default:
		log.Info("operation done")
	}

	o.r.deps.Notify()
}

// closeServed drops the manual needs the terminal operation served, done or
// failed: a request is good for one attempt.
func closeServed(c *content) {
	served := plan.Served(&c.Current.Operation)

	c.Manual = slices.DeleteFunc(c.Manual, func(n protocol.Need) bool { return slices.Contains(served, n.ID) })
}
