package lease

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	rebootTimeout = 2 * time.Minute
	// rebootGrace is how long after issuing the reboot an unchanged boot id
	// still means "about to go down".
	rebootGrace = 5 * time.Minute
)

// reboot persists the rebooting phase, then issues the reboot, and reports
// whether it was issued. A cancel written before the phase wins.
func (o *operation) reboot(ctx context.Context) bool {
	ids := o.lp.Reboot

	var (
		err       error
		cancelled bool
	)

	for attempt := range persistAttempts {
		if attempt > 0 {
			sleep(ctx, o.r.poll)
		}

		err = o.r.st.update(func(c *content) {
			cur := c.Current
			if cur == nil {
				return
			}

			if cancelled = cur.Cancelled; cancelled {
				return
			}

			now := o.now()
			cur.Operation.Phase = protocol.PhaseRebooting
			cur.Operation.UpdatedAt = now
			cur.Operation.Disrupted = true
			cur.RebootIssuedAt = now
			cur.Touched = slices.Clone(cur.WasRunning)

			markSteps(cur, ids, protocol.StepRunning, "")
		})
		if err == nil {
			break
		}
	}

	if cancelled {
		o.noteCancel()

		return false
	}

	if err != nil {
		// Rebooting without the phase on disk would lose the operation.
		msg := fmt.Sprintf("cannot persist the state before the reboot: %v", err)

		o.fail(msg)
		o.setSteps(ids, protocol.StepFailed, msg)

		return false
	}

	o.r.deps.Notify()
	o.log.Warn("rebooting the host")

	cctx, cancel := context.WithTimeout(ctx, rebootTimeout)
	err = o.r.deps.Host.Reboot(cctx)

	cancel()

	if err != nil {
		msg := fmt.Sprintf("reboot: %v", err)

		o.fail(msg)
		o.setSteps(ids, protocol.StepFailed, msg)

		return false
	}

	return true
}

// awaitReboot waits until the boot id changes; unchanged rebootGrace after
// the reboot was issued, the steps fail.
func (o *operation) awaitReboot(ctx context.Context, ids []string) error {
	for {
		rec := o.r.st.current()
		if rec == nil {
			return errInterrupted
		}

		sys, err := o.system(ctx)

		switch {
		case err != nil && ctx.Err() != nil:
			return errInterrupted
		case err != nil:
			o.log.WithError(err).Warn("cannot read the boot id while waiting for the reboot")
		case sys.BootID != "" && sys.BootID != rec.BootID:
			o.log.WithField("bootId", sys.BootID).Info("the host is back from the reboot")
			o.setSteps(ids, protocol.StepDone, "")

			return nil
		}

		if o.now().Sub(rec.RebootIssuedAt) > rebootGrace {
			msg := fmt.Sprintf("the reboot was issued at %s but the host did not reboot "+
				"within %s (boot id unchanged)",
				rec.RebootIssuedAt.UTC().Format(time.RFC3339), rebootGrace)

			o.fail(msg)
			o.setSteps(ids, protocol.StepFailed, msg)

			return nil
		}

		if !sleep(ctx, o.r.poll) {
			return errInterrupted
		}
	}
}
