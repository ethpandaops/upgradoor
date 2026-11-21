package operation

import (
	"context"
	"fmt"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// bringBack runs the starting and verifying phases and closes the
// operation. It also runs after a failure, so that the host comes back as
// far as it can.
func (o *operation) bringBack(ctx context.Context) error {
	rec := o.store.Snapshot().Current
	if rec == nil {
		return errInterrupted
	}

	if rec.Operation.Disrupted {
		// verify.timeout bounds starting and verifying together.
		deadline := o.now().Add(o.cfg.Verify.Timeout)

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

// startContainers makes sure every managed container that ran before the
// operation runs again, execution client first. Right after a boot the
// Docker daemon may not answer yet, so it keeps trying up to deadline (the
// verify.timeout shared with the verifying phase).
//
// A pass over the containers is not cut short when ctx ends, so that an
// agent that is being stopped still brings the containers back if the
// runtime lets it; only the wait for another pass is given up.
func (o *operation) startContainers(
	ctx context.Context, names []string, deadline time.Time,
) error {
	o.setPhase(protocol.PhaseStarting)

	var (
		work  = context.WithoutCancel(ctx)
		order = o.ordered(names, startOrder)
	)

	for {
		var failure string

		for _, name := range order {
			cctx, cancel := context.WithTimeout(work, startTimeout)
			err := o.runtime.Start(cctx, name)

			cancel()

			if err != nil && failure == "" {
				failure = fmt.Sprintf("start container %s: %v", name, err)
			}
		}

		if failure == "" {
			return nil
		}

		if ctx.Err() != nil {
			return errInterrupted
		}

		if !o.now().Before(deadline) {
			o.fail(failure)

			return nil
		}

		o.log.WithField("error", failure).Debug("containers not started yet; trying again")

		if !sleep(ctx, o.poll) {
			return errInterrupted
		}
	}
}
