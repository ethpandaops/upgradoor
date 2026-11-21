package operation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/imagewatch"
	"github.com/ethpandaops/upgradoor/pkg/agent/needs"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// applyPackages upgrades the granted blocked packages. ctx is the
// uninterruptible context of execute.
func (o *operation) applyPackages(ctx context.Context, plan *operationPlan) {
	if o.stopped() || len(plan.packageIDs) == 0 || !o.proceed(plan.packageIDs) {
		return
	}

	o.setPhase(protocol.PhaseApplying)

	// Upgrading Docker restarts the daemon and with it every container.
	if !o.markDisrupted(ctx, plan.packageIDs) {
		return
	}

	o.touch(plan.wasRunning)

	o.log.WithField("packages", plan.packages).Info("upgrading packages")

	cctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	err := o.system.UpgradePackages(cctx, plan.packages)

	cancel()

	if err != nil {
		msg := fmt.Sprintf("upgrade packages %s: %v", strings.Join(plan.packages, ", "), err)

		o.fail(msg)
		o.setSteps(plan.packageIDs, protocol.StepFailed, msg)

		return
	}

	o.setSteps(plan.packageIDs, protocol.StepDone, "")
}

// applyImages recreates the granted containers, execution client first.
// ctx is the uninterruptible context of execute.
func (o *operation) applyImages(ctx context.Context, plan *operationPlan) {
	if len(plan.images) == 0 {
		return
	}

	if !o.stopped() {
		o.setPhase(protocol.PhaseApplying)
	}

	for i := range plan.images {
		if o.stopped() {
			break
		}

		o.recreate(ctx, plan, &plan.images[i])
	}
}

// recreate serves one image task. The runtime verifies the digest before it
// stops anything; a mismatch therefore costs nothing.
func (o *operation) recreate(ctx context.Context, plan *operationPlan, task *imageTask) {
	name := task.container
	log := o.log.WithFields(logrus.Fields{logContainer: name, "digest": task.digest})

	// Look again right before acting: a person may have swapped the image
	// since the lease was validated.
	containers, err := o.observe(ctx)
	if err != nil {
		msg := fmt.Sprintf("cannot list containers before recreating %s: %v", name, err)

		o.fail(msg)
		o.setSteps(task.ids, protocol.StepFailed, msg)

		return
	}

	_, targets := needs.Enrich(o.cfg, containers, o.overrides(), noRegistryCheck)
	c := needs.FindContainer(containers, name)

	var change hostapi.Recreation

	if task.override != "" {
		target, ok := targets[name]
		if reason := overrideStale(c, &target, ok, task.override, task.digest); reason != "" {
			o.setSteps(task.ids, protocol.StepSuperseded, reason)

			return
		}

		change = hostapi.Recreation{
			Image: target.Image, Command: target.Command, SetCommand: target.Flags,
			Labels: target.Labels,
		}

		log = log.WithFields(logrus.Fields{"image": target.Image, "flags": target.Flags})
	} else if reason, _ := needs.Rollable(c, name); reason != "" {
		o.setSteps(task.ids, protocol.StepSuperseded, reason)

		return
	}

	// A container the name map of agent.yaml put in its group gets its
	// labels with this recreate: labels are set only when a container is
	// created.
	if c.Unlabelled {
		labels := c.GroupLabels()
		maps.Copy(labels, change.Labels)
		change.Labels = labels
		log = log.WithField("group", c.Group)
	}

	ref := cmp.Or(change.Image, c.Image)

	wasDisrupted := o.disrupted()
	// After a drain the container stays stopped until the host is back; a
	// container that was not running is not started by a roll either.
	start := len(plan.reboot) == 0 && slices.Contains(plan.wasRunning, name)
	timeout := o.stopTimeout(name)

	if !o.proceed(task.ids) {
		return
	}

	if !o.markDisrupted(ctx, task.ids) {
		return
	}

	log.WithField("start", start).Info("recreating container")

	cctx, cancel := context.WithTimeout(ctx, timeout+recreateGrace)
	err = o.runtime.RecreateAs(cctx, name, task.digest, timeout, start, change)

	cancel()

	switch {
	case err == nil:
		if slices.Contains(plan.wasRunning, name) {
			o.touch([]string{name})
		}

		o.setSteps(task.ids, protocol.StepDone, "")
	case errors.Is(err, hostapi.ErrDigestMismatch):
		if !wasDisrupted {
			o.clearDisrupted()
		}

		o.digestMismatch(ctx, log, task, ref)
	default:
		msg := fmt.Sprintf("recreate container %s: %v", name, err)

		o.fail(msg)
		o.setSteps(task.ids, protocol.StepFailed, msg)
	}
}

// digestMismatch decides what a refused recreate means: if the tag has
// moved on, the need is superseded (a new one follows by itself); if the
// tag still points at the granted digest but the local image is something
// else, the step failed.
func (o *operation) digestMismatch(
	ctx context.Context, log logrus.FieldLogger, task *imageTask, ref string,
) {
	rctx, cancel := context.WithTimeout(ctx, imagewatch.ResolveTimeout)
	current, err := o.registry.Resolve(rctx, ref)

	cancel()

	switch {
	case err != nil:
		msg := fmt.Sprintf("the local image of %s is not the granted digest %s and the "+
			"registry could not be asked: %v", ref, task.digest, err)

		o.fail(msg)
		o.setSteps(task.ids, protocol.StepFailed, msg)
	case current != task.digest:
		log.WithField("current", current).Info("tag moved on; image need superseded")

		o.setSteps(task.ids, protocol.StepSuperseded,
			fmt.Sprintf("tag %s now points at %s", ref, current))
	default:
		msg := fmt.Sprintf("the local image of %s is not the granted digest %s "+
			"although the tag still points at it", ref, task.digest)

		o.fail(msg)
		o.setSteps(task.ids, protocol.StepFailed, msg)
	}
}
