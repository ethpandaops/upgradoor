package controller

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Retention: rows older than their retention are deleted, a small batch per
// transaction with a pause between batches, so no write waits long behind
// them.
const (
	retentionInterval = time.Hour
	retentionBatch    = 1000
	retentionPause    = 50 * time.Millisecond
)

// pruneFunc deletes at most limit rows older than before and returns how
// many it deleted.
type pruneFunc func(ctx context.Context, before time.Time, limit int) (int64, error)

// pruned is one kind of row the retention deletes.
type pruned struct {
	// what names the rows in the log.
	what string
	// keep is how long they stay; 0 keeps them.
	keep   time.Duration
	delete pruneFunc
}

// retention deletes old rows from the database: the audit entries and the
// SSH login groups.
type retention struct {
	log      logrus.FieldLogger
	targets  []pruned
	interval time.Duration
	pause    time.Duration
	now      func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newRetention prunes the targets with a positive keep.
func newRetention(log logrus.FieldLogger, targets ...pruned) *retention {
	kept := make([]pruned, 0, len(targets))

	for _, t := range targets {
		if t.keep > 0 {
			kept = append(kept, t)
		}
	}

	return &retention{
		log:      log.WithField("component", "retention"),
		targets:  kept,
		interval: retentionInterval,
		pause:    retentionPause,
		now:      time.Now,
	}
}

// Start prunes now and every interval until Stop; without a target it
// starts nothing.
func (r *retention) Start(ctx context.Context) error {
	if len(r.targets) == 0 {
		return nil
	}

	ctx, r.cancel = context.WithCancel(context.WithoutCancel(ctx))

	r.wg.Add(1)

	go func() {
		defer r.wg.Done()

		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()

		for {
			for i := range r.targets {
				r.prune(ctx, &r.targets[i])
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return nil
}

// Stop ends the pruning and waits for it.
func (r *retention) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}

	r.wg.Wait()

	return nil
}

// prune deletes a target's rows older than its retention, batch by batch.
func (r *retention) prune(ctx context.Context, t *pruned) {
	before := r.now().Add(-t.keep)

	var total int64

	for ctx.Err() == nil {
		n, err := t.delete(ctx, before, retentionBatch)
		if err != nil {
			if ctx.Err() == nil {
				r.log.WithError(err).WithField("rows", t.what).Warn("Could not delete old rows")
			}

			break
		}

		total += n

		if n < retentionBatch {
			break
		}

		select {
		case <-ctx.Done():
		case <-time.After(r.pause):
		}
	}

	if total > 0 {
		r.log.WithFields(logrus.Fields{"rows": t.what, "deleted": total, "before": before}).
			Info("Deleted old rows")
	}
}
