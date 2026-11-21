package agent

import (
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

// localAPI is the agent behind the CLI socket. A change writes an override
// entry or the local hold, or adds a roll for the loop, and wakes the loop.
type localAPI struct{ a *agent }

var _ local.Agent = localAPI{}

// Status returns the last published status with the rolls as they are now.
func (l localAPI) Status() local.Status {
	a := l.a

	a.mu.Lock()
	out := a.status
	a.mu.Unlock()

	out.Rolls = a.rolls.List()

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

// lastView is the view of the last cycle; an empty one before the first.
func (l localAPI) lastView() *plan.View {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()

	if l.a.view == nil {
		return &plan.View{}
	}

	return l.a.view
}
