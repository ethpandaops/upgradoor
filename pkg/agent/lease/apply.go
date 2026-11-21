package lease

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of the calls of the applying phase.
const (
	upgradeTimeout = 30 * time.Minute
	// recreateGrace is added to the container's stop timeout.
	recreateGrace  = 10 * time.Minute
	resolveTimeout = time.Minute
)

// applyPackages upgrades the granted blocked packages.
func (o *operation) applyPackages(ctx context.Context) {
	ids := o.lp.PackageIDs
	if o.stopped() || len(ids) == 0 || !o.proceed(ids) {
		return
	}

	o.setPhase(protocol.PhaseApplying)

	// Upgrading Docker restarts the daemon and with it every container.
	if !o.markDisrupted(ctx, ids) {
		return
	}

	o.touch(o.lp.WasRunning)

	o.log.WithField("packages", o.lp.Packages).Info("upgrading packages")

	cctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	err := o.r.deps.Host.UpgradePackages(cctx, o.lp.Packages)

	cancel()

	if err != nil {
		msg := fmt.Sprintf("upgrade packages %s: %v", strings.Join(o.lp.Packages, ", "), err)

		o.fail(msg)
		o.setSteps(ids, protocol.StepFailed, msg)

		return
	}

	o.setSteps(ids, protocol.StepDone, "")
}

// applyImages recreates the granted containers in start order.
func (o *operation) applyImages(ctx context.Context) {
	if len(o.lp.Recreates) == 0 {
		return
	}

	if !o.stopped() {
		o.setPhase(protocol.PhaseApplying)
	}

	for i := range o.lp.Recreates {
		if o.stopped() {
			return
		}

		o.recreate(ctx, &o.lp.Recreates[i])
	}
}

// recreate serves one recreate. What the container becomes is read from the
// host's files (plan.Recheck) right before; host checks the digest before it
// stops anything.
func (o *operation) recreate(ctx context.Context, task *plan.Recreate) {
	name := task.Container
	log := o.log.WithFields(logrus.Fields{"container": name, "digest": task.Digest})

	list, err := o.containers(ctx)
	if err != nil {
		msg := fmt.Sprintf("cannot list containers before recreating %s: %v", name, err)

		o.fail(msg)
		o.setSteps(task.IDs, protocol.StepFailed, msg)

		return
	}

	view := o.classify(list, nil)

	rec, reason := plan.Recheck(task, view)
	if reason != "" {
		o.setSteps(task.IDs, protocol.StepSuperseded, reason)

		return
	}

	if task.Target != "" {
		log = log.WithFields(logrus.Fields{"image": rec.Image, "flags": rec.SetCommand})
	}

	if rec.Group != "" {
		log = log.WithField("group", rec.Group)
	}

	ref := cmp.Or(rec.Image, view.Container(name).Image)
	wasDisrupted := o.disrupted()
	// After a drain the container stays stopped until the host is back; a
	// container that was not running is not started by a roll either.
	start := len(o.lp.Reboot) == 0 && slices.Contains(o.lp.WasRunning, name)
	timeout := o.stopTimeout(name)

	if !o.proceed(task.IDs) || !o.markDisrupted(ctx, task.IDs) {
		return
	}

	log.WithField("start", start).Info("recreating container")

	change := host.Change(rec)
	cctx, cancel := context.WithTimeout(ctx, timeout+recreateGrace)
	err = o.r.deps.Host.Recreate(cctx, name, task.Digest, timeout, start, &change)

	cancel()

	switch {
	case err == nil:
		if slices.Contains(o.lp.WasRunning, name) {
			o.touch([]string{name})
		}

		o.setSteps(task.IDs, protocol.StepDone, "")
	case errors.Is(err, host.ErrDigestMismatch):
		// Nothing was stopped: the mark set for this call alone goes.
		if !wasDisrupted {
			o.apply(func(c *content) { c.Current.Operation.Disrupted = false })
		}

		o.digestMismatch(ctx, log, task, ref)
	default:
		msg := fmt.Sprintf("recreate container %s: %v", name, err)

		o.fail(msg)
		o.setSteps(task.IDs, protocol.StepFailed, msg)
	}
}

// digestMismatch supersedes the step when the tag moved on and fails it
// otherwise.
func (o *operation) digestMismatch(
	ctx context.Context, log logrus.FieldLogger, task *plan.Recreate, ref string,
) {
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	current, err := o.r.deps.Registry.Resolve(rctx, ref)

	cancel()

	switch {
	case err != nil:
		msg := fmt.Sprintf("the local image of %s is not the granted digest %s and the "+
			"registry could not be asked: %v", ref, task.Digest, err)

		o.fail(msg)
		o.setSteps(task.IDs, protocol.StepFailed, msg)
	case current != task.Digest:
		log.WithField("current", current).Info("tag moved on; image need superseded")

		o.setSteps(task.IDs, protocol.StepSuperseded, fmt.Sprintf("tag %s now points at %s", ref, current))
	default:
		msg := fmt.Sprintf("the local image of %s is not the granted digest %s "+
			"although the tag still points at it", ref, task.Digest)

		o.fail(msg)
		o.setSteps(task.IDs, protocol.StepFailed, msg)
	}
}
