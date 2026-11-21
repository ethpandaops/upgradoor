package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of the single calls an operation makes.
const (
	observeTimeout = 30 * time.Second
	statusTimeout  = 30 * time.Second
	// inventoryTimeout bounds the collection of the package inventory.
	inventoryTimeout = 2 * time.Minute
	startTimeout     = 2 * time.Minute
	stopGrace        = time.Minute
	recreateGrace    = 10 * time.Minute
	upgradeTimeout   = 30 * time.Minute
	rebootTimeout    = 2 * time.Minute

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

// operation executes the lease in the state file. One runs at a time, in
// its own goroutine; the sync loop reports its persisted phase meanwhile.
type operation struct {
	log      logrus.FieldLogger
	cfg      *Config
	runtime  ContainerRuntime
	registry Registry
	system   System
	holds    *holds
	store    *stateStore
	now      func() time.Time
	// poll is the pause between looks while waiting for something.
	poll time.Duration
	// notify tells the sync loop that the phase changed.
	notify func()
	// audit is the host's audit log; nil writes none.
	audit *auditLog

	// failed is true once a step failed; the remaining steps are skipped,
	// the containers are still brought back.
	failed bool
	// cancelled is true once a step was not begun because the lease was
	// cancelled; the remaining steps are skipped like after a failure, and
	// the containers are brought back the same way.
	cancelled bool
}

// operationPlan is the outcome of validating a lease: what will be done and
// the verdict on everything that will not.
type operationPlan struct {
	// verdicts are the steps that are not executed, by need ID.
	verdicts map[string]protocol.Step
	// reboot are the IDs of the reboot needs to serve.
	reboot []string
	// packageIDs are the IDs of the packages needs to serve, packages the
	// package names to upgrade.
	packageIDs []string
	packages   []string
	images     []imageTask

	wasRunning []string
	bootID     string
}

// imageTask is one container to recreate.
type imageTask struct {
	// ids are the needs served by this recreate.
	ids       []string
	container string
	digest    string
	// override is the ID of the override need the recreate serves; empty
	// for a recreate on the container's own image and command. What the
	// container is recreated as is read from the overrides file when the
	// recreate begins, never from the lease.
	override string
}

func (p *operationPlan) empty() bool {
	return len(p.reboot) == 0 && len(p.packageIDs) == 0 && len(p.images) == 0
}

func (p *operationPlan) verdict(id string, status protocol.StepStatus, reason string) {
	p.verdicts[id] = protocol.Step{NeedID: id, Status: status, Error: reason}
}

// run works on the current lease until it is terminal or ctx ends. The end
// of ctx does not cut a disruptive step short: run returns only once the
// draining and applying phases are through (see execute).
func (o *operation) run(ctx context.Context) {
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

// reasonWithdrawn is why a lease is not started that the controller stopped
// offering before the agent had begun.
const reasonWithdrawn = "the controller no longer offered the lease before the operation started"

// reasonCancelled is why a lease is not started that a person ended before
// the agent had begun, and the note on the steps of a started operation that
// were not begun for the same reason.
const (
	reasonCancelled     = "the lease was cancelled before the operation started"
	reasonStepCancelled = "cancelled: the lease was ended before this step had begun"
)

// revoked returns why a lease that has not started must not be started any
// more, or "".
func (r *operationRecord) revoked() string {
	switch {
	case r.Cancelled:
		return reasonCancelled
	case r.Withdrawn:
		return reasonWithdrawn
	default:
		return ""
	}
}

// begin decides whether to act on a freshly persisted lease and, if so,
// executes it.
func (o *operation) begin(ctx context.Context, st *persistedState) error {
	rec := st.Current

	if reason := rec.revoked(); reason != "" {
		return o.decline(reason)
	}

	if !o.now().Before(rec.Deadline) {
		return o.decline(fmt.Sprintf(
			"lease deadline %s passed before the operation started",
			rec.Deadline.UTC().Format(time.RFC3339)))
	}

	if hold := o.holds.Read(); hold.Active() {
		return o.decline(holdReason(hold))
	}

	plan, err := o.planUntil(ctx, st, rec.Deadline)

	switch {
	case errors.Is(err, errInterrupted):
		return err
	case err != nil:
		// The host could not be looked at for as long as the lease was
		// good. Nothing was touched, so this is a lease that never started,
		// not a failed operation: a failure would halt the rollout.
		return o.decline(fmt.Sprintf(
			"lease deadline passed before the host could be examined: %v", err))
	}

	// Planning asked the runtime and the system; time has passed.
	if !o.now().Before(rec.Deadline) {
		return o.decline(
			"lease deadline passed while the operation was prepared")
	}

	if hold := o.holds.Read(); hold.Active() {
		return o.decline(holdReason(hold))
	}

	// Last exit: from the next write on the operation is started and runs
	// its disruptive phases to the end.
	if ctx.Err() != nil {
		return errInterrupted
	}

	first := protocol.PhaseApplying

	switch {
	case plan.empty():
		first = protocol.PhaseDone
	case len(plan.reboot) > 0:
		first = protocol.PhaseDraining
	}

	// The start of a lease excludes its withdrawal and its cancellation:
	// whichever is written first stands.
	revoked := ""

	err = o.store.Update(func(st *persistedState) {
		cur := st.Current
		if cur == nil {
			return
		}

		if revoked = cur.revoked(); revoked != "" {
			return
		}

		cur.Started = true
		cur.BootID = plan.bootID
		cur.WasRunning = plan.wasRunning
		cur.Operation.Phase = first
		cur.Operation.StartedAt = o.now()
		cur.Operation.UpdatedAt = cur.Operation.StartedAt

		for i := range cur.Operation.Steps {
			if v, ok := plan.verdicts[cur.Operation.Steps[i].NeedID]; ok {
				cur.Operation.Steps[i] = v
			}
		}

		if first.Terminal() {
			closeManualNeeds(st)
		}
	})
	if err != nil {
		// Not persisted means not started: the loop tries again.
		o.log.WithError(err).Error("cannot persist the start of the operation; not acting")

		return nil
	}

	if revoked != "" {
		return o.decline(revoked)
	}

	o.notify()

	if first.Terminal() {
		o.log.Info("nothing to do for this lease: every need is superseded or refused")

		return nil
	}

	o.log.WithFields(logrus.Fields{
		"reboot":   len(plan.reboot) > 0,
		"packages": plan.packages,
		"images":   len(plan.images),
	}).Info("operation started")

	return o.execute(ctx, plan)
}

// planUntil plans the operation. The runtime or the system not answering is
// taken as transient (a Docker daemon that restarts, a busy dpkg): planning
// is tried again until the lease deadline, and only then the last error is
// returned. It returns errInterrupted when ctx ends first.
func (o *operation) planUntil(
	ctx context.Context, st *persistedState, deadline time.Time,
) (*operationPlan, error) {
	warned := false

	for {
		plan, err := o.plan(ctx, st)
		if err == nil {
			return plan, nil
		}

		if ctx.Err() != nil {
			return nil, errInterrupted
		}

		if !o.now().Before(deadline) {
			return nil, err
		}

		if !warned {
			warned = true

			o.log.WithError(err).WithField("deadline", deadline.UTC().Format(time.RFC3339)).
				Warn("cannot examine the host for the lease; trying again until its deadline")
		}

		if !sleep(ctx, o.poll) {
			return nil, errInterrupted
		}
	}
}

// decline ends an operation that never started: it is done, every step is
// refused for the same reason and nothing is disrupted.
func (o *operation) decline(reason string) error {
	err := o.store.Update(func(st *persistedState) {
		cur := st.Current
		if cur == nil {
			return
		}

		now := o.now()
		cur.Operation.Phase = protocol.PhaseDone
		cur.Operation.StartedAt = now
		cur.Operation.UpdatedAt = now
		cur.Operation.Disrupted = false

		for i := range cur.Operation.Steps {
			cur.Operation.Steps[i].Status = protocol.StepRefused
			cur.Operation.Steps[i].Error = reason
		}
	})
	if err != nil {
		o.log.WithError(err).Error("cannot persist the refusal of the lease")

		return nil
	}

	o.log.WithField("reason", reason).Warn("lease not acted on")
	o.notify()

	return nil
}

func holdReason(h protocol.Hold) string {
	switch {
	case h.Local && h.LocalReason != "":
		return "host is on local hold: " + h.LocalReason
	case h.Local:
		return "host is on local hold"
	default:
		return "host is on controller hold"
	}
}

// plan validates every granted need against the local configuration and
// the current state of the host. The lease grants a slot; it cannot widen
// what the agent is willing to do.
func (o *operation) plan(ctx context.Context, st *persistedState) (*operationPlan, error) {
	needs := st.Current.Lease.Needs
	plan := &operationPlan{verdicts: make(map[string]protocol.Step, len(needs))}

	containers, err := o.observe(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list containers: %w", err)
	}

	_, targets := enrichContainers(o.cfg, containers, o.overrides(), noRegistryCheck)

	for i := range containers {
		if containers[i].Managed && containers[i].ShouldRun() {
			plan.wasRunning = append(plan.wasRunning, containers[i].Name)
		}
	}

	slices.Sort(plan.wasRunning)

	sys, err := o.status(ctx)

	switch {
	case err == nil:
		plan.bootID = sys.BootID
	case needsSystem(needs):
		return nil, fmt.Errorf("cannot read the system state: %w", err)
	}

	byContainer := make(map[string]int, len(needs))
	packages := make(map[string]struct{}, 8)

	// The override and spec needs come first: a recreate that brings a
	// container in line with agent.yaml and the overrides file also serves a
	// roll of that container.
	for i := range needs {
		if n := &needs[i]; recreates(n) && o.cfg.Allows(protocol.VerbRoll) {
			o.planOverride(plan, n, containers, targets, byContainer)
		}
	}

	for i := range needs {
		n := &needs[i]

		switch {
		case recreates(n) && o.cfg.Allows(protocol.VerbRoll):
		case n.Kind.Verb() == "":
			plan.verdict(n.ID, protocol.StepRefused, fmt.Sprintf("unknown need kind %q", n.Kind))
		case !o.cfg.Allows(n.Kind.Verb()):
			plan.verdict(n.ID, protocol.StepRefused,
				fmt.Sprintf("verb %s is not allowed on this host", n.Kind.Verb()))
		case (n.DirectiveID != "" || n.Local) && !manualNeedOpen(st.ManualNeeds, n):
			plan.verdict(n.ID, protocol.StepSuperseded, "the manual need is no longer open")
		case n.Kind == protocol.NeedReboot:
			o.planReboot(plan, n, sys)
		case n.Kind == protocol.NeedPackages:
			o.planPackages(plan, n, sys, packages)
		default:
			o.planImage(plan, n, containers, byContainer)
		}
	}

	plan.packages = make([]string, 0, len(packages))
	for name := range packages {
		plan.packages = append(plan.packages, name)
	}

	slices.Sort(plan.packages)

	rank := func(t imageTask) int { return kindRank(startOrder, o.kindOf(t.container)) }

	slices.SortStableFunc(plan.images, func(a, b imageTask) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.container, b.container))
	})

	return plan, nil
}

