package lease

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// pullTimeout bounds one image pull.
const pullTimeout = 10 * time.Minute

// keyImage is the log field of an image reference.
const keyImage = "image"

// reasonPullInterrupted defers the steps of an operation whose pulls a
// restart of the agent cut short.
const reasonPullInterrupted = "the agent restarted while the images were pulled"

// pullImages makes the image of every recreate local at its digest, before
// anything is stopped. When one cannot be pulled, every open step is
// deferred and nothing of the lease is begun. A recreate whose tag moved on
// is superseded. The pulls end with ctx.
func (o *operation) pullImages(ctx context.Context) error {
	if len(o.lp.Recreates) == 0 {
		return nil
	}

	list, err := o.containers(ctx)
	if err != nil {
		o.deferSteps(fmt.Sprintf("cannot list containers before the pull: %v", err))

		return nil
	}

	view := o.classify(list, nil)
	kept := make([]plan.Recreate, 0, len(o.lp.Recreates))

	for i := range o.lp.Recreates {
		task := &o.lp.Recreates[i]

		if cur := o.r.st.current(); cur != nil && cur.Cancelled {
			o.noteCancel()

			return nil
		}

		if o.held() {
			return nil
		}

		// A recreate that no longer applies is superseded in its turn.
		rec, reason := plan.Recheck(task, view)
		if reason != "" {
			kept = append(kept, *task)

			continue
		}

		ref := cmp.Or(rec.Image, view.Container(task.Container).Image)

		got, err := o.pull(ctx, ref, task.Digest)
		if ctx.Err() != nil {
			return errInterrupted
		}

		switch {
		case err != nil:
			o.deferSteps(fmt.Sprintf("pull %s: %v", ref, err))

			return nil
		case got != task.Digest:
			o.log.WithFields(logrus.Fields{keyImage: ref, "current": got}).
				Info("tag moved on; image need superseded")
			o.setSteps(task.IDs, protocol.StepSuperseded, fmt.Sprintf("tag %s now points at %s", ref, got))
		default:
			kept = append(kept, *task)
		}
	}

	o.lp.Recreates = kept

	return nil
}

// pull returns the registry digest of the local image of ref, pulling it
// unless it is local at digest, and tells the tracker how a pull went.
func (o *operation) pull(ctx context.Context, ref, digest string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, containersTimeout)
	got, _, err := o.r.deps.Host.LocalImage(cctx, ref)

	cancel()

	if err == nil && got == digest {
		return got, nil
	}

	o.log.WithFields(logrus.Fields{keyImage: ref, "digest": digest}).Info("pulling image")

	cctx, cancel = context.WithTimeout(ctx, pullTimeout)
	got, _, err = o.r.deps.Host.Pull(cctx, ref)

	cancel()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	o.r.deps.Registry.NotePull(ref, err)

	if err != nil {
		o.log.WithError(err).WithField(keyImage, ref).Warn("image pull failed; nothing of the lease is begun")
	}

	return got, err
}

// deferSteps defers every step not settled yet: the operation ends without
// having begun anything.
func (o *operation) deferSteps(reason string) {
	o.deferred = true

	o.apply(func(c *content) {
		for i := range c.Current.Operation.Steps {
			if s := &c.Current.Operation.Steps[i]; s.Status == protocol.StepPending {
				s.Status, s.Error = protocol.StepDeferred, reason
			}
		}
	})
}
