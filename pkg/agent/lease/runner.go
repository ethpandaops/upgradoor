package lease

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of the single calls an operation makes, and of its waits.
const (
	containersTimeout = 30 * time.Second
	systemTimeout     = 30 * time.Second

	// persistAttempts is how often a write that must precede a disruptive
	// call is tried.
	persistAttempts = 3

	// The poll while waiting: a fifth of the verify window, within bounds.
	pollPerWindow = 5
	minPoll       = 5 * time.Millisecond
	maxPoll       = time.Second

	// ownLeasePrefix starts the ID of a lease the agent gave itself, which
	// has ownLeaseTTL to start.
	ownLeasePrefix = "fallback-"
	ownLeaseTTL    = 10 * time.Minute
)

// errInterrupted ends an operation run without a verdict while the runner
// stops; the persisted phase says where to go on.
var errInterrupted = errors.New("lease: operation interrupted")

// Overrides is the overrides file as it is now; local.Files satisfies it.
type Overrides interface {
	Load() plan.Overrides
}

// Pulls hears how the pulls of a lease went; registry.Tracker satisfies it.
type Pulls interface {
	NotePull(ref string, err error)
}

// Audit records how requested needs ended; local.Audit satisfies it.
type Audit interface {
	LeaseResult(lease string, need *protocol.Need, step *protocol.Step)
}

// Deps are what the runner is handed; every field is required.
type Deps struct {
	Host      host.Host
	Policy    *plan.Policy
	Overrides Overrides
	Registry  Pulls
	Audit     Audit
	// Notify tells the loop to sync now.
	Notify func()
	Now    func() time.Time
}

// Runner carries out the lease in the state file in its own goroutine.
type Runner interface {
	// Start runs the goroutine and resumes an operation from the state file;
	// one found in draining or applying fails its running step. The end of
	// ctx behaves like Stop.
	Start(ctx context.Context) error
	// Stop ends the goroutine and waits for it. Draining, applying and
	// issuing the reboot are carried through; waits end at once and resume
	// at the next Start.
	Stop() error

	// Follow takes the lease of one answer (nil for none) under the state
	// file's lock and reports whether a new lease was taken; reported is the
	// operation that request reported.
	//
	//	agent holds      answer's lease   outcome
	//	nothing          new              taken once persisted
	//	L                L with Cancel    L cancelled unless terminal
	//	L, not started   none or another  L withdrawn, never started
	//	L, started       none or another  L finishes first
	//	L, terminal      none or another  L forgotten once reported
	//	                                  terminal; then as "nothing"
	//
	// A lease without ID or with Cancel is never taken. Every call wakes the
	// goroutine, which retries a failed start or decline write.
	Follow(lease *protocol.Lease, reported *protocol.Operation) (taken bool)
	// Grant takes a lease the agent gives itself for needs, while the
	// controller cannot be reached, and reports whether it was taken: only
	// when no lease is held or the held one is finished. Follow treats it
	// like any other lease the controller does not offer.
	Grant(needs []protocol.Need) (taken bool)
	// Current returns a copy of the held lease record, or nil.
	Current() *Record
}

