// Package operation executes a lease: the needs it grants, in one downtime,
// by phase (plan.go, drain.go, apply.go, reboot.go, start.go, verify.go).
//
// The operation is safety-critical. It persists its progress before it acts
// (the lease before it starts, the "disrupted" mark before the first call
// that stops something, the rebooting phase before the reboot), honours a
// cancel before every disruptive call, and resumes from the state file after
// a restart of the agent or a reboot of the host.
package operation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of the single calls an operation makes.
const (
	observeTimeout = 30 * time.Second
	statusTimeout  = 30 * time.Second
	startTimeout   = 2 * time.Minute
	stopGrace      = time.Minute
	recreateGrace  = 10 * time.Minute
	upgradeTimeout = 30 * time.Minute
	rebootTimeout  = 2 * time.Minute

	// defaultRebootGrace is how long after issuing a reboot an unchanged
	// boot id is still taken as "the host is about to go down".
	defaultRebootGrace = 5 * time.Minute

	// defaultStopTimeout is used for a container whose configuration
	// disappeared while an operation was open.
	defaultStopTimeout = 300 * time.Second

	persistAttempts = 3

	// logContainer is the log field naming a container.
	logContainer = "container"
)

// errInterrupted ends an operation run without a verdict: the agent is
// shutting down (or the host is). The persisted phase says where to go on.
//
// Only the waiting parts of an operation can be interrupted: planning (not
// started yet), waiting for the reboot, and the waits of starting and
// verifying. Draining, applying and issuing the reboot always run to their
// end, see execute.
var errInterrupted = errors.New("agent: operation interrupted")

// Container orders: start the execution client first and the validator
// client last, stop them the other way round.
var (
	startOrder = []protocol.ContainerKind{
		protocol.KindEL, protocol.KindCL, protocol.KindVC, protocol.KindOther,
	}
	drainOrder = []protocol.ContainerKind{
		protocol.KindVC, protocol.KindCL, protocol.KindEL, protocol.KindOther,
	}
)

// Operation executes the lease in the state file. One runs at a time, in its
// own goroutine; the sync loop reports its persisted phase meanwhile.
type Operation interface {
	// Run works on the current lease until it is terminal or ctx ends. The
	// end of ctx does not cut a disruptive step short: Run returns only once
	// the draining and applying phases are through.
	Run(ctx context.Context)
}

// Holds is the hold state of the host, which an operation checks before it
// starts.
type Holds interface {
	Hold() protocol.Hold
}

// Deps are what an operation works with.
type Deps struct {
	Log      logrus.FieldLogger
	Config   *config.Config
	Runtime  hostapi.ContainerRuntime
	Registry hostapi.Registry
	System   hostapi.System
	Holds    Holds
	Store    journal.Store
	Now      func() time.Time
	// Poll is the pause between looks while waiting for something.
	Poll time.Duration
	// Notify tells the sync loop that the phase changed.
	Notify func()
	// Audit is the host's audit log; nil writes none.
	Audit auditlog.Log
}

// New returns the operation on the lease in the state file.
func New(d *Deps) Operation {
	return &operation{
		log:      d.Log,
		cfg:      d.Config,
		runtime:  d.Runtime,
		registry: d.Registry,
		system:   d.System,
		holds:    d.Holds,
		store:    d.Store,
		now:      d.Now,
		poll:     d.Poll,
		notify:   d.Notify,
		audit:    d.Audit,
	}
}

// operation is the Operation.
type operation struct {
	log      logrus.FieldLogger
	cfg      *config.Config
	runtime  hostapi.ContainerRuntime
	registry hostapi.Registry
	system   hostapi.System
	holds    Holds
	store    journal.Store
	now      func() time.Time
	// poll is the pause between looks while waiting for something.
	poll time.Duration
	// notify tells the sync loop that the phase changed.
	notify func()
	// audit is the host's audit log; nil writes none.
	audit auditlog.Log

	// failed is true once a step failed; the remaining steps are skipped,
	// the containers are still brought back.
	failed bool
	// cancelled is true once a step was not begun because the lease was
	// cancelled; the remaining steps are skipped like after a failure, and
	// the containers are brought back the same way.
	cancelled bool
	// seen are the containers as the operation last observed them, by name.
	seen map[string]protocol.Container
}

