package controller

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Audit retention: entries older than the retention are deleted, a small
// batch per transaction with a pause between batches, so no write waits
// long behind them.
const (
	retentionInterval = time.Hour
	retentionBatch    = 1000
	retentionPause    = 50 * time.Millisecond
)

// auditPruner is what the retention deletes from.
type auditPruner interface {
	DeleteAuditBefore(ctx context.Context, before time.Time, limit int) (int64, error)
}

// auditRetention deletes old audit entries from the database.
type auditRetention struct {
	log       logrus.FieldLogger
	store     auditPruner
	retention time.Duration
	interval  time.Duration
	pause     time.Duration
	now       func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newAuditRetention(log logrus.FieldLogger, st auditPruner, retention time.Duration) *auditRetention {
	return &auditRetention{
		log:       log.WithField("component", "audit-retention"),
		store:     st,
		retention: retention,
		interval:  retentionInterval,
		pause:     retentionPause,
		now:       time.Now,
	}
}

// Start prunes now and every interval until Stop; a retention of 0 keeps
// every entry and starts nothing.
func (r *auditRetention) Start(ctx context.Context) error {
	if r.retention <= 0 {
		return nil
	}

	ctx, r.cancel = context.WithCancel(context.WithoutCancel(ctx))

	r.wg.Add(1)

	go func() {
		defer r.wg.Done()

		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()

		for {
			r.prune(ctx)

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
func (r *auditRetention) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}

	r.wg.Wait()

	return nil
}

// prune deletes the entries older than the retention, batch by batch.
func (r *auditRetention) prune(ctx context.Context) {
	before := r.now().Add(-r.retention)

	var total int64

	for ctx.Err() == nil {
		n, err := r.store.DeleteAuditBefore(ctx, before, retentionBatch)
		if err != nil {
			if ctx.Err() == nil {
				r.log.WithError(err).Warn("Could not delete old audit entries")
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
		r.log.WithFields(logrus.Fields{"deleted": total, "before": before}).Info("Deleted old audit entries")
	}
}