// runner is the Runner.
type runner struct {
	log  logrus.FieldLogger
	cfg  *Config
	st   *store
	deps *Deps
	// poll is the pause between looks while an operation waits.
	poll time.Duration
	// wakeup holds at most one pending wake of the goroutine.
	wakeup chan struct{}
	// ignored is the lease last logged as not taken; guarded by st.mu.
	ignored string

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Runner = (*runner)(nil)

// New returns the runner on the state file state was opened on.
func New(log logrus.FieldLogger, cfg *Config, state State, deps *Deps) Runner {
	return &runner{
		log:    log.WithField("package", "lease"),
		cfg:    cfg,
		st:     state.file(),
		deps:   deps,
		poll:   min(max(cfg.Window/pollPerWindow, minPoll), maxPoll),
		wakeup: make(chan struct{}, 1),
	}
}

// Start implements Runner.
func (r *runner) Start(ctx context.Context) error {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()

	if r.cancel != nil {
		return errors.New("lease: runner already started")
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.cancel, r.done = cancel, done

	go func() {
		defer close(done)

		for ctx.Err() == nil {
			r.work(ctx)

			select {
			case <-ctx.Done():
				return
			case <-r.wakeup:
			}
		}
	}()

	return nil
}

// Stop implements Runner.
func (r *runner) Stop() error {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()

	if r.cancel == nil {
		return nil
	}

	r.cancel()
	<-r.done
	r.cancel, r.done = nil, nil

	return nil
}

// Current implements Runner.
func (r *runner) Current() *Record { return r.st.current() }

func (r *runner) wake() {
	select {
	case r.wakeup <- struct{}{}:
	default:
	}
}

// Follow implements Runner.
func (r *runner) Follow(lease *protocol.Lease, reported *protocol.Operation) bool {
	defer r.wake()

	r.st.mu.Lock()
	defer r.st.mu.Unlock()

	if cur := r.st.cur.Current; cur != nil {
		log := r.log.WithField("lease", cur.Lease.ID)
		terminal := cur.Operation.Phase.Terminal()

		switch {
		case lease != nil && lease.ID == cur.Lease.ID:
			if lease.Cancel && !cur.Cancelled && !terminal {
				r.cancelLocked(log)
			}

			return false
		case !terminal && !cur.Started:
			if !cur.Withdrawn {
				r.withdrawLocked(log)
			}

			return false
		case !terminal:
			if lease != nil && r.ignored != lease.ID {
				r.ignored = lease.ID

				r.log.WithFields(logrus.Fields{"lease": lease.ID, "current": cur.Lease.ID}).
					Warn("another lease offered while an operation is open; finishing first")
			}

			return false
		case reported == nil || reported.LeaseID != cur.Lease.ID || !reported.Phase.Terminal():
			// Not forgotten before the controller was told the outcome.
			return false
		}

		if err := r.st.updateLocked(func(c *content) { c.Current = nil }); err != nil {
			log.WithError(err).Error("cannot drop the finished operation")

			return false
		}

		log.WithField("phase", cur.Operation.Phase).
			Info("lease released by the controller; operation forgotten")
	}

	if lease == nil {
		return false
	}

	return r.takeLocked(lease)
}

// Grant implements Runner.
func (r *runner) Grant(needs []protocol.Need) bool {
	defer r.wake()

	r.st.mu.Lock()
	defer r.st.mu.Unlock()

	if cur := r.st.cur.Current; cur != nil && !cur.Operation.Phase.Terminal() {
		return false
	}

	if len(needs) == 0 {
		return false
	}

	now := r.deps.Now()

	return r.takeLocked(&protocol.Lease{
		ID:         fmt.Sprintf("%s%d", ownLeasePrefix, now.UnixMilli()),
		GrantedAt:  now,
		ExpiresAt:  now.Add(ownLeaseTTL),
		TTLSeconds: int64(ownLeaseTTL / time.Second),
		Needs:      needs,
	})
}

// cancelLocked marks the lease cancelled, in memory even when the write
// fails.
func (r *runner) cancelLocked(log logrus.FieldLogger) {
	err := r.st.applyLocked(func(c *content) {
		c.Current.Cancelled = true
		c.Current.Lease.Cancel = true
	})
	if err != nil {
		log.WithError(err).Error("cannot persist that the lease was cancelled; honouring it all the same")
	}

	log.Warn("the lease was cancelled; no further step of it is begun")
}

// withdrawLocked marks an unstarted lease withdrawn, in memory even when the
// write fails; the goroutine declines it.
func (r *runner) withdrawLocked(log logrus.FieldLogger) {
	if err := r.st.applyLocked(func(c *content) { c.Current.Withdrawn = true }); err != nil {
		log.WithError(err).Error("cannot persist that the lease was withdrawn; honouring it all the same")
	}

	log.Warn("the controller no longer offers the lease; it will not be started")
}

// takeLocked persists a new lease with its deadline; the goroutine acts on
// it only once it is on disk.
func (r *runner) takeLocked(lease *protocol.Lease) bool {
	log := r.log.WithField("lease", lease.ID)

	if lease.ID == "" {
		log.Warn("ignoring a lease without an ID")

		return false
	}

	if lease.Cancel {
		if r.ignored != lease.ID {
			r.ignored = lease.ID

			log.Warn("ignoring a lease that was cancelled before the agent had it")
		}

		return false
	}

	rec := &Record{
		Lease:    *lease,
		Deadline: leaseDeadline(lease, r.deps.Now()),
		Operation: protocol.Operation{
			LeaseID: lease.ID,
			Steps:   make([]protocol.Step, 0, len(lease.Needs)),
		},
	}
	rec.Lease.Needs = plan.CloneNeeds(lease.Needs)

	for i := range lease.Needs {
		rec.Operation.Steps = append(rec.Operation.Steps, protocol.Step{
			NeedID: lease.Needs[i].ID, Status: protocol.StepPending,
		})
	}

	if err := r.st.updateLocked(func(c *content) { c.Current = rec }); err != nil {
		log.WithError(err).Error("cannot persist the lease; not acting on it")

		return false
	}

	log.WithFields(logrus.Fields{
		"needs":    len(lease.Needs),
		"deadline": rec.Deadline.UTC().Format(time.RFC3339),
	}).Info("lease received")

	return true
}

// leaseDeadline is the earlier of ExpiresAt and receipt plus TTL, which
// tolerates a controller clock that is off in either direction.
func leaseDeadline(lease *protocol.Lease, received time.Time) time.Time {
	deadline := received.Add(time.Duration(max(lease.TTLSeconds, 0)) * time.Second)

	if !lease.ExpiresAt.IsZero() && lease.ExpiresAt.Before(deadline) {
		deadline = lease.ExpiresAt
	}

	return deadline
}

func (r *runner) work(ctx context.Context) {
	rec := r.st.current()
	if rec == nil || rec.Operation.Phase.Terminal() {
		return
	}

	o := &operation{r: r, log: r.log.WithField("lease", rec.Lease.ID)}

	var err error

	if rec.Started {
		err = o.resume(ctx, rec)
	} else {
		err = o.begin(ctx, rec)
	}

	if errors.Is(err, errInterrupted) {
		o.log.Warn("operation interrupted; it continues when the agent runs again")
	}
}

// operation is one run of the runner on one lease.
type operation struct {
	r   *runner
	log logrus.FieldLogger

	// lp is what the lease does; zero for a resumed operation.
	lp plan.LeasePlan
	// failed skips the remaining steps; the containers are still brought back.
	failed bool
	// cancelled skips the remaining steps as after a failure.
	cancelled bool
	// holdReason is set when a local hold cancelled the operation.
	holdReason string
	// deferred: an image could not be pulled, so nothing is begun.
	deferred bool
	// view is the last classified view, for order and stop timeouts.
	view *plan.View
}

// execute runs a started operation. Draining, applying and issuing the
// reboot are not cancelled by ctx; only the pulls and the waits are.
func (o *operation) execute(ctx context.Context) error {
	if err := o.pullImages(ctx); err != nil {
		return err
	}

	if o.deferred {
		return o.bringBack(ctx)
	}

	work := context.WithoutCancel(ctx)
	reboot := len(o.lp.Reboot) > 0

	if reboot {
		o.drain(work)
	}

	o.applyPackages(work)
	o.applyImages(work)

	if reboot && !o.stopped() && o.reboot(work) {
		if err := o.awaitReboot(ctx, o.lp.Reboot); err != nil {
			return err
		}
	}

	return o.bringBack(ctx)
}

// resume continues an operation from the state file after a restart or reboot.
func (o *operation) resume(ctx context.Context, rec *Record) error {
	phase := rec.Operation.Phase
	o.failed = rec.Operation.Error != ""
	// A cancel persisted by an earlier process still holds.
	o.cancelled = rec.Cancelled

	o.log.WithField("phase", phase).Info("resuming operation from the state file")

	switch phase {
	case protocol.PhaseRebooting:
		if err := o.awaitReboot(ctx, stepIDs(rec, protocol.StepRunning)); err != nil {
			return err
		}
	case protocol.PhasePulling:
		// Nothing was stopped yet: the lease is not begun.
		o.deferSteps(reasonPullInterrupted)
	case protocol.PhaseDraining, protocol.PhaseApplying:
		// Only a kill or crash leaves these phases; the running step's
		// outcome is unknown, so it failed.
		o.fail(fmt.Sprintf("the agent restarted during phase %s", phase))
		o.setSteps(stepIDs(rec, protocol.StepRunning), protocol.StepFailed,
			"interrupted: the agent restarted during this step")
	default:
	}

	return o.bringBack(ctx)
}

func stepIDs(rec *Record, status protocol.StepStatus) []string {
	ids := make([]string, 0, len(rec.Operation.Steps))

	for _, s := range rec.Operation.Steps {
		if s.Status == status {
			ids = append(ids, s.NeedID)
		}
	}

	return ids
}

func (o *operation) now() time.Time { return o.r.deps.Now() }

// apply changes the operation in memory and on disk; a failed write is
// logged and the operation goes on.
func (o *operation) apply(fn func(c *content)) {
	err := o.r.st.apply(func(c *content) {
		if c.Current == nil {
			return
		}

		fn(c)
		c.Current.Operation.UpdatedAt = o.now()
	})
	if err != nil {
		o.log.WithError(err).Error("cannot persist the operation state; continuing")
	}
}

func (o *operation) setPhase(phase protocol.Phase) {
	changed := false

	o.apply(func(c *content) {
		changed = c.Current.Operation.Phase != phase
		c.Current.Operation.Phase = phase
	})

	if changed {
		o.log.WithField("phase", phase).Info("operation phase")
		o.r.deps.Notify()
	}
}

func (o *operation) setSteps(ids []string, status protocol.StepStatus, reason string) {
	if len(ids) == 0 {
		return
	}

	o.apply(func(c *content) { markSteps(c.Current, ids, status, reason) })
}

func markSteps(rec *Record, ids []string, status protocol.StepStatus, reason string) {
	for i := range rec.Operation.Steps {
		if slices.Contains(ids, rec.Operation.Steps[i].NeedID) {
			rec.Operation.Steps[i].Status = status
			rec.Operation.Steps[i].Error = reason
		}
	}
}

// stopped reports whether no further step is to be begun.
func (o *operation) stopped() bool { return o.failed || o.cancelled }

func (o *operation) noteCancel() {
	if !o.cancelled {
		o.cancelled = true

		o.log.Warn("the lease was cancelled: beginning no further step, bringing the host back")
	}
}

// held reports whether a local hold was set; it cancels the operation. Only
// an explicit hold counts: an overrides file that cannot be read refuses a
// lease before its start, but does not end one that runs. The controller
// hold does not stop a lease it granted.
func (o *operation) held() bool {
	if o.holdReason != "" {
		return true
	}

	files := o.r.deps.Overrides.Load()
	if !files.Known {
		return false
	}

	hold := plan.HoldState(&files, o.now())
	if !hold.Local {
		return false
	}

	o.holdReason = plan.HoldReason(hold)
	o.cancelled = true

	o.log.WithField("reason", o.holdReason).
		Warn("the host is on local hold: beginning no further step, bringing the host back")

	return true
}

// proceed reports whether a disruptive call may be made. It marks the steps
// ids running under the lock that reads the cancel mark, so a cancel either
// prevents the call or finds the steps begun. A local hold counts as a cancel.
func (o *operation) proceed(ids []string) bool {
	if o.cancelled || o.held() {
		return false
	}

	ok := false

	o.apply(func(c *content) {
		if c.Current.Cancelled {
			return
		}

		ok = true

		markSteps(c.Current, ids, protocol.StepRunning, "")
	})

	if !ok {
		o.noteCancel()
	}

	return ok
}

// fail records the first failure of the operation.
func (o *operation) fail(reason string) {
	o.failed = true

	o.log.WithField("error", reason).Error("operation step failed")

	o.apply(func(c *content) {
		if c.Current.Operation.Error == "" {
			c.Current.Operation.Error = reason
		}
	})
}

// markDisrupted reports whether a call that stops something may be made:
// only with the disrupted mark on disk, so that a later process brings the
// containers back. When the write fails the steps ids fail untouched.
func (o *operation) markDisrupted(ctx context.Context, ids []string) bool {
	if o.disrupted() {
		return true
	}

	var err error

	for attempt := range persistAttempts {
		if attempt > 0 {
			sleep(ctx, o.r.poll)
		}

		err = o.r.st.update(func(c *content) {
			if c.Current != nil {
				c.Current.Operation.Disrupted = true
				c.Current.Operation.UpdatedAt = o.now()
			}
		})
		if err == nil {
			return true
		}
	}

	// The error names a varying temp file: log it, keep it out of the report.
	o.log.WithError(err).Error("cannot persist the state before the first disruptive call")

	const msg = "cannot write the state file before the first disruptive call; nothing was touched"

	o.fail(msg)
	o.setSteps(ids, protocol.StepFailed, msg)

	return false
}

func (o *operation) disrupted() bool {
	rec := o.r.st.current()

	return rec != nil && rec.Operation.Disrupted
}

// touch adds containers to the set the verifying phase watches.
func (o *operation) touch(names []string) {
	o.apply(func(c *content) {
		for _, name := range names {
			if !slices.Contains(c.Current.Touched, name) {
				c.Current.Touched = append(c.Current.Touched, name)
			}
		}
	})
}

func (o *operation) containers(ctx context.Context) ([]protocol.Container, error) {
	cctx, cancel := context.WithTimeout(ctx, containersTimeout)
	defer cancel()

	return o.r.deps.Host.Containers(cctx)
}

func (o *operation) system(ctx context.Context) (*plan.System, error) {
	cctx, cancel := context.WithTimeout(ctx, systemTimeout)
	defer cancel()

	sys, err := o.r.deps.Host.System(cctx)
	if err != nil {
		return nil, err
	}

	out := plan.System(sys)

	return &out, nil
}

// classify builds and keeps the view of a fresh list, without registry checks.
func (o *operation) classify(list []protocol.Container, sys *plan.System) *plan.View {
	files := o.r.deps.Overrides.Load()
	view := plan.Classify(o.r.deps.Policy,
		&plan.Facts{Containers: list, Fresh: true, System: sys}, &files, nil, o.now())
	o.view = &view

	return &view
}

// classified are the containers of the last view; nil before a look.
func (o *operation) classified() []protocol.Container {
	if o.view == nil {
		return nil
	}

	return o.view.Containers
}

func (o *operation) stopTimeout(name string) time.Duration {
	var c *protocol.Container
	if o.view != nil {
		c = o.view.Container(name)
	}

	return plan.StopTimeout(o.r.deps.Policy, c)
}

func (o *operation) hold() protocol.Hold {
	files := o.r.deps.Overrides.Load()

	return plan.HoldState(&files, o.now())
}

// sleep waits for d; it reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// host.Change and plan.Recreation must stay convertible.
var _ = host.Change(plan.Recreation{})