var _ Operation = (*operation)(nil)

// Run works on the current lease until it is terminal or ctx ends. The end
// of ctx does not cut a disruptive step short: Run returns only once the
// draining and applying phases are through (see execute).
func (o *operation) Run(ctx context.Context) {
	st := o.store.Snapshot()

	rec := st.Current
	if rec == nil || rec.Operation.Phase.Terminal() {
		return
	}

	o.log = o.log.WithField("lease", rec.Lease.ID)

	var err error

	if rec.Started {
		err = o.resume(ctx, rec)
	} else {
		err = o.begin(ctx, &st)
	}

	if errors.Is(err, errInterrupted) {
		o.log.Warn("operation interrupted; it continues when the agent runs again")
	}
}

// execute runs a started operation from the top.
//
// Once started, finish: stopping the agent (a restart of the unit, a new
// binary or configuration being deployed) must not cut a container
// recreate or a package upgrade in half, nor leave the plan half done. The
// draining and applying phases and the issuing of the reboot therefore run
// on a context that the end of ctx does not cancel; each call in them is
// bounded by its own timeout. The agent's Stop waits for them. Only what
// follows can be interrupted, because the state file says how to go on:
// waiting for the reboot and the waits of starting and verifying.
//
// A cancelled lease (Lease.Cancel) is the one thing that stops a started
// operation short: every disruptive call (the stop of a container ahead of a
// reboot, the package upgrade, a recreate, the reboot) first asks proceed,
// and once that says no, nothing further is begun. What was begun is
// completed, and bringBack runs as after a failed step: the containers that
// ran before the operation are started again.
func (o *operation) execute(ctx context.Context, plan *operationPlan) error {
	work := context.WithoutCancel(ctx)

	if len(plan.reboot) > 0 {
		o.drain(work, plan)
	}

	o.applyPackages(work, plan)
	o.applyImages(work, plan)

	if len(plan.reboot) > 0 && !o.stopped() {
		if o.reboot(work, plan) {
			if err := o.awaitReboot(ctx, plan.reboot); err != nil {
				return err
			}
		}
	}

	return o.bringBack(ctx)
}

// resume continues an operation found in the state file after a restart of
// the agent, which is how a reboot looks from here.
func (o *operation) resume(ctx context.Context, rec *journal.Record) error {
	phase := rec.Operation.Phase
	o.failed = rec.Operation.Error != ""
	// A cancel that the process before this one took in still holds: the
	// steps it kept from beginning were not begun because of it, and are
	// reported as cancelled (finish).
	o.cancelled = rec.Cancelled

	o.log.WithField("phase", phase).Info("resuming operation from the state file")

	switch phase {
	case protocol.PhaseRebooting:
		if err := o.awaitReboot(ctx, stepIDs(rec, protocol.StepRunning)); err != nil {
			return err
		}
	case protocol.PhaseDraining, protocol.PhaseApplying:
		// These phases are never left by a regular stop of the agent (see
		// execute), so the process was killed or the host went down in the
		// middle of a step. What that step left behind is unknown, so it
		// counts as failed; bring the containers back.
		o.fail(fmt.Sprintf("the agent restarted during phase %s", phase))
		o.setSteps(stepIDs(rec, protocol.StepRunning), protocol.StepFailed,
			"interrupted: the agent restarted during this step")
	case protocol.PhaseStarting, protocol.PhaseVerifying,
		protocol.PhaseDone, protocol.PhaseFailed:
	}

	return o.bringBack(ctx)
}

