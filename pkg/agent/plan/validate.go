package plan

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// DefaultStopTimeout is the stop grace period when the container names none.
const DefaultStopTimeout = 300 * time.Second

// LeasePlan is what a lease will do, and the verdict on each granted need it
// will not serve.
type LeasePlan struct {
	// Decline is set when the host is on hold: every step is refused with
	// it and nothing is disrupted.
	Decline string
	// Verdicts are the superseded or refused steps, by need ID.
	Verdicts map[string]protocol.Step
	// Reboot are the IDs of the reboot needs to serve.
	Reboot []string
	// PackageIDs are the packages needs to serve; Packages the sorted names
	// to upgrade.
	PackageIDs []string
	Packages   []string
	// Recreates are the containers to recreate, one each, in start order.
	Recreates []Recreate
	// WasRunning are the containers the starting phase brings back, sorted.
	WasRunning []string
}

// Empty reports whether the plan does nothing.
func (p *LeasePlan) Empty() bool {
	return len(p.Reboot) == 0 && len(p.PackageIDs) == 0 && len(p.Recreates) == 0
}

// Recreate is one container to recreate under a lease; what it is recreated
// as is decided by Recheck right before.
type Recreate struct {
	// IDs are the needs this recreate serves.
	IDs       []string
	Container string
	Digest    string
	// Target is the spec or override need served; empty for a plain recreate.
	Target string
}

// Recreation is what a recreate changes about a container (host.Change).
type Recreation struct {
	// Image is the replacement's image; empty keeps the container's own.
	Image string
	// Command replaces the command line when SetCommand.
	Command    []string
	SetCommand bool
	// Base is written to the override labels when SetBase (nil removes
	// them); without SetBase the labels stay as they are.
	Base    *protocol.OverrideBase
	SetBase bool
}

// Validate checks every granted need against the policy, a fresh view and
// the open manual needs: a lease grants a slot, it cannot widen what the
// agent does. It errs only when the view lacks fresh containers or the
// system state a need requires. Checks, in order:
//
//   - hold active: Decline;
//   - spec and override needs first, so they also serve a roll of the same
//     container;
//   - unknown kind or verb not allowed: refused;
//   - a manual need the agent does not hold open: superseded;
//   - per kind: malformed or outside policy is refused, no longer needed
//     is superseded; a second need of a container joins its recreate at
//     the same digest and is superseded at another.
func Validate(
	policy *Policy, lease *protocol.Lease, view *View, manual []protocol.Need,
) (LeasePlan, error) {
	granted := lease.Needs
	v := validation{
		policy: policy, view: view, manual: manual,
		plan:        LeasePlan{Verdicts: make(map[string]protocol.Step, len(granted))},
		byContainer: make(map[string]int, len(granted)),
		packages:    make(map[string]struct{}, len(policy.Packages.Blocked)),
	}

	if view.Hold.Active() {
		v.plan.Decline = HoldReason(view.Hold)

		return v.plan, nil
	}

	// A lease is never planned on the last good list.
	if !view.Fresh {
		return LeasePlan{}, errors.New("the containers could not be listed")
	}

	if view.System == nil && slices.ContainsFunc(granted, func(n protocol.Need) bool {
		return n.Kind == protocol.NeedReboot || n.Kind == protocol.NeedPackages
	}) {
		return LeasePlan{}, errors.New("the system state is not known")
	}

	// Spec and override needs first: their recreate also serves a roll.
	roll := policy.Allows(protocol.VerbRoll)

	for i := range granted {
		if n := &granted[i]; recreatesTarget(n) && roll {
			v.target(n)
		}
	}

	for i := range granted {
		v.other(&granted[i], roll)
	}

	v.plan.Packages = slices.Sorted(maps.Keys(v.packages))
	v.plan.WasRunning = wasRunning(&v.plan, view.Containers)

	// By the classified kind: a name-mapped container has its mapped kind.
	slices.SortStableFunc(v.plan.Recreates, func(a, b Recreate) int {
		return compareOrder(a.Container, b.Container, view.Containers, startOrder)
	})

	return v.plan, nil
}

// validation is the state of one Validate.
type validation struct {
	policy *Policy
	view   *View
	manual []protocol.Need
	plan   LeasePlan
	// byContainer is the index in plan.Recreates of a container's recreate.
	byContainer map[string]int
	// packages are the names of the packages to upgrade.
	packages map[string]struct{}
}

func (v *validation) verdict(id string, status protocol.StepStatus, reason string) {
	v.plan.Verdicts[id] = protocol.Step{NeedID: id, Status: status, Error: reason}
}