func noRegistryCheck(string) (protocol.RegistryCheck, bool) {
	return protocol.RegistryCheck{}, false
}

func needsSystem(needs []protocol.Need) bool {
	for i := range needs {
		if needs[i].Kind == protocol.NeedReboot || needs[i].Kind == protocol.NeedPackages {
			return true
		}
	}

	return false
}

// manualNeedOpen reports whether a granted manual need is one this agent
// created for a directive and has not served yet.
func manualNeedOpen(manual []protocol.Need, n *protocol.Need) bool {
	for i := range manual {
		if manual[i].ID == n.ID && manual[i].DirectiveID == n.DirectiveID &&
			manual[i].Kind == n.Kind {
			return true
		}
	}

	return false
}

func (o *operation) planReboot(plan *operationPlan, n *protocol.Need, sys *SystemStatus) {
	switch {
	case n.Reboot == nil:
		plan.verdict(n.ID, protocol.StepRefused, "malformed reboot need")
	case n.DirectiveID == "" && len(rebootReasons(sys)) == 0:
		plan.verdict(n.ID, protocol.StepSuperseded, "the host no longer needs a reboot")
	default:
		plan.reboot = append(plan.reboot, n.ID)
	}
}

func (o *operation) planPackages(
	plan *operationPlan, n *protocol.Need, sys *SystemStatus, names map[string]struct{},
) {
	if n.Packages == nil || len(n.Packages.Packages) == 0 {
		plan.verdict(n.ID, protocol.StepRefused, "malformed packages need")

		return
	}

	for _, p := range n.Packages.Packages {
		if !slices.Contains(o.cfg.Packages.Blocked, p.Name) {
			plan.verdict(n.ID, protocol.StepRefused,
				fmt.Sprintf("package %q is not in the configured blocked set", p.Name))

			return
		}
	}

	pending := blockedChanges(o.cfg, sys)
	wanted := make([]string, 0, len(n.Packages.Packages))

	for _, p := range n.Packages.Packages {
		if slices.ContainsFunc(pending, func(c protocol.PackageChange) bool { return c.Name == p.Name }) {
			wanted = append(wanted, p.Name)
		}
	}

	if len(wanted) == 0 {
		plan.verdict(n.ID, protocol.StepSuperseded, "the packages are already upgraded")

		return
	}

	for _, name := range wanted {
		names[name] = struct{}{}
	}

	plan.packageIDs = append(plan.packageIDs, n.ID)
}

