package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// deployIgnored are the docker_container comparisons Ansible leaves to the
// agent for a container the agent rolls; labels too, so that a container the
// name map groups is not recreated to get them.
var deployIgnored = []string{"image", "command", "labels"}

// localAPI is the agent behind the CLI socket. A change writes an override
// entry or the local hold, or adds a roll for the loop, and wakes the loop.
type localAPI struct{ a *agent }

var _ local.Agent = localAPI{}

// Status returns the last published status with the rolls as they are now,
// and the containers, hold and needs from its facts and the overrides file
// as it is now.
func (l localAPI) Status() local.Status {
	a := l.a

	a.mu.Lock()
	out, facts := a.status, a.seen
	a.mu.Unlock()

	out.Rolls = a.rolls.List()

	if facts == nil {
		return out
	}

	files := a.files.Load()
	view := plan.Classify(&a.cfg.Policy, facts, &files, a.tracker.Checks(), a.now())
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

	prev, next, err := a.files.SetEntry(req.Container, user, built.wrap(
		func(*plan.Entry) (*plan.Entry, error) {
			return plan.LocalEntry(&a.cfg.Policy, view, asked, a.now())
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

// Hold sets the local hold plan.LocalHold builds, or releases it. The
// history line names the bare user for a set and "local:<user>" for a
// release.
func (l localAPI) Hold(user string, req *local.HoldRequest) error {
	a := l.a
	rec := &local.Record{
		Action: "hold.set", Source: local.SourceLocal, User: user, Reason: plan.Line(req.Reason),
		Result: local.Accepted,
	}

	var err error

	if req.Release {
		rec.Action = "hold.release"

		if reason := noLocalHold(a.files.Load(), a.now()); reason != "" {
			rec.Result, rec.Error = local.Refused, reason
			a.audit.Record(rec)

			return plan.Refusal("%s", reason)
		}

		err = a.files.SetHold(plan.FromLocal, nil, user, "released")
	} else {
		hold := plan.LocalHold(user, req.Reason)
		err = a.files.SetHold(plan.FromLocal, &hold, hold.By, "set: "+hold.Reason)
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

// noLocalHold says why a release has nothing to release, naming the
// controller's hold that stays; empty when there is a local hold or the
// file cannot be read.
func noLocalHold(files plan.Overrides, now time.Time) string {
	if _, ok := files.Holds[plan.HoldLocal]; ok || !files.Known {
		return ""
	}

	h, ok := files.Holds[plan.HoldController]
	if !ok || h.Expired(now) {
		return "there is no local hold"
	}

	msg := "there is no local hold; the host is on controller hold"

	if h.By != "" {
		msg += " by " + h.By
	}

	if h.Reason != "" {
		msg += " (" + h.Reason + ")"
	}

	return msg + ", which is lifted on the controller"
}

// Roll adds a roll unless plan.RollRefusal refuses it against the last view
// with the hold as the file says now.
func (l localAPI) Roll(user, container string) (*local.Roll, error) {
	a := l.a
	rec := &local.Record{
		Action: "roll.request", Source: local.SourceLocal, User: user, Container: container,
		Result: local.Accepted,
	}

	view := *l.lastView()
	files := a.files.Load()
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

// InitOverrides writes the generated block from what the managed containers
// of the last view are deployed as.
func (l localAPI) InitOverrides(user string) (string, error) {
	a := l.a

	if err := a.files.InitBlock(plan.Deployment(l.lastView())); err != nil {
		return "", err
	}

	a.audit.Record(&local.Record{
		Action: "overrides.init", Source: local.SourceLocal, User: user, Result: local.Accepted,
	})

	return a.files.Path(), nil
}

// Deploy writes what Ansible deployed a container as and, with Reset, drops
// its override. A container the agent rolls and that runs something else
// than its target is reported now, in its group's staggered rollout.
func (l localAPI) Deploy(
	ctx context.Context, user string, req *local.DeployRequest,
) (*local.DeployResult, error) {
	a := l.a

	if _, err := imageref.Parse(req.Image); err != nil {
		return nil, plan.Refusal("image: %v", err)
	}

	d := plan.Deployed{Image: req.Image, Command: req.Command, HasCommand: req.Command != nil}

	changed, err := a.files.SetDeployed(req.Container, d, req.Reset, user)
	if err != nil || changed {
		rec := &local.Record{
			Action: "deploy", Source: local.SourceLocal, User: user, Container: req.Container,
			After: deployedText(&d), Result: local.Accepted,
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

	files := a.files.Load()
	facts := plan.Facts{Containers: list, Fresh: true}
	view := plan.Classify(&a.cfg.Policy, &facts, &files, a.tracker.Checks(), a.now())
	exists, managed, differs := plan.DeployState(&a.cfg.Policy, &view, req.Container)

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