func stepIDs(rec *journal.Record, status protocol.StepStatus) []string {
	ids := make([]string, 0, len(rec.Operation.Steps))

	for _, s := range rec.Operation.Steps {
		if s.Status == status {
			ids = append(ids, s.NeedID)
		}
	}

	return ids
}

// finish makes the operation terminal and closes the manual needs it
// served.
func (o *operation) finish() {
	phase := protocol.PhaseDone
	if o.failed {
		phase = protocol.PhaseFailed
	}

	o.apply(func(st *journal.State) {
		cur := st.Current
		cur.Operation.Phase = phase

		for i := range cur.Operation.Steps {
			s := &cur.Operation.Steps[i]

			switch {
			case s.Status != protocol.StepPending:
			case o.cancelled && !o.failed:
				// Not begun because the lease was cancelled, and nothing
				// failed. After a failure the steps that were not begun are
				// reported as after any failure, whether or not a cancel
				// arrived as well and whichever process of the agent finishes
				// the operation: the report must not depend on a restart.
				s.Status = protocol.StepCancelled
				s.Error = reasonStepCancelled
			case phase == protocol.PhaseFailed && s.Error == "":
				s.Error = "not attempted: " + cur.Operation.Error
			}
		}

		closeManualNeeds(st)
	})

	rec := o.store.Snapshot().Current
	if rec == nil {
		return
	}

	o.auditResults(rec)

	log := o.log.WithFields(logrus.Fields{
		"phase":     phase,
		"disrupted": rec.Operation.Disrupted,
		"took":      o.now().Sub(rec.Operation.StartedAt).Round(time.Second),
	})

	switch {
	case o.failed:
		log.WithField("error", rec.Operation.Error).Error("operation failed")
	case o.cancelled:
		log.Warn("operation ended: the lease was cancelled, the remaining steps were not begun")
	default:
		log.Info("operation done")
	}

	o.notify()
}

// auditResults writes into the host's audit log how the needs that somebody
// asked for ended: the overrides, and the rolls and reboots of a user or a
// local command. What the controller rolls out by itself is not in it.
func (o *operation) auditResults(rec *journal.Record) {
	if o.audit == nil {
		return
	}

	for i := range rec.Lease.Needs {
		n := &rec.Lease.Needs[i]

		step := protocol.Step{NeedID: n.ID}
		for _, s := range rec.Operation.Steps {
			if s.NeedID == n.ID {
				step = s
			}
		}

		out := auditlog.Record{
			Source: auditlog.SourceAgent, Lease: rec.Lease.ID, Result: string(step.Status), Error: step.Error,
		}

		switch {
		case n.Kind == protocol.NeedOverride && n.Override != nil:
			out.Action = "override.apply"
			if n.Override.Restore {
				out.Action = "override.restore"
			}

			out.Container = n.Override.Container
			out.Before = cmp.Or(n.Override.FromImage, n.Override.Image) + "@" + n.Override.FromDigest
			out.After = n.Override.Image + "@" + n.Override.ToDigest

			if n.Override.Flags {
				out.After += ", command line changed"
			}
		case n.Kind == protocol.NeedSpec && n.Spec != nil:
			out.Action = "spec.apply"
			out.Container = n.Spec.Container
			out.Before = cmp.Or(n.Spec.FromImage, n.Spec.Image) + "@" + n.Spec.FromDigest
			out.After = n.Spec.Image + "@" + n.Spec.ToDigest

			if n.Spec.Flags {
				out.After += ", command line changed"
			}
		case n.Kind == protocol.NeedImage && n.Image != nil && (n.Local || n.DirectiveID != ""):
			out.Action = "roll.result"
			out.Container = n.Image.Container
			out.Directive = n.DirectiveID
			out.Before = n.Image.Image + "@" + n.Image.FromDigest
			out.After = n.Image.Image + "@" + n.Image.ToDigest
		case n.Kind == protocol.NeedReboot && n.DirectiveID != "":
			out.Action = "reboot.result"
			out.Directive = n.DirectiveID
		default:
			continue
		}

		o.audit.Record(out)
	}
}