// other validates a need that target does not handle.
func (v *validation) other(n *protocol.Need, roll bool) {
	switch {
	case recreatesTarget(n) && roll:
	case n.Kind.Verb() == "":
		v.verdict(n.ID, protocol.StepRefused, fmt.Sprintf("unknown need kind %q", n.Kind))
	case !v.policy.Allows(n.Kind.Verb()):
		v.verdict(n.ID, protocol.StepRefused,
			fmt.Sprintf("verb %s is not allowed on this host", n.Kind.Verb()))
	case (n.DirectiveID != "" || n.Local) && !manualNeedOpen(v.manual, n):
		// A lease cannot make up a manual request.
		v.verdict(n.ID, protocol.StepSuperseded, "the manual need is no longer open")
	case n.Kind == protocol.NeedReboot:
		v.reboot(n)
	case n.Kind == protocol.NeedPackages:
		v.packagesNeed(n)
	default:
		v.image(n)
	}
}

// target validates a spec or override need against the current target.
func (v *validation) target(n *protocol.Need) {
	name, digest := needTarget(n)
	if name == "" || digest == "" {
		v.verdict(n.ID, protocol.StepRefused, "malformed "+string(n.Kind)+" need")

		return
	}

	c := v.view.Container(name)
	if reason, refused := rollable(c, name); refused {
		v.verdict(n.ID, protocol.StepRefused, reason)

		return
	}

	if reason := targetStale(v.view, c, name, n.ID, digest); reason != "" {
		v.verdict(n.ID, protocol.StepSuperseded, reason)

		return
	}

	if _, dup := v.byContainer[name]; dup {
		v.verdict(n.ID, protocol.StepSuperseded, "another need of this lease recreates container "+name)

		return
	}

	v.byContainer[name] = len(v.plan.Recreates)
	v.plan.Recreates = append(v.plan.Recreates, Recreate{
		IDs: []string{n.ID}, Container: name, Digest: digest, Target: n.ID,
	})
}

func (v *validation) reboot(n *protocol.Need) {
	switch {
	case n.Reboot == nil:
		v.verdict(n.ID, protocol.StepRefused, "malformed reboot need")
	case n.DirectiveID == "" && len(rebootReasons(v.view.System)) == 0:
		v.verdict(n.ID, protocol.StepSuperseded, "the host no longer needs a reboot")
	default:
		v.plan.Reboot = append(v.plan.Reboot, n.ID)
	}
}

func (v *validation) packagesNeed(n *protocol.Need) {
	if n.Packages == nil || len(n.Packages.Packages) == 0 {
		v.verdict(n.ID, protocol.StepRefused, "malformed packages need")

		return
	}

	for _, p := range n.Packages.Packages {
		if !slices.Contains(v.policy.Packages.Blocked, p.Name) {
			v.verdict(n.ID, protocol.StepRefused,
				fmt.Sprintf("package %q is not in the configured blocked set", p.Name))

			return
		}
	}

	pending := blockedChanges(v.policy, v.view.System)
	wanted := make([]string, 0, len(n.Packages.Packages))

	for _, p := range n.Packages.Packages {
		if slices.ContainsFunc(pending, func(c protocol.PackageChange) bool { return c.Name == p.Name }) {
			wanted = append(wanted, p.Name)
		}
	}

	if len(wanted) == 0 {
		v.verdict(n.ID, protocol.StepSuperseded, "the packages are already upgraded")

		return
	}

	for _, name := range wanted {
		v.packages[name] = struct{}{}
	}

	v.plan.PackageIDs = append(v.plan.PackageIDs, n.ID)
}

func (v *validation) image(n *protocol.Need) {
	if n.Image == nil || n.Image.Container == "" || n.Image.ToDigest == "" {
		v.verdict(n.ID, protocol.StepRefused, "malformed image need")

		return
	}

	name := n.Image.Container
	c := v.view.Container(name)

	if reason, refused := rollable(c, name); reason != "" {
		status := protocol.StepSuperseded
		if refused {
			status = protocol.StepRefused
		}

		v.verdict(n.ID, status, reason)

		return
	}

	switch {
	case n.Image.Image != "" && !imageref.Same(n.Image.Image, c.Image):
		v.verdict(n.ID, protocol.StepSuperseded, fmt.Sprintf(
			"container %s is on %s, the need was for %s", name, c.Image, n.Image.Image))

		return
	case n.DirectiveID == "" && !n.Local && c.Digest == n.Image.ToDigest:
		// A manual roll is served at the same digest: a person asked for it.
		v.verdict(n.ID, protocol.StepSuperseded, "container "+name+" already runs the wanted digest")

		return
	}

	// Guards a lease with several image needs for one container: it is
	// recreated once.
	if i, ok := v.byContainer[name]; ok {
		if v.plan.Recreates[i].Digest == n.Image.ToDigest {
			v.plan.Recreates[i].IDs = append(v.plan.Recreates[i].IDs, n.ID)
		} else {
			v.verdict(n.ID, protocol.StepSuperseded, "another need of this lease recreates container "+name)
		}

		return
	}

	v.byContainer[name] = len(v.plan.Recreates)
	v.plan.Recreates = append(v.plan.Recreates, Recreate{
		IDs: []string{n.ID}, Container: name, Digest: n.Image.ToDigest,
	})
}

