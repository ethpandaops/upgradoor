package operation

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// reboot persists the phase, then issues the reboot. It reports whether the
// reboot was issued; the caller then waits in awaitReboot, where the agent
// normally dies, and picks the operation up from the state file after the
// boot. ctx is the uninterruptible context of execute: stopping the agent
// between the drain and the reboot would leave the host drained for nothing.
func (o *operation) reboot(ctx context.Context, plan *operationPlan) bool {
	var (
		err       error
		cancelled bool
	)

	for range persistAttempts {
		err = o.store.Update(func(st *journal.State) {
			cur := st.Current
			if cur == nil {
				return
			}

			// The reboot has not begun: a cancel wins.
			if cancelled = cur.Cancelled; cancelled {
				return
			}

			now := o.now()
			cur.Operation.Phase = protocol.PhaseRebooting
			cur.Operation.UpdatedAt = now
			cur.Operation.Disrupted = true
			cur.RebootIssuedAt = now
			cur.Touched = slices.Clone(cur.WasRunning)

			markSteps(cur, plan.reboot, protocol.StepRunning, "")
		})
		if err == nil {
			break
		}

		sleep(ctx, o.poll)
	}

	if cancelled {
		o.stop()

		return false
	}

	if err != nil {
		// Rebooting without the phase on disk would lose the operation.
		msg := fmt.Sprintf("cannot persist the state before the reboot: %v", err)

		o.fail(msg)
		o.setSteps(plan.reboot, protocol.StepFailed, msg)

		return false
	}

	o.notify()
	o.log.Warn("rebooting the host")

	cctx, cancel := context.WithTimeout(ctx, rebootTimeout)
	err = o.system.Reboot(cctx)

	cancel()

	if err != nil {
		msg := fmt.Sprintf("reboot: %v", err)

		o.fail(msg)
		o.setSteps(plan.reboot, protocol.StepFailed, msg)

		return false
	}

	return true
}

// awaitReboot waits until the boot id differs from the one the operation
// started under. If it still is the same long after the reboot was issued,
// the host did not reboot and the reboot steps fail.
func (o *operation) awaitReboot(ctx context.Context, ids []string) error {
	for {
		rec := o.store.Snapshot().Current
		if rec == nil {
			return errInterrupted
		}

		sys, err := o.status(ctx)

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

		if waited := o.now().Sub(rec.RebootIssuedAt); waited > defaultRebootGrace {
			msg := fmt.Sprintf("the reboot was issued at %s but the host did not reboot "+
				"within %s (boot id unchanged)",
				rec.RebootIssuedAt.UTC().Format(time.RFC3339), defaultRebootGrace)

			o.fail(msg)
			o.setSteps(ids, protocol.StepFailed, msg)

			return nil
		}

		if !sleep(ctx, o.poll) {
			return errInterrupted
		}
	}
}
