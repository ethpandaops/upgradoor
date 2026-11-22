package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// deployIgnored are the docker_container comparisons Ansible leaves to the
// agent for a container the agent rolls; labels too, since a recreate adds
// the override labels.
var deployIgnored = []string{"image", "command", "labels"}

// localAPI is the agent behind the CLI socket. A change writes an override
// entry or the local hold, or adds a roll for the loop, and wakes the loop.
type localAPI struct{ a *agent }

var _ local.Agent = localAPI{}

// Status returns the last published status with the rolls as they are now,
// and the containers, hold and needs from its facts and the host's files as
// they are now.
func (l localAPI) Status() local.Status {
	a := l.a

	a.mu.Lock()
	out, facts := a.status, a.seen
	a.mu.Unlock()

	out.Rolls = a.rolls.List()

	if facts == nil {
		return out
	}

	files := a.inputs()
	view := plan.Classify(&a.cfg.Policy, facts, &files, a.tracker.Checks(), a.now())
	view.ActionsRan = a.ranActions()
	manual, _ := a.state.ManualAndCurrent()

	out.Hold, out.OverridesError = view.Hold, view.OverridesError
	out.Containers = local.ContainersOf(&view)
	out.Needs = plan.Needs(&a.cfg.Policy, &view, manual)

	return out
}

// Override sets or clears the override of one container as
// plan.LocalEntry decides.
func (l localAPI) Override(user string, req *local.OverrideRequest) (string, error) {
	a := l.a
	view := l.lastView()
	rec := local.Record{
		Source: local.SourceLocal, User: user, Container: req.Container, Reason: plan.Line(req.Reason),
	}
	asked := &plan.LocalRequest{
		Container: req.Container, Image: req.Image, Add: req.AddFlags, Remove: req.RemoveFlags,
		Until: req.Until, User: user, Reason: req.Reason, Clear: req.Clear,
	}

	var built overrideBuild

	deployed := a.deployed(req.Container)

	prev, next, err := a.files.SetEntry(req.Container, user, built.wrap(
		func(*plan.Entry) (*plan.Entry, error) {
			return plan.LocalEntry(view, asked, deployed, a.now())
		}))

	a.auditOverride(&rec, &built, prev, next, err)

	if err != nil {
		return "", err
	}

	a.wake()

	if next == nil {
		return "", nil
	}

	return next.Describe(), nil
}

// Hold sets the local hold plan.LocalHold builds in the state file, or
// releases it.
func (l localAPI) Hold(user string, req *local.HoldRequest) error {
	a := l.a
	rec := &local.Record{
		Action: "hold.set", Source: local.SourceLocal, User: user, Reason: plan.Line(req.Reason),
		Result: local.Accepted,
	}

	var err error

	if req.Release {
		rec.Action = "hold.release"

		if reason := noLocalHold(a.state.Holds(), a.now()); reason != "" {
			rec.Result, rec.Error = local.Refused, reason
			a.audit.Record(rec)

			return plan.Refusal("%s", reason)
		}

		err = a.state.SetHold(protocol.HolderHost, nil)
	} else {
		hold := plan.LocalHold(user, req.Reason, a.now())
		err = a.state.SetHold(protocol.HolderHost, &hold)
	}

	if err != nil {
		rec.Result, rec.Error = local.Failed, err.Error()
	}

	a.audit.Record(rec)

	if err == nil {
		a.wake()
	}

	return err
}

// noLocalHold says why a release has nothing to release, naming the holds
// of the controller that stay; empty when there is a local hold.
func noLocalHold(holds map[string]plan.Hold, now time.Time) string {
	if _, ok := holds[protocol.HolderHost]; ok {
		return ""
	}

	held := make([]string, 0, len(holds))

	for _, holder := range slices.Sorted(maps.Keys(holds)) {
		h := holds[holder]
		if h.Expired(now) {
			continue
		}

		part := holder
		if h.By != "" {
			part += " by " + h.By
		}

		if h.Reason != "" {
			part += " (" + h.Reason + ")"
		}

		held = append(held, part)
	}

	if len(held) == 0 {
		return "there is no local hold"
	}

	return "there is no local hold; the host is held by " + strings.Join(held, ", ") +
		", lifted on the controller"
}

