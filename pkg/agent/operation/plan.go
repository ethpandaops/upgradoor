package operation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/agent/needs"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

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

// whyRevoked returns why a lease that has not started must not be started any
// more, or "".
func whyRevoked(r *journal.Record) string {
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
func (o *operation) begin(ctx context.Context, st *journal.State) error {
	rec := st.Current

	if reason := whyRevoked(rec); reason != "" {
		return o.decline(reason)
	}

	if !o.now().Before(rec.Deadline) {
		return o.decline(fmt.Sprintf(
			"lease deadline %s passed before the operation started",
			rec.Deadline.UTC().Format(time.RFC3339)))
	}

	if hold := o.holds.Hold(); hold.Active() {
		return o.decline(overrides.HoldReason(hold))
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

	if hold := o.holds.Hold(); hold.Active() {
		return o.decline(overrides.HoldReason(hold))
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

	err = o.store.Update(func(st *journal.State) {
		cur := st.Current
		if cur == nil {
			return
		}

		if revoked = whyRevoked(cur); revoked != "" {
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
	ctx context.Context, st *journal.State, deadline time.Time,
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
	err := o.store.Update(func(st *journal.State) {
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

// plan validates every granted need against the local configuration and
// the current state of the host. The lease grants a slot; it cannot widen
// what the agent is willing to do.
func (o *operation) plan(ctx context.Context, st *journal.State) (*operationPlan, error) {
	granted := st.Current.Lease.Needs
	plan := &operationPlan{verdicts: make(map[string]protocol.Step, len(granted))}

	containers, err := o.observe(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list containers: %w", err)
	}

	_, targets := needs.Enrich(o.cfg, containers, o.overrides(), noRegistryCheck)

	// The default group is independent of the node: its containers are
	// stopped, started and watched only when the lease rolls them.
	defaultRunning := make([]string, 0, 4)

	for i := range containers {
		switch c := &containers[i]; {
		case !c.Managed || !c.ShouldRun():
		case c.Group == protocol.DefaultGroup:
			defaultRunning = append(defaultRunning, c.Name)
		default:
			plan.wasRunning = append(plan.wasRunning, c.Name)
		}
	}

	sys, err := o.status(ctx)

	switch {
	case err == nil:
		plan.bootID = sys.BootID
	case needsSystem(granted):
		return nil, fmt.Errorf("cannot read the system state: %w", err)
	}

	byContainer := make(map[string]int, len(granted))
	packages := make(map[string]struct{}, 8)

	// The override and spec needs come first: a recreate that brings a
	// container in line with agent.yaml and the overrides file also serves a
	// roll of that container.
	for i := range granted {
		if n := &granted[i]; recreates(n) && o.cfg.Allows(protocol.VerbRoll) {
			o.planOverride(plan, n, containers, targets, byContainer)
		}
	}

	for i := range granted {
		n := &granted[i]

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

	for i := range plan.images {
		if name := plan.images[i].container; slices.Contains(defaultRunning, name) {
			plan.wasRunning = append(plan.wasRunning, name)
		}
	}

	slices.Sort(plan.wasRunning)

	return plan, nil
}

func noRegistryCheck(string) (protocol.RegistryCheck, bool) {
	return protocol.RegistryCheck{}, false
}

func needsSystem(list []protocol.Need) bool {
	for i := range list {
		if list[i].Kind == protocol.NeedReboot || list[i].Kind == protocol.NeedPackages {
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

func (o *operation) planReboot(plan *operationPlan, n *protocol.Need, sys *hostapi.SystemStatus) {
	switch {
	case n.Reboot == nil:
		plan.verdict(n.ID, protocol.StepRefused, "malformed reboot need")
	case n.DirectiveID == "" && len(needs.RebootReasons(sys)) == 0:
		plan.verdict(n.ID, protocol.StepSuperseded, "the host no longer needs a reboot")
	default:
		plan.reboot = append(plan.reboot, n.ID)
	}
}

func (o *operation) planPackages(
	plan *operationPlan, n *protocol.Need, sys *hostapi.SystemStatus, names map[string]struct{},
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

	pending := needs.BlockedChanges(o.cfg, sys)
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
func (o *operation) overrides() *overrides.Set {
	return overrides.Current(o.cfg.OverridesFile, o.now())
}

// planOverride validates an override need: the container is still not what
// the overrides file says, and the file still says what the need was
// reported for.
func (o *operation) planOverride(
	plan *operationPlan, n *protocol.Need, containers []protocol.Container,
	targets map[string]overrides.Target, byContainer map[string]int,
) {
	name, digest := recreateTarget(n)
	if name == "" || digest == "" {
		plan.verdict(n.ID, protocol.StepRefused, "malformed "+string(n.Kind)+" need")

		return
	}

	if c := needs.FindContainer(containers, name); c != nil && !c.Managed {
		plan.verdict(n.ID, protocol.StepRefused, "container "+name+" is not managed")

		return
	}

	target, ok := targets[name]
	if reason := overrideStale(needs.FindContainer(containers, name), &target, ok, n.ID, digest); reason != "" {
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
	c *protocol.Container, target *overrides.Target, known bool, needID, digest string,
) string {
	switch {
	case c == nil || !known:
		return "the container does not exist"
	case target.Err != "":
		return target.Err
	case target.InSync:
		return "container " + c.Name + " already runs what agent.yaml and the overrides file say"
	case target.NeedID(c, digest) != needID:
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
	c := needs.FindContainer(containers, name)

	if reason, refused := needs.Rollable(c, name); reason != "" {
		status := protocol.StepSuperseded
		if refused {
			status = protocol.StepRefused
		}

		plan.verdict(n.ID, status, reason)

		return
	}

	switch {
	case n.Image.Image != "" && !imageref.Same(n.Image.Image, c.Image):
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