// closeManualNeeds drops the manual needs a terminal operation served,
// successfully or not: a directive is good for one attempt.
func closeManualNeeds(st *journal.State) {
	cur := st.Current
	if cur == nil {
		return
	}

	served := make(map[string]struct{}, len(cur.Operation.Steps))

	for _, s := range cur.Operation.Steps {
		if s.Status == protocol.StepDone || s.Status == protocol.StepFailed {
			served[s.NeedID] = struct{}{}
		}
	}

	st.ManualNeeds = slices.DeleteFunc(st.ManualNeeds, func(n protocol.Need) bool {
		_, ok := served[n.ID]

		return ok
	})
}

// apply changes the operation in memory and on disk. A failed write is
// logged and the operation goes on: once started it finishes.
func (o *operation) apply(fn func(st *journal.State)) {
	err := o.store.Apply(func(st *journal.State) {
		if st.Current == nil {
			return
		}

		fn(st)
		st.Current.Operation.UpdatedAt = o.now()
	})
	if err != nil {
		o.log.WithError(err).Error("cannot persist the operation state; continuing")
	}
}

func (o *operation) setPhase(phase protocol.Phase) {
	changed := false

	o.apply(func(st *journal.State) {
		changed = st.Current.Operation.Phase != phase
		st.Current.Operation.Phase = phase
	})

	if changed {
		o.log.WithField("phase", phase).Info("operation phase")
		o.notify()
	}
}

func (o *operation) setSteps(ids []string, status protocol.StepStatus, reason string) {
	if len(ids) == 0 {
		return
	}

	o.apply(func(st *journal.State) { markSteps(st.Current, ids, status, reason) })
}

func markSteps(rec *journal.Record, ids []string, status protocol.StepStatus, reason string) {
	for i := range rec.Operation.Steps {
		if slices.Contains(ids, rec.Operation.Steps[i].NeedID) {
			rec.Operation.Steps[i].Status = status
			rec.Operation.Steps[i].Error = reason
		}
	}
}

// stopped reports whether no further step is to be begun: one failed, or the
// lease was cancelled.
func (o *operation) stopped() bool { return o.failed || o.cancelled }

// stop records that the lease was cancelled before a step had begun.
func (o *operation) stop() {
	if !o.cancelled {
		o.cancelled = true

		o.log.Warn("the lease was cancelled: beginning no further step, bringing the host back")
	}
}

// proceed is asked right before every disruptive call. It reports whether the
// call may be made, and in the same critical section of the state store marks
// the steps it begins as running: a cancel that the sync loop writes
// (service.cancelLease) either comes first, and the step is never begun, or
// comes after, and the step counts as begun and is completed. ids are the
// steps the call begins; none for the stop of a container ahead of a reboot,
// which belongs to no step of its own.
func (o *operation) proceed(ids []string) bool {
	if o.cancelled {
		return false
	}

	ok := false

	o.apply(func(st *journal.State) {
		if st.Current.Cancelled {
			return
		}

		ok = true

		markSteps(st.Current, ids, protocol.StepRunning, "")
	})

	if !ok {
		o.stop()
	}

	return ok
}

// fail records the first failure of the operation.
func (o *operation) fail(reason string) {
	o.failed = true

	o.log.WithField("error", reason).Error("operation step failed")

	o.apply(func(st *journal.State) {
		if st.Current.Operation.Error == "" {
			st.Current.Operation.Error = reason
		}
	})
}

