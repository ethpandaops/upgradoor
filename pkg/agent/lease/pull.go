package lease

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// pullMargin is kept of the lease's TTL for the report after the pulls.
	pullMargin = 2 * time.Minute
	// pullFallbackBudget bounds the pulls of a lease without a TTL.
	pullFallbackBudget = 45 * time.Minute
	// progressEvery is the least time between two writes of pull progress.
	progressEvery = 30 * time.Second
)

// defaultPullBackoff are the pauses before the retries of a stalled or
// transiently failed pull; their number is the number of retries.
var defaultPullBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

// keyImage is the log field of an image reference.
const keyImage = "image"

// reasonPullInterrupted defers the steps of an operation whose pulls a
// restart of the agent cut short.
const reasonPullInterrupted = "the agent restarted while the images were pulled"

var (
	// errPullCancelled ends a pull of a lease that was cancelled.
	errPullCancelled = errors.New("the lease was cancelled during the pull")
	// errPullBudget ends the pulls that outlast the lease's pull budget.
	errPullBudget = errors.New("the pulls did not finish within the lease's time")
)

// Pull errors worth another attempt, and those that are not even when they
// also match one of them. Anything else is not retried.
var (
	transientPull = regexp.MustCompile(`connection reset|connection refused|broken pipe|\beof\b|` +
		`timeout|timed out|toomanyrequests|too many requests|internal server error|bad gateway|` +
		`service unavailable|temporarily unavailable`)
	permanentPull = regexp.MustCompile(`not found|manifest unknown|unauthorized|denied|` +
		`authentication required|invalid reference|no such image`)
)

// retryablePull reports whether a failed pull is tried again: a stall or a
// network or registry error that is likely to pass.
func retryablePull(err error) bool {
	if errors.Is(err, host.ErrPullStalled) {
		return true
	}

	if errors.Is(err, errPullCancelled) || errors.Is(err, errPullBudget) {
		return false
	}

	msg := strings.ToLower(err.Error())

	return transientPull.MatchString(msg) && !permanentPull.MatchString(msg)
}

// pullBudget is how long the pulls of a lease may take: its TTL less the
// margin for the report, at least half the TTL.
func pullBudget(lease *protocol.Lease) time.Duration {
	ttl := time.Duration(lease.TTLSeconds) * time.Second
	if ttl <= 0 {
		return pullFallbackBudget
	}

	return max(ttl-pullMargin, ttl/2)
}

// pullImages makes the image of every recreate local at its digest, before
// anything is stopped. When one cannot be pulled, every open step is
// deferred and nothing of the lease is begun. A recreate whose tag moved on
// is superseded. The pulls end with ctx, with a cancel of the lease and at
// the lease's pull budget.
func (o *operation) pullImages(ctx context.Context) error {
	if len(o.lp.Recreates) == 0 {
		return nil
	}

	defer o.showPull(nil)

	list, err := o.containers(ctx)
	if err != nil {
		o.deferSteps(fmt.Sprintf("cannot list containers before the pull: %v", err))

		return nil
	}

	budget := pullFallbackBudget
	if rec := o.r.st.current(); rec != nil {
		budget = pullBudget(&rec.Lease)
	}

	end := o.now().Add(budget)
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

		got, err := o.pull(ctx, ref, task.Digest, end)
		if ctx.Err() != nil {
			return errInterrupted
		}

		switch {
		case errors.Is(err, errPullCancelled):
			o.noteCancel()

			return nil
		case errors.Is(err, errPullBudget):
			o.deferSteps(fmt.Sprintf("pull %s: %v; the lease leaves %s for its pulls", ref, err, budget))

			return nil
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
// unless it is local at digest, and tells the tracker how a pull went. A
// stalled or transiently failed pull is tried again after a pause, while
// end leaves room for it.
func (o *operation) pull(ctx context.Context, ref, digest string, end time.Time) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, containersTimeout)
	got, _, err := o.r.deps.Host.LocalImage(cctx, ref)

	cancel()

	if err == nil && got == digest {
		return got, nil
	}

	log := o.log.WithField(keyImage, ref)
	log.WithField("digest", digest).Info("pulling image")

	wctx, stop := o.watchCancel(ctx)
	defer stop()

	started := o.now()
	shown := &protocol.PullProgress{Image: ref, StartedAt: started}

	attempt := 1

	for ; ; attempt++ {
		next := *shown
		next.Attempt, next.LastProgressAt = attempt, o.now()
		o.showPull(&next)

		got, err = o.pullOnce(wctx, ref, end)
		shown = o.pulling

		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		if err == nil || attempt > len(o.r.pullBackoff) || !retryablePull(err) {
			break
		}

		pause := o.r.pullBackoff[attempt-1]
		if !o.now().Add(pause).Before(end) {
			break
		}

		log.WithError(err).WithFields(logrus.Fields{"attempt": attempt, "pause": pause}).
			Warn("image pull failed; trying again")

		if !sleep(wctx, pause) {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}

			return "", errPullCancelled
		}
	}

	if errors.Is(err, errPullCancelled) {
		return "", err
	}

	if err != nil && attempt > 1 {
		err = fmt.Errorf("%w, attempt %d", err, attempt)
	}

	o.r.deps.Registry.NotePull(ref, err)

	if err != nil {
		log.WithError(err).Warn("image pull failed; nothing of the lease is begun")
	}

	return got, err
}

