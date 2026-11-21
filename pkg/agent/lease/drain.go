package lease

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// stopGrace is added to a container's stop timeout for the call's bound.
const stopGrace = time.Minute

// drain stops the containers ahead of a reboot, validator client first.
func (o *operation) drain(ctx context.Context) {
	o.setPhase(protocol.PhaseDraining)

	for _, name := range plan.DrainOrder(o.lp.WasRunning, o.classified()) {
		if !o.proceed(nil) || !o.markDisrupted(ctx, o.lp.Reboot) {
			return
		}

		timeout := o.stopTimeout(name)
		o.log.WithFields(logrus.Fields{"container": name, "timeout": timeout}).
			Info("stopping container ahead of the reboot")

		cctx, cancel := context.WithTimeout(ctx, timeout+stopGrace)
		err := o.r.deps.Host.Stop(cctx, name, timeout)

		cancel()

		if err != nil {
			// No reboot under a container that could not be stopped cleanly.
			msg := fmt.Sprintf("stop container %s: %v", name, err)

			o.fail(msg)
			o.setSteps(o.lp.Reboot, protocol.StepFailed, msg)

			return
		}
	}
}
