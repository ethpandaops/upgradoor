package controllertest

import (
	"context"
	"slices"
	"sync"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
)

// Call is one recorded scheduler action.
type Call struct {
	Name  string
	Actor string
	Req   any
	// Auth is the authorization the call's context carried.
	Auth scheduler.Authorization
}

// OpenAuth is the authorization of every call in open mode.
var OpenAuth = scheduler.Authorization{By: authz.AuthorizedByOpen}

// authOf returns the authorization a scheduler call was made with.
func AuthOf(ctx context.Context) scheduler.Authorization {
	a, _ := scheduler.AuthorizationFrom(ctx)

	return a
}

// Scheduler is a recording scheduler.Service.
type Scheduler struct {
	sync.Mutex

	CurrentPlan     *scheduler.Plan
	CurrentSettings fleet.Settings
	Assignments     map[string]*scheduler.Assignment
	Calls           []Call
	Nudges          int

	// ActionErr fails every action and PatchSettings; AssignmentErr fails
	// Assignment; SettingsErr fails Settings.
	ActionErr     error
	AssignmentErr error
	SettingsErr   error
	// Result is returned by every action.
	Result *scheduler.ActionResult
}

var _ scheduler.Service = (*Scheduler)(nil)

func NewScheduler() *Scheduler {
	return &Scheduler{
		CurrentSettings: fleet.DefaultSettings(),
		Assignments:     make(map[string]*scheduler.Assignment, 2),
		Result:          &scheduler.ActionResult{Message: "done", Hosts: []string{"a"}},
	}
}

func (f *Scheduler) Start(context.Context) error { return nil }
func (f *Scheduler) Stop() error                 { return nil }

func (f *Scheduler) Nudge() {
	f.Lock()
	defer f.Unlock()

	f.Nudges++
}

func (f *Scheduler) Assignment(_ context.Context, host string) (*scheduler.Assignment, error) {
	f.Lock()
	defer f.Unlock()

	if f.AssignmentErr != nil {
		return nil, f.AssignmentErr
	}

	if a, ok := f.Assignments[host]; ok {
		return a, nil
	}

	return &scheduler.Assignment{}, nil
}

func (f *Scheduler) Plan() *scheduler.Plan {
	f.Lock()
	defer f.Unlock()

	return f.CurrentPlan
}

func (f *Scheduler) Settings(context.Context) (fleet.Settings, error) {
	f.Lock()
	defer f.Unlock()

	return f.CurrentSettings, f.SettingsErr
}

func (f *Scheduler) PatchSettings(
	ctx context.Context, actor string, apply func(*fleet.Settings) error,
) (fleet.Settings, bool, error) {
	f.Lock()
	defer f.Unlock()

	if f.SettingsErr != nil {
		return fleet.Settings{}, false, f.SettingsErr
	}

	next := f.CurrentSettings
	if err := apply(&next); err != nil {
		return f.CurrentSettings, false, err
	}

	if next.Equal(&f.CurrentSettings) {
		return f.CurrentSettings, false, nil
	}

	f.Calls = append(f.Calls, Call{Name: "settings", Actor: actor, Req: next, Auth: AuthOf(ctx)})

	if f.ActionErr != nil {
		return f.CurrentSettings, false, f.ActionErr
	}

	f.CurrentSettings = next

	return next, true, nil
}

func (f *Scheduler) record(ctx context.Context, name, actor string, req any) (*scheduler.ActionResult, error) {
	f.Lock()
	defer f.Unlock()

	f.Calls = append(f.Calls, Call{Name: name, Actor: actor, Req: req, Auth: AuthOf(ctx)})

	if f.ActionErr != nil {
		return nil, f.ActionErr
	}

	return f.Result, nil
}

func (f *Scheduler) Roll(ctx context.Context, actor string, req scheduler.RollRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "roll", actor, req)
}

func (f *Scheduler) Reboot(ctx context.Context, actor string, req scheduler.RebootRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "reboot", actor, req)
}

func (f *Scheduler) HostAction(ctx context.Context, actor string, req scheduler.HostActionRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "host-action", actor, req)
}

func (f *Scheduler) Hold(ctx context.Context, actor string, req scheduler.HoldRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "hold", actor, req)
}

func (f *Scheduler) ImageOverride(
	ctx context.Context, actor string, req scheduler.ImageOverrideRequest,
) (*scheduler.ActionResult, error) {
	return f.record(ctx, "image-override", actor, req)
}

func (f *Scheduler) FlagOverride(
	ctx context.Context, actor string, req scheduler.FlagOverrideRequest,
) (*scheduler.ActionResult, error) {
	return f.record(ctx, "flag-override", actor, req)
}

func (f *Scheduler) ClientOverride(
	ctx context.Context, actor string, req scheduler.ClientOverrideRequest,
) (*scheduler.ActionResult, error) {
	res, err := f.record(ctx, "client-override", actor, req)
	if res == nil {
		return res, err
	}

	out := *res
	out.Hosts = req.Hosts

	return &out, err
}

func (f *Scheduler) SetClientPolicy(
	ctx context.Context, actor string, req scheduler.ClientPolicyRequest,
) (*scheduler.ActionResult, error) {
	return f.record(ctx, "client-policy", actor, req)
}

func (f *Scheduler) Unhold(ctx context.Context, actor string, req scheduler.HoldRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "unhold", actor, req)
}

func (f *Scheduler) MarkOffline(ctx context.Context, actor string, req scheduler.OfflineRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "mark-offline", actor, req)
}

func (f *Scheduler) UnmarkOffline(ctx context.Context, actor string, req scheduler.OfflineRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "unmark-offline", actor, req)
}

func (f *Scheduler) Pause(ctx context.Context, actor string, req scheduler.ScopeRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "pause", actor, req)
}

func (f *Scheduler) Resume(ctx context.Context, actor string, req scheduler.ScopeRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "resume", actor, req)
}

func (f *Scheduler) Skip(ctx context.Context, actor string, req scheduler.RolloutHostsRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "skip", actor, req)
}

func (f *Scheduler) Retry(ctx context.Context, actor string, req scheduler.RolloutHostsRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "retry", actor, req)
}

func (f *Scheduler) Abort(ctx context.Context, actor string, req scheduler.ScopeRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "abort", actor, req)
}

func (f *Scheduler) Forget(ctx context.Context, actor string, req scheduler.HoldRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "forget", actor, req)
}

func (f *Scheduler) EndLease(ctx context.Context, actor string, req scheduler.EndLeaseRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record(ctx, "end-lease", actor, req)
}

func (f *Scheduler) Recorded() []Call {
	f.Lock()
	defer f.Unlock()

	return slices.Clone(f.Calls)
}

func (f *Scheduler) NudgeCount() int {
	f.Lock()
	defer f.Unlock()

	return f.Nudges
}

func (f *Scheduler) Set(mod func(*Scheduler)) {
	f.Lock()
	defer f.Unlock()

	mod(f)
}