// overrides reads the overrides file as it is now. An entry whose end has
// passed counts as gone, whether or not it has been taken out of the file.
func (o *operation) overrides() *overrideSet {
	return currentOverrides(o.cfg.OverridesFile, o.now())
}

// planOverride validates an override need: the container is still not what
// the overrides file says, and the file still says what the need was
// reported for.
func (o *operation) planOverride(
	plan *operationPlan, n *protocol.Need, containers []protocol.Container,
	targets map[string]overrideTarget, byContainer map[string]int,
) {
	name, digest := recreateTarget(n)
	if name == "" || digest == "" {
		plan.verdict(n.ID, protocol.StepRefused, "malformed "+string(n.Kind)+" need")

		return
	}

	if o.cfg.Managed(name) == nil {
		plan.verdict(n.ID, protocol.StepRefused, "container "+name+" is not managed")

		return
	}

	target, ok := targets[name]
	if reason := overrideStale(findContainer(containers, name), &target, ok, n.ID, digest); reason != "" {
		plan.verdict(n.ID, protocol.StepSuperseded, reason)

		return
	}

	if _, dup := byContainer[name]; dup {
		plan.verdict(n.ID, protocol.StepSuperseded, "another need of this lease recreates container "+name)

		return
	}

	byContainer[name] = len(plan.images)
	plan.images = append(plan.images, imageTask{
		ids: []string{n.ID}, container: name, digest: digest, override: n.ID,
	})
}