// wasRunning are the containers the starting phase brings back, sorted;
// unlabelled containers only when the lease recreates them.
func wasRunning(p *LeasePlan, containers []protocol.Container) []string {
	out := make([]string, 0, len(containers))

	for i := range containers {
		c := &containers[i]
		if !c.Managed || !c.ShouldRun() {
			continue
		}

		if c.Kind != "" ||
			slices.ContainsFunc(p.Recreates, func(r Recreate) bool { return r.Container == c.Name }) {
			out = append(out, c.Name)
		}
	}

	slices.Sort(out)

	return out
}

// manualNeedOpen reports whether the agent holds a granted manual need open.
func manualNeedOpen(manual []protocol.Need, n *protocol.Need) bool {
	return slices.ContainsFunc(manual, func(m protocol.Need) bool {
		return m.ID == n.ID && m.DirectiveID == n.DirectiveID && m.Kind == n.Kind
	})
}

// recreatesTarget reports whether a need is a spec or override need.
func recreatesTarget(n *protocol.Need) bool {
	return n.Kind == protocol.NeedOverride || n.Kind == protocol.NeedSpec
}

// needTarget is a spec or override need's container and digest; empty when
// malformed.
func needTarget(n *protocol.Need) (container, digest string) {
	switch {
	case n.Kind == protocol.NeedOverride && n.Override != nil:
		return n.Override.Container, n.Override.ToDigest
	case n.Kind == protocol.NeedSpec && n.Spec != nil:
		return n.Spec.Container, n.Spec.ToDigest
	default:
		return "", ""
	}
}

// targetStale says why a spec or override need no longer applies; empty
// when it does. The need ID is recomputed with the need's own digest.
func targetStale(view *View, c *protocol.Container, name, needID, digest string) string {
	target, known := view.Targets[name]

	switch {
	case c == nil || !known:
		return "the container does not exist"
	case target.Err != "":
		return target.Err
	case target.InSync:
		return "container " + c.Name + " already runs what agent.yaml and the overrides file say"
	case target.needID(c, digest) != needID:
		return "agent.yaml or the overrides file asks for something else now"
	default:
		return ""
	}
}

// Recheck returns what a container is recreated as, from a fresh view right
// before the recreate, or why the recreate is superseded. Nothing of it
// comes from the lease. The view must be built from fresh facts.
func Recheck(task *Recreate, view *View) (rec Recreation, reason string) {
	name := task.Container
	c := view.Container(name)

	if task.Target != "" {
		if reason = targetStale(view, c, name, task.Target, task.Digest); reason != "" {
			return Recreation{}, reason
		}

		t := view.Targets[name]
		rec = Recreation{
			Image: t.Image, Command: slices.Clone(t.Command), SetCommand: t.Flags,
			Base: t.Base, SetBase: true,
		}
	} else if reason, _ = rollable(c, name); reason != "" {
		return Recreation{}, reason
	}

	return rec, ""
}

// StopTimeout is a container's stop grace period: its own, else
// DefaultStopTimeout.
func StopTimeout(c *protocol.Container) time.Duration {
	if c == nil || c.StopTimeout <= 0 {
		return DefaultStopTimeout
	}

	return c.StopTimeout
}

// Start the execution client first and the validator client last; stop
// in reverse. Every other kind, and an unlabelled container, comes after.
var (
	startOrder = []protocol.ContainerKind{protocol.KindEL, protocol.KindCL, protocol.KindVC}
	drainOrder = []protocol.ContainerKind{protocol.KindVC, protocol.KindCL, protocol.KindEL}
)

// StartOrder sorts container names el, cl, vc, the rest, then by name.
func StartOrder(names []string, containers []protocol.Container) []string {
	return ordered(names, containers, startOrder)
}

// DrainOrder sorts container names vc, cl, el, the rest, then by name.
func DrainOrder(names []string, containers []protocol.Container) []string {
	return ordered(names, containers, drainOrder)
}

func ordered(names []string, containers []protocol.Container, order []protocol.ContainerKind) []string {
	out := slices.Clone(names)

	slices.SortStableFunc(out, func(a, b string) int { return compareOrder(a, b, containers, order) })

	return out
}

func compareOrder(a, b string, containers []protocol.Container, order []protocol.ContainerKind) int {
	return cmp.Or(
		cmp.Compare(kindRank(order, kindOf(a, containers)), kindRank(order, kindOf(b, containers))),
		cmp.Compare(a, b),
	)
}

// kindOf is a managed container's kind; empty when it is not found or
// unlabelled.
func kindOf(name string, containers []protocol.Container) protocol.ContainerKind {
	for i := range containers {
		if c := &containers[i]; c.Name == name && c.Managed {
			return c.Kind
		}
	}

	return ""
}

func kindRank(order []protocol.ContainerKind, kind protocol.ContainerKind) int {
	if i := slices.Index(order, kind); i >= 0 {
		return i
	}

	return len(order)
}