// markDisrupted is called right before the first call that stops something,
// and reports whether that call may be made: only with the mark on disk.
// Unlike the rest of an operation's progress (apply), this one write is not
// kept in memory when it fails. If the process died after the call, the next
// one would find in the state file an operation that stopped nothing: it
// fails it (killed in draining or applying, see resume) and then skips the
// bring-back, which runs only for an operation that disrupted something, so
// the containers that were stopped would stay down; and its report would say
// that nothing was disrupted, which the controller reads as "the lease cost
// nothing". So nothing disruptive happens without the mark on disk: when it
// cannot be written, the steps the call would have begun fail, before
// anything is touched. ids are those steps.
func (o *operation) markDisrupted(ctx context.Context, ids []string) bool {
	if o.disrupted() {
		return true
	}

	var err error

	for attempt := range persistAttempts {
		if attempt > 0 {
			sleep(ctx, o.poll)
		}

		err = o.store.Update(func(st *journal.State) {
			if st.Current != nil {
				st.Current.Operation.Disrupted = true
				st.Current.Operation.UpdatedAt = o.now()
			}
		})
		if err == nil {
			return true
		}
	}

	// The error names a temporary file, a different one each time: it goes to
	// the log, not into the report.
	o.log.WithError(err).Error("cannot persist the state before the first disruptive call")

	const msg = "cannot write the state file before the first disruptive call; nothing was touched"

	o.fail(msg)
	o.setSteps(ids, protocol.StepFailed, msg)

	return false
}

func (o *operation) clearDisrupted() {
	o.apply(func(st *journal.State) { st.Current.Operation.Disrupted = false })
}

func (o *operation) disrupted() bool {
	rec := o.store.Snapshot().Current

	return rec != nil && rec.Operation.Disrupted
}

// touch adds containers to the set the verifying phase watches.
func (o *operation) touch(names []string) {
	o.apply(func(st *journal.State) {
		for _, name := range names {
			if !slices.Contains(st.Current.Touched, name) {
				st.Current.Touched = append(st.Current.Touched, name)
			}
		}
	})
}

func (o *operation) observe(ctx context.Context) ([]protocol.Container, error) {
	cctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	list, err := o.runtime.Observe(cctx)
	if err != nil {
		return nil, err
	}

	if o.seen == nil {
		o.seen = make(map[string]protocol.Container, len(list))
	}

	for i := range list {
		o.seen[list[i].Name] = list[i]
	}

	return list, nil
}

func (o *operation) status(ctx context.Context) (*hostapi.SystemStatus, error) {
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	sys, err := o.system.Status(cctx)
	if err != nil {
		return nil, err
	}

	if sys == nil {
		return nil, errors.New("no system status")
	}

	return sys, nil
}

// kindOf is the kind of a container by its label, as last observed.
func (o *operation) kindOf(name string) protocol.ContainerKind {
	if c, ok := o.seen[name]; ok && c.Group != "" {
		return protocol.ParseKind(string(c.Kind))
	}

	return protocol.KindOther
}

// stopTimeout is the stop grace period of a container: its group's, else
// the one it was created with, else defaultStopTimeout.
func (o *operation) stopTimeout(name string) time.Duration {
	c, ok := o.seen[name]

	switch {
	case !ok:
		return defaultStopTimeout
	case c.Group != "" && o.cfg.Group(c.Group).StopTimeout > 0:
		return o.cfg.Group(c.Group).StopTimeout
	case c.StopTimeout > 0:
		return c.StopTimeout
	default:
		return defaultStopTimeout
	}
}

// ordered sorts container names by kind in the given order, then by name.
func (o *operation) ordered(names []string, order []protocol.ContainerKind) []string {
	out := slices.Clone(names)

	slices.SortStableFunc(out, func(a, b string) int {
		return cmp.Or(
			cmp.Compare(kindRank(order, o.kindOf(a)), kindRank(order, o.kindOf(b))),
			cmp.Compare(a, b),
		)
	})

	return out
}

func kindRank(order []protocol.ContainerKind, kind protocol.ContainerKind) int {
	if i := slices.Index(order, kind); i >= 0 {
		return i
	}

	return len(order)
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