// pullOnce is one attempt, bounded by end; a cancel of the lease ends it with
// errPullCancelled.
func (o *operation) pullOnce(ctx context.Context, ref string, end time.Time) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, end.Sub(o.now()))
	defer cancel()

	got, _, err := o.r.deps.Host.Pull(cctx, ref, o.noteProgress)

	switch {
	case err == nil:
		return got, nil
	case errors.Is(context.Cause(ctx), errPullCancelled):
		return "", errPullCancelled
	case ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded):
		return "", errPullBudget
	default:
		return "", err
	}
}

// watchCancel returns ctx ended with errPullCancelled once the lease is
// cancelled; stop ends the watch.
func (o *operation) watchCancel(ctx context.Context) (wctx context.Context, stop func()) {
	wctx, cancel := context.WithCancelCause(ctx)

	var wg sync.WaitGroup

	wg.Go(func() {
		ticker := time.NewTicker(o.r.poll)
		defer ticker.Stop()

		for {
			select {
			case <-wctx.Done():
				return
			case <-ticker.C:
				if cur := o.r.st.current(); cur != nil && cur.Cancelled {
					cancel(errPullCancelled)

					return
				}
			}
		}
	})

	return wctx, func() {
		cancel(nil)
		wg.Wait()
	}
}

// noteProgress takes in the counts of the pull in flight; they are written
// at most every progressEvery.
func (o *operation) noteProgress(p *protocol.PullProgress) {
	if o.pulling == nil {
		return
	}

	next := *o.pulling
	next.BytesDone, next.BytesTotal = p.BytesDone, p.BytesTotal
	next.LayersDone, next.LayersTotal = p.LayersDone, p.LayersTotal
	next.LastProgressAt = o.now()

	if next.LastProgressAt.Sub(o.pullShownAt) < progressEvery {
		o.pulling = &next

		return
	}

	o.showPull(&next)
}

// showPull writes the pull in flight into the operation; nil clears it.
func (o *operation) showPull(p *protocol.PullProgress) {
	if p == nil && o.pulling == nil {
		return
	}

	o.pulling, o.pullShownAt = p, o.now()

	var shown *protocol.PullProgress

	if p != nil {
		c := *p
		shown = &c
	}

	o.apply(func(c *content) { c.Current.Operation.Pull = shown })
}

// deferSteps defers every step not settled yet: the operation ends without
// having begun anything.
func (o *operation) deferSteps(reason string) {
	o.deferred = true

	o.apply(func(c *content) {
		c.Current.Operation.Pull = nil

		for i := range c.Current.Operation.Steps {
			if s := &c.Current.Operation.Steps[i]; s.Status == protocol.StepPending {
				s.Status, s.Error = protocol.StepDeferred, reason
			}
		}
	})
}