// recreateTarget is the container and target digest of an override or a
// spec need; empty for a malformed one.
func recreateTarget(n *protocol.Need) (container, digest string) {
	switch {
	case n.Kind == protocol.NeedOverride && n.Override != nil:
		return n.Override.Container, n.Override.ToDigest
	case n.Kind == protocol.NeedSpec && n.Spec != nil:
		return n.Spec.Container, n.Spec.ToDigest
	default:
		return "", ""
	}
}

// recreates reports whether a need recreates a container the way agent.yaml
// and the overrides file say: an override or a spec need.
func recreates(n *protocol.Need) bool {
	return n.Kind == protocol.NeedOverride || n.Kind == protocol.NeedSpec
}

// overrideStale says why an override or spec need no longer applies; empty
// when it does.
func overrideStale(
	c *protocol.Container, target *overrideTarget, known bool, needID, digest string,
) string {
	switch {
	case c == nil || !known:
		return "the container does not exist"
	case target.err != "":
		return target.err
	case target.inSync:
		return "container " + c.Name + " already runs what agent.yaml and the overrides file say"
	case target.needID(c, digest) != needID:
		return "agent.yaml or the overrides file asks for something else now"
	default:
		return ""
	}
}

func (o *operation) planImage(
	plan *operationPlan, n *protocol.Need, containers []protocol.Container, byContainer map[string]int,
) {
	if n.Image == nil || n.Image.Container == "" || n.Image.ToDigest == "" {
		plan.verdict(n.ID, protocol.StepRefused, "malformed image need")

		return
	}

	name := n.Image.Container
	c := findContainer(containers, name)

	if reason, refused := rollable(o.cfg, c, name); reason != "" {
		status := protocol.StepSuperseded
		if refused {
			status = protocol.StepRefused
		}

		plan.verdict(n.ID, status, reason)

		return
	}

	switch {
	case n.Image.Image != "" && !SameReference(n.Image.Image, c.Image):
		plan.verdict(n.ID, protocol.StepSuperseded, fmt.Sprintf(
			"container %s is on %s, the need was for %s", name, c.Image, n.Image.Image))

		return
	case n.DirectiveID == "" && !n.Local && c.Digest == n.Image.ToDigest:
		plan.verdict(n.ID, protocol.StepSuperseded,
			"container "+name+" already runs the wanted digest")

		return
	}

	// A lease holds at most one image need per container, because the agent
	// reports at most one. This is a guard for a lease that breaks that rule:
	// the container is still recreated once, not once per need.
	if i, ok := byContainer[name]; ok {
		if plan.images[i].digest == n.Image.ToDigest {
			plan.images[i].ids = append(plan.images[i].ids, n.ID)
		} else {
			plan.verdict(n.ID, protocol.StepSuperseded,
				"another need of this lease recreates container "+name)
		}

		return
	}

	byContainer[name] = len(plan.images)
	plan.images = append(plan.images, imageTask{
		ids: []string{n.ID}, container: name, digest: n.Image.ToDigest,
	})
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
func (o *operation) resume(ctx context.Context, rec *operationRecord) error {
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

func stepIDs(rec *operationRecord, status protocol.StepStatus) []string {
	ids := make([]string, 0, len(rec.Operation.Steps))

	for _, s := range rec.Operation.Steps {
		if s.Status == status {
			ids = append(ids, s.NeedID)
		}
	}

	return ids
}

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

	_, targets := enrichContainers(o.cfg, containers, o.overrides(), noRegistryCheck)
	c := findContainer(containers, name)

	var change Recreation

	if task.override != "" {
		target, ok := targets[name]
		if reason := overrideStale(c, &target, ok, task.override, task.digest); reason != "" {
			o.setSteps(task.ids, protocol.StepSuperseded, reason)

			return
		}

		change = Recreation{
			Image: target.image, Command: target.command, SetCommand: target.flags,
			Labels: target.labels,
		}

		log = log.WithFields(logrus.Fields{"image": target.image, "flags": target.flags})
	} else if reason, _ := rollable(o.cfg, c, name); reason != "" {
		o.setSteps(task.ids, protocol.StepSuperseded, reason)

		return
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
	case errors.Is(err, ErrDigestMismatch):
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
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
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

// reboot persists the phase, then issues the reboot. It reports whether the
// reboot was issued; the caller then waits in awaitReboot, where the agent
// normally dies, and picks the operation up from the state file after the
// boot. ctx is the uninterruptible context of execute: stopping the agent
// between the drain and the reboot would leave the host drained for nothing.
func (o *operation) reboot(ctx context.Context, plan *operationPlan) bool {
	var (
		err       error
		cancelled bool
	)

	for range persistAttempts {
		err = o.store.Update(func(st *persistedState) {
			cur := st.Current
			if cur == nil {
				return
			}

			// The reboot has not begun: a cancel wins.
			if cancelled = cur.Cancelled; cancelled {
				return
			}

			now := o.now()
			cur.Operation.Phase = protocol.PhaseRebooting
			cur.Operation.UpdatedAt = now
			cur.Operation.Disrupted = true
			cur.RebootIssuedAt = now
			cur.Touched = slices.Clone(cur.WasRunning)

			markSteps(cur, plan.reboot, protocol.StepRunning, "")
		})
		if err == nil {
			break
		}

		sleep(ctx, o.poll)
	}

	if cancelled {
		o.stop()

		return false
	}

	if err != nil {
		// Rebooting without the phase on disk would lose the operation.
		msg := fmt.Sprintf("cannot persist the state before the reboot: %v", err)

		o.fail(msg)
		o.setSteps(plan.reboot, protocol.StepFailed, msg)

		return false
	}

	o.notify()
	o.log.Warn("rebooting the host")

	cctx, cancel := context.WithTimeout(ctx, rebootTimeout)
	err = o.system.Reboot(cctx)

	cancel()

	if err != nil {
		msg := fmt.Sprintf("reboot: %v", err)

		o.fail(msg)
		o.setSteps(plan.reboot, protocol.StepFailed, msg)

		return false
	}

	return true
}

// awaitReboot waits until the boot id differs from the one the operation
// started under. If it still is the same long after the reboot was issued,
// the host did not reboot and the reboot steps fail.
func (o *operation) awaitReboot(ctx context.Context, ids []string) error {
	for {
		rec := o.store.Snapshot().Current
		if rec == nil {
			return errInterrupted
		}

		sys, err := o.status(ctx)

		switch {
		case err != nil && ctx.Err() != nil:
			return errInterrupted
		case err != nil:
			o.log.WithError(err).Warn("cannot read the boot id while waiting for the reboot")
		case sys.BootID != "" && sys.BootID != rec.BootID:
			o.log.WithField("bootId", sys.BootID).Info("the host is back from the reboot")
			o.setSteps(ids, protocol.StepDone, "")

			return nil
		}

		if waited := o.now().Sub(rec.RebootIssuedAt); waited > defaultRebootGrace {
			msg := fmt.Sprintf("the reboot was issued at %s but the host did not reboot "+
				"within %s (boot id unchanged)",
				rec.RebootIssuedAt.UTC().Format(time.RFC3339), defaultRebootGrace)

			o.fail(msg)
			o.setSteps(ids, protocol.StepFailed, msg)

			return nil
		}

		if !sleep(ctx, o.poll) {
			return errInterrupted
		}
	}
}

// bringBack runs the starting and verifying phases and closes the
// operation. It also runs after a failure, so that the host comes back as
// far as it can.
func (o *operation) bringBack(ctx context.Context) error {
	rec := o.store.Snapshot().Current
	if rec == nil {
		return errInterrupted
	}

	if rec.Operation.Disrupted {
		// verify.timeout bounds starting and verifying together.
		deadline := o.now().Add(o.cfg.Verify.Timeout)

		if err := o.startContainers(ctx, rec.WasRunning, deadline); err != nil {
			return err
		}

		if !o.failed && len(rec.Touched) > 0 {
			if err := o.verify(ctx, rec.Touched, deadline); err != nil {
				return err
			}
		}
	}

	o.finish()

	return nil
}

// startContainers makes sure every managed container that ran before the
// operation runs again, execution client first. Right after a boot the
// Docker daemon may not answer yet, so it keeps trying up to deadline (the
// verify.timeout shared with the verifying phase).
//
// A pass over the containers is not cut short when ctx ends, so that an
// agent that is being stopped still brings the containers back if the
// runtime lets it; only the wait for another pass is given up.
func (o *operation) startContainers(
	ctx context.Context, names []string, deadline time.Time,
) error {
	o.setPhase(protocol.PhaseStarting)

	var (
		work  = context.WithoutCancel(ctx)
		order = o.ordered(names, startOrder)
	)

	for {
		var failure string

		for _, name := range order {
			cctx, cancel := context.WithTimeout(work, startTimeout)
			err := o.runtime.Start(cctx, name)

			cancel()

			if err != nil && failure == "" {
				failure = fmt.Sprintf("start container %s: %v", name, err)
			}
		}

		if failure == "" {
			return nil
		}

		if ctx.Err() != nil {
			return errInterrupted
		}

		if !o.now().Before(deadline) {
			o.fail(failure)

			return nil
		}

		o.log.WithField("error", failure).Debug("containers not started yet; trying again")

		if !sleep(ctx, o.poll) {
			return errInterrupted
		}
	}
}

// verify waits until every touched container has been running, without a
// restart, for verify.window in one piece. It gives up at deadline, the
// verify.timeout that began with the starting phase.
func (o *operation) verify(ctx context.Context, names []string, deadline time.Time) error {
	o.setPhase(protocol.PhaseVerifying)

	var (
		baseline map[string]int
		stable   time.Time
		problem  = "containers were never observed"
	)

	for {
		counts, reason, err := o.restartCounts(ctx, names)

		switch {
		case err != nil && ctx.Err() != nil:
			return errInterrupted
		case err != nil:
			baseline, problem = nil, "cannot list containers: "+err.Error()
		case reason != "":
			baseline, problem = nil, reason
		case baseline == nil:
			baseline, stable = counts, o.now()
		default:
			if name := restarted(baseline, counts); name != "" {
				problem = "container " + name + " restarted"
				baseline, stable = counts, o.now()
			}
		}

		if baseline != nil && o.now().Sub(stable) >= o.cfg.Verify.Window {
			return nil
		}

		if !o.now().Before(deadline) {
			o.fail(fmt.Sprintf("containers did not stay up for %s within %s: %s",
				o.cfg.Verify.Window, o.cfg.Verify.Timeout, problem))

			return nil
		}

		if !sleep(ctx, o.poll) {
			return errInterrupted
		}
	}
}

// restartCounts returns the restart count of every named container, or the
// reason why one of them does not count as up.
func (o *operation) restartCounts(
	ctx context.Context, names []string,
) (counts map[string]int, reason string, err error) {
	containers, err := o.observe(ctx)
	if err != nil {
		return nil, "", err
	}

	counts = make(map[string]int, len(names))

	for _, name := range names {
		c := findContainer(containers, name)

		switch {
		case c == nil:
			return nil, "container " + name + " does not exist", nil
		case c.State != protocol.StateRunning:
			return nil, "container " + name + " is " + c.State, nil
		}

		counts[name] = c.RestartCount
	}

	return counts, "", nil
}

func restarted(before, after map[string]int) string {
	names := make([]string, 0, len(after))
	for name := range after {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if before[name] != after[name] {
			return name
		}
	}

	return ""
}

// finish makes the operation terminal and closes the manual needs it
// served.
func (o *operation) finish() {
	phase := protocol.PhaseDone
	if o.failed {
		phase = protocol.PhaseFailed
	}

	o.apply(func(st *persistedState) {
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
func (o *operation) auditResults(rec *operationRecord) {
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

		out := AuditRecord{
			Source: auditAgent, Lease: rec.Lease.ID, Result: string(step.Status), Error: step.Error,
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

		o.audit.record(out)
	}
}

// closeManualNeeds drops the manual needs a terminal operation served,
// successfully or not: a directive is good for one attempt.
func closeManualNeeds(st *persistedState) {
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
func (o *operation) apply(fn func(st *persistedState)) {
	err := o.store.Apply(func(st *persistedState) {
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

	o.apply(func(st *persistedState) {
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

	o.apply(func(st *persistedState) { markSteps(st.Current, ids, status, reason) })
}

func markSteps(rec *operationRecord, ids []string, status protocol.StepStatus, reason string) {
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

	o.apply(func(st *persistedState) {
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

	o.apply(func(st *persistedState) {
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

		err = o.store.Update(func(st *persistedState) {
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
	o.apply(func(st *persistedState) { st.Current.Operation.Disrupted = false })
}

func (o *operation) disrupted() bool {
	rec := o.store.Snapshot().Current

	return rec != nil && rec.Operation.Disrupted
}

// touch adds containers to the set the verifying phase watches.
func (o *operation) touch(names []string) {
	o.apply(func(st *persistedState) {
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

	return o.runtime.Observe(cctx)
}

func (o *operation) status(ctx context.Context) (*SystemStatus, error) {
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

func (o *operation) kindOf(name string) protocol.ContainerKind {
	if m := o.cfg.Managed(name); m != nil {
		return m.Kind
	}

	return protocol.KindOther
}

func (o *operation) stopTimeout(name string) time.Duration {
	if m := o.cfg.Managed(name); m != nil && m.StopTimeout > 0 {
		return m.StopTimeout
	}

	return defaultStopTimeout
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
