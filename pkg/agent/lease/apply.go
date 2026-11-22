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
	recreateGrace = 10 * time.Minute
)

// applyPackages upgrades the granted blocked packages.
func (o *operation) applyPackages(ctx context.Context) {
	ids := o.lp.PackageIDs
	if o.stopped() || len(ids) == 0 || !o.proceed(ids) {
		return
	}

	o.setPhase(protocol.PhaseApplying)

	var touched []string
	if rec := o.r.st.current(); rec != nil {
		touched = slices.Clone(rec.Touched)
	}

	wasDisrupted := o.disrupted()
	before, beforeErr := o.containers(ctx)

	// Upgrading Docker may restart the daemon and with it every container.
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

	if !wasDisrupted && beforeErr == nil {
		if after, err := o.containers(ctx); err == nil && !restartedAny(before, after) {
			o.log.Info("the package upgrade restarted no container")
			o.apply(func(c *content) {
				c.Current.Operation.Disrupted = false
				c.Current.Touched = touched
			})
		}
	}

	o.setSteps(ids, protocol.StepDone, "")
}

// restartedAny reports whether a container running in before is not
// running in after, or was started again.
func restartedAny(before, after []protocol.Container) bool {
	for i := range before {
		b := &before[i]
		if b.State != protocol.StateRunning {
			continue
		}

		j := slices.IndexFunc(after, func(c protocol.Container) bool { return c.ID == b.ID })
		if j < 0 || after[j].State != protocol.StateRunning || !after[j].StartedAt.Equal(b.StartedAt) ||
			after[j].RestartCount != b.RestartCount {
			return true
		}
	}

	return false
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
// host's files (plan.Recheck) right before; host checks the digest, which
// the pulling phase made local, before it stops anything.
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
		log = log.WithFields(logrus.Fields{keyImage: rec.Image, "flags": rec.SetCommand})
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

		log.Info("the local image changed after the pull; image need superseded")
		o.setSteps(task.IDs, protocol.StepSuperseded,
			fmt.Sprintf("the local image of %s is not the granted digest %s any more", ref, task.Digest))
	case errors.Is(err, host.ErrImageMoved):
		// The container was stopped and rolled back: the disrupted mark stays,
		// the starting phase brings it back if the rollback could not.
		if slices.Contains(o.lp.WasRunning, name) {
			o.touch([]string{name})
		}

		log.WithError(err).Warn("the local image changed after the stop; image need superseded")
		o.setSteps(task.IDs, protocol.StepSuperseded,
			fmt.Sprintf("the local image of %s changed from the granted digest %s during the recreate", ref, task.Digest))
	default:
		msg := fmt.Sprintf("recreate container %s: %v", name, err)

		o.fail(msg)
		o.setSteps(task.IDs, protocol.StepFailed, msg)
	}
}
