package operation

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// drain stops the managed containers ahead of a reboot: validator client
// first, execution client last, each with its stop timeout. ctx is the
// uninterruptible context of execute.
func (o *operation) drain(ctx context.Context, plan *operationPlan) {
	o.setPhase(protocol.PhaseDraining)

	for _, name := range o.ordered(plan.wasRunning, drainOrder) {
		if !o.proceed(nil) {
			return
		}

		if !o.markDisrupted(ctx, plan.reboot) {
			return
		}

		timeout := o.stopTimeout(name)
		log := o.log.WithFields(logrus.Fields{logContainer: name, "timeout": timeout})

		log.Info("stopping container ahead of the reboot")

		cctx, cancel := context.WithTimeout(ctx, timeout+stopGrace)
		err := o.runtime.Stop(cctx, name, timeout)

		cancel()

		if err == nil {
			continue
		}

		// Do not reboot under a container that could not be stopped
		// cleanly.
		msg := fmt.Sprintf("stop container %s: %v", name, err)

		o.fail(msg)
		o.setSteps(plan.reboot, protocol.StepFailed, msg)

		return
	}
}