// Roll adds a roll unless plan.RollRefusal refuses it against the last view
// with the hold as it is now.
func (l localAPI) Roll(user, container string) (*local.Roll, error) {
	a := l.a
	rec := &local.Record{
		Action: "roll.request", Source: local.SourceLocal, User: user, Container: container,
		Result: local.Accepted,
	}

	view := *l.lastView()
	files := a.inputs()
	view.Hold = plan.HoldState(&files, a.now())

	var (
		roll local.Roll
		err  error
	)

	if reason := plan.RollRefusal(&a.cfg.Policy, &view, container); reason != "" {
		err = plan.Refusal("%s", reason)
	} else {
		roll, err = a.rolls.Add(user, container)
	}

	if err != nil {
		rec.Result, rec.Error = local.Refused, err.Error()
		a.audit.Record(rec)

		return nil, err
	}

	a.audit.Record(rec)
	a.wake()

	return &roll, nil
}

// InitOverrides lays out the overrides file from the last view.
func (l localAPI) InitOverrides(user string) (string, error) {
	a := l.a
	files := a.inputs()

	keep, drop := plan.OverrideKeys(l.lastView(), &files)
	if _, err := a.files.Layout(keep, drop); err != nil {
		return "", err
	}

	a.audit.Record(&local.Record{
		Action: "overrides.init", Source: local.SourceLocal, User: user, Result: local.Accepted,
	})

	return a.files.Path(), nil
}

// Deploy reads managed.yaml again after upgradoor deploy wrote the
// container's entry and, with Reset, clears its override; the overrides
// file's default lines follow. A container the agent rolls and that runs
// something else than its target is reported now, in the staggered rollout
// of its team and kind.
func (l localAPI) Deploy(
	ctx context.Context, user string, req *local.DeployRequest,
) (*local.DeployResult, error) {
	a := l.a

	before := a.deployed(req.Container)

	a.reloadManaged()

	d := a.deployed(req.Container)
	if d == nil {
		return nil, plan.Refusal("managed.yaml has no entry for container %s", req.Container)
	}

	changed := before == nil || !sameDeployed(before, d)

	var err error

	if req.Reset && a.files.Path() != "" {
		var prev *plan.Entry

		prev, _, err = a.files.SetEntry(req.Container, user, func(*plan.Entry) (*plan.Entry, error) {
			return nil, nil //nolint:nilnil // nil clears the override.
		})
		changed = changed || prev != nil
	}

	if err != nil || changed {
		rec := &local.Record{
			Action: "deploy", Source: local.SourceLocal, User: user, Container: req.Container,
			After: deployedText(d), Result: local.Accepted,
		}

		if req.Reset {
			rec.Reason = "reset"
		}

		if err != nil {
			rec.Result, rec.Error = local.Failed, err.Error()
		}

		a.audit.Record(rec)
	}

	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, observeTimeout)
	list, err := a.host.Containers(cctx)

	cancel()

	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	files := a.inputs()
	facts := plan.Facts{Containers: list, Fresh: true}
	view := plan.Classify(&a.cfg.Policy, &facts, &files, a.tracker.Checks(), a.now())
	exists, managed, differs := plan.DeployState(&a.cfg.Policy, &view, req.Container)

	a.layout(&view, &files)

	out := &local.DeployResult{
		Container: req.Container, Exists: exists, Managed: managed, Changed: changed,
		Roll: local.DeployRollNone, Ignore: []string{}, Pull: !managed,
	}

	if managed {
		out.Ignore = deployIgnored
	}

	if differs {
		out.Roll = local.DeployRollRequested

		a.wake()
	}

	return out, nil
}

// deployed is a container's entry of managed.yaml as last read, or nil.
func (a *agent) deployed(container string) *plan.Deployed {
	a.managedMu.Lock()
	defer a.managedMu.Unlock()

	d, ok := a.managed[container]
	if !ok {
		return nil
	}

	d.Command = slices.Clone(d.Command)

	return &d
}

func sameDeployed(a, b *plan.Deployed) bool {
	return a.Image == b.Image && a.HasCommand == b.HasCommand && slices.Equal(a.Command, b.Command)
}

// deployedText is a deployed entry for the audit log.
func deployedText(d *plan.Deployed) string {
	if !d.HasCommand {
		return "image " + d.Image
	}

	return "image " + d.Image + ", command " + strings.Join(d.Command, " ")
}

// lastView is the view of the last cycle; an empty one before the first.
func (l localAPI) lastView() *plan.View {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()

	if l.a.view == nil {
		return &plan.View{}
	}

	return l.a.view
}
