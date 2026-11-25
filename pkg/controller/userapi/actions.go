package userapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	maxActionHosts      = 2000
	maxActionContainers = 64
	maxNameLength       = 253
	maxReasonLength     = 500
)

// actions maps the action name in POST /api/v1/actions/{name} to its handler.
// Every action passes the authenticated actor to the scheduler, which applies
// and audits it.
//
// The second argument of each registration is the action's requirement: the
// feature that allows it and what it acts on (for the feature's team twin).
// This is the only place where an
// action says what it needs; an action cannot be registered without it.
func (s *Handler) actions() map[string]UserHandler {
	sched := s.opts.Scheduler

	return map[string]UserHandler{
		"roll":   action(s, needRoll, validateRoll, sched.Roll),
		"reboot": action(s, needManage[scheduler.RebootRequest], validateReboot, sched.Reboot),
		// A configured host action; permitted per host: see handleHostAction.
		"host-action": s.handleHostAction,
		"hold":        action(s, needHold, validateHostsRequest, sched.Hold),
		"unhold":      action(s, needUnhold, validateUnhold, sched.Unhold),
		// A host known to be offline is left out of the gate and the budget.
		"mark-offline":   action(s, needManage[scheduler.OfflineRequest], s.validateMarkOffline, sched.MarkOffline),
		"unmark-offline": action(s, needManage[scheduler.OfflineRequest], validateUnmarkOffline, sched.UnmarkOffline),
		// Instructions for a host's overrides file; its agent decides.
		"image-override": action(s, needImageOverride, validateImageOverride, sched.ImageOverride),
		"flag-override":  action(s, needFlagOverride, validateFlagOverride, sched.FlagOverride),
		// Many hosts at once, each judged alone: see handleClientOverride.
		"client-override": s.handleClientOverride,
		// The image policy of one client.
		"client-policy": action(s, needClientPolicy, validateClientPolicy, sched.SetClientPolicy),
		"forget":        action(s, needAdmin[scheduler.HoldRequest], validateHostsRequest, sched.Forget),
		// Ask for the end of one open lease: it is marked, and ends as cancelled.
		"end-lease": action(s, needEndLease, validateEndLease, sched.EndLease),
		"pause":     action(s, needScope, validateScope(false), sched.Pause),
		// needScope, or the action's own permission: see handleResume.
		"resume": s.handleResume,
		"abort":  action(s, needAbort, validateScope(true), sched.Abort),
		"skip":   action(s, needRecoverHosts, validateRolloutHosts(true), sched.Skip),
		"retry":  action(s, needRetry, validateRolloutHosts(false), sched.Retry),
		// Whoever may approve the rollout: see allowApproval.
		"ignore-gate":   s.handleIgnoreGate,
		"ignore-budget": s.handleIgnoreBudget,
		"approve-lease": s.handleApproveLease,
	}
}

// needRoll: manage, or team_manage for the containers the roll touches.
func needRoll(req *scheduler.RollRequest) authz.Requirement {
	return authz.Requirement{
		Feature: authz.FeatureManage, Team: authz.ContainerTargets(req.Hosts, req.All, req.Containers),
	}
}

// needManage: manage; the twin does not allow it (reboot, mark-offline,
// unmark-offline).
func needManage[T any](*T) authz.Requirement {
	return authz.Requirement{Feature: authz.FeatureManage}
}

// needHold: manage (staff's hold), or team_manage for the named hosts (the
// holds of the token's teams there).
func needHold(req *scheduler.HoldRequest) authz.Requirement {
	return authz.Requirement{Feature: authz.FeatureManage, Team: authz.HoldTargets(req.Hosts, nil)}
}

// needUnhold: manage, or team_manage for the named hosts and only for holds
// of the token's teams.
func needUnhold(req *scheduler.HoldRequest) authz.Requirement {
	return authz.Requirement{Feature: authz.FeatureManage, Team: authz.HoldTargets(req.Hosts, req.Holders)}
}

// validateMarkOffline: hosts as for a hold, a reason, and an end, if any, in
// the future.
func (s *Handler) validateMarkOffline(req *scheduler.OfflineRequest) error {
	if err := validateUnmarkOffline(req); err != nil {
		return err
	}

	if strings.TrimSpace(req.Reason) == "" {
		return httpjson.BadRequestf("reason is required")
	}

	if req.Until != nil && !req.Until.After(s.now()) {
		return httpjson.BadRequestf("until must be in the future")
	}

	return nil
}

// validateUnmarkOffline: hosts as for a hold.
func validateUnmarkOffline(req *scheduler.OfflineRequest) error {
	return validateHostsRequest(&scheduler.HoldRequest{Hosts: req.Hosts, Reason: req.Reason})
}

// needImageOverride: manage_image, or team_manage_image for that one
// container of the host.
func needImageOverride(req *scheduler.ImageOverrideRequest) authz.Requirement {
	return authz.Requirement{
		Feature: authz.FeatureManageImage,
		Team:    authz.ContainerTargets([]string{req.Host}, false, []string{req.Container}),
	}
}

// needFlagOverride: manage_flags, or team_manage_flags for that one
// container of the host.
func needFlagOverride(req *scheduler.FlagOverrideRequest) authz.Requirement {
	return authz.Requirement{
		Feature: authz.FeatureManageFlags,
		Team:    authz.ContainerTargets([]string{req.Host}, false, []string{req.Container}),
	}
}

// needClientPolicy: manage, or team_manage for a client whose reported
// containers are all owned by a team of the token.
func needClientPolicy(req *scheduler.ClientPolicyRequest) authz.Requirement {
	return authz.Requirement{Feature: authz.FeatureManage, Team: &authz.Targets{Client: req.Client}}
}

// needAdmin: admin and nothing else (forget).
func needAdmin[T any](*T) authz.Requirement {
	return authz.Requirement{Feature: authz.FeatureAdmin}
}

// needEndLease: manage, or team_manage for a lease that only rolls
// containers of the token's teams.
func needEndLease(req *scheduler.EndLeaseRequest) authz.Requirement {
	return authz.Requirement{Feature: authz.FeatureManage, Team: authz.RecoverLeaseTarget(req.Lease)}
}

// needScope: manage, or team_manage for a rollout of the team, for pause
// and resume (which approves) of one rollout; without a rollout the
// request is the global switch and needs admin.
func needScope(req *scheduler.ScopeRequest) authz.Requirement {
	if req.Rollout == "" {
		return authz.Requirement{Feature: authz.FeatureAdmin}
	}

	return authz.Requirement{Feature: authz.FeatureManage, Team: authz.RolloutTarget(req.Rollout)}
}

// needAbort: manage, or team_manage for a rollout of the team.
func needAbort(req *scheduler.ScopeRequest) authz.Requirement {
	return authz.Requirement{Feature: authz.FeatureManage, Team: authz.RolloutTarget(req.Rollout)}
}

// needRetry: without hosts it reopens the whole rollout and needs manage, or
// team_manage for a rollout of the team. With hosts it is the recovery of
// those hosts, like skip. An aborted rollout is retried by nobody (the
// scheduler answers 409).
func needRetry(req *scheduler.RolloutHostsRequest) authz.Requirement {
	if len(req.Hosts) == 0 {
		return authz.Requirement{Feature: authz.FeatureManage, Team: authz.RolloutTarget(req.Rollout)}
	}

	return needRecoverHosts(req)
}

// needRecoverHosts: manage, or team_manage for members of the rollout that
// only roll containers of the token's teams (skip, retry with hosts).
func needRecoverHosts(req *scheduler.RolloutHostsRequest) authz.Requirement {
	return authz.Requirement{
		Feature: authz.FeatureManage, Team: authz.RecoverMembersTarget(req.Rollout, req.Hosts),
	}
}

// action builds the handler of one action: decode, authorize, validate, call,
// answer. The authorization comes before the validation, so a login that
// lacks the feature is told so whatever the body says, and before the
// scheduler, so a refused request changes nothing.
func action[T any](
	s *Handler,
	need func(*T) authz.Requirement,
	validate func(*T) error,
	call func(ctx context.Context, actor string, req T) (*scheduler.ActionResult, error),
) UserHandler {
	return func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		var req T
		if err := decodeBody(w, r, &req); err != nil {
			s.writeBodyError(w, err)

			return
		}

		required := need(&req)

		ctx, ok := s.authz.Allow(w, r, u, &required)
		if !ok {
			return
		}

		if err := validate(&req); err != nil {
			httpjson.BadRequest(s.log, w, err)

			return
		}

		result, err := call(ctx, u.Actor, req)
		if err != nil {
			s.writeSchedulerError(w, r, u, err)

			return
		}

		if result == nil {
			result = &scheduler.ActionResult{Message: "ok"}
		}

		s.opts.Notify()
		httpjson.Write(s.log, w, http.StatusOK, result)
	}
}

// writeSchedulerError maps a scheduler error to its status: an unknown host,
// rollout or lease is 404, an action that cannot apply now is 409, one the
// request's authorization does not allow on the state the scheduler finds is
// 403. Those messages
// are written by the scheduler for people; anything else stays internal.
func (s *Handler) writeSchedulerError(w http.ResponseWriter, r *http.Request, u *authz.User, err error) {
	switch {
	case errors.Is(err, scheduler.ErrUnknownHost), errors.Is(err, scheduler.ErrUnknownRollout),
		errors.Is(err, scheduler.ErrUnknownLease):
		httpjson.Error(s.log, w, http.StatusNotFound, err.Error())
	case errors.Is(err, scheduler.ErrInvalidAction):
		httpjson.Error(s.log, w, http.StatusConflict, err.Error())
	case errors.Is(err, scheduler.ErrForbidden):
		// The scheduler asked again, under its lock, what authorize asked
		// before: the answer has the same shape.
		msg := strings.TrimPrefix(err.Error(), scheduler.ErrForbidden.Error()+": ")
		s.authz.Refuse(w, r, u, "forbidden: "+s.opts.AuthService+":"+msg)
	default:
		httpjson.InternalError(s.log, w, r, err)
	}
}

func validateReason(reason string) error {
	if utf8.RuneCountInString(reason) > maxReasonLength {
		return httpjson.BadRequestf("reason is longer than %d characters", maxReasonLength)
	}

	return nil
}

// validateHosts checks a host list. Host names are certificate subjects, so
// they obey the identity name rules.
func validateHosts(hosts []string, required bool) error {
	if required && len(hosts) == 0 {
		return httpjson.BadRequestf("hosts must not be empty")
	}

	if len(hosts) > maxActionHosts {
		return httpjson.BadRequestf("more than %d hosts", maxActionHosts)
	}

	seen := make(map[string]struct{}, len(hosts))

	for _, host := range hosts {
		if err := identity.ValidateName(host); err != nil {
			return httpjson.BadRequestf("invalid host name %q", host)
		}

		if _, dup := seen[host]; dup {
			return httpjson.BadRequestf("host %q is listed twice", host)
		}

		seen[host] = struct{}{}
	}

	return nil
}

// validateSelection checks the "these hosts or all" pair of roll and reboot.
func validateSelection(hosts []string, all bool) error {
	switch {
	case all && len(hosts) > 0:
		return httpjson.BadRequestf("give hosts or all, not both")
	case !all && len(hosts) == 0:
		return httpjson.BadRequestf("give hosts, or set all")
	}

	return validateHosts(hosts, false)
}

func validateRoll(req *scheduler.RollRequest) error {
	if err := validateSelection(req.Hosts, req.All); err != nil {
		return err
	}

	if len(req.Containers) > maxActionContainers {
		return httpjson.BadRequestf("more than %d containers", maxActionContainers)
	}

	for _, name := range req.Containers {
		if name == "" || len(name) > maxNameLength {
			return httpjson.BadRequestf("invalid container name %q", name)
		}
	}

	return validateReason(req.Reason)
}

// Bounds of what an override action carries. The agent has its own.
const (
	maxOverrideImage   = 512
	maxOverrideFlags   = 64
	maxOverrideFlagLen = 2048
)

func validateOverrideTarget(host, container, reason string) error {
	if err := validateHosts([]string{host}, true); err != nil {
		return err
	}

	if container == "" || len(container) > maxNameLength {
		return httpjson.BadRequestf("invalid container name %q", container)
	}

	return validateReason(reason)
}

func validateImageOverride(req *scheduler.ImageOverrideRequest) error {
	if len(req.Image) > maxOverrideImage || strings.ContainsAny(req.Image, " \t\r\n") {
		return httpjson.BadRequestf("image must be an image reference")
	}

	return validateOverrideTarget(req.Host, req.Container, req.Reason)
}

func validateFlagOverride(req *scheduler.FlagOverrideRequest) error {
	if len(req.AddFlags)+len(req.RemoveFlags) > maxOverrideFlags {
		return httpjson.BadRequestf("more than %d flags", maxOverrideFlags)
	}

	for _, flag := range slices.Concat(req.AddFlags, req.RemoveFlags) {
		if flag == "" || len(flag) > maxOverrideFlagLen || strings.TrimSpace(flag) != flag ||
			strings.ContainsAny(flag, "\x00\r\n") || strings.Count(flag, " ") > 1 {
			return httpjson.BadRequestf("invalid flag %q", flag)
		}
	}

	return validateOverrideTarget(req.Host, req.Container, req.Reason)
}

func validateClientPolicy(req *scheduler.ClientPolicyRequest) error {
	if !protocol.ValidClient(req.Client) {
		return httpjson.BadRequestf("invalid client name %q", req.Client)
	}

	switch req.Policy {
	case "", fleet.PolicyAuto, fleet.PolicyManual:
	default:
		return httpjson.BadRequestf("policy must be auto, manual or empty (the default)")
	}

	return validateReason(req.Reason)
}

func validateReboot(req *scheduler.RebootRequest) error {
	if err := validateSelection(req.Hosts, req.All); err != nil {
		return err
	}

	return validateReason(req.Reason)
}

func validateHostsRequest(req *scheduler.HoldRequest) error {
	if len(req.Holders) > 0 {
		return httpjson.BadRequestf("holders are named only to unhold")
	}

	if err := validateHosts(req.Hosts, true); err != nil {
		return err
	}

	return validateReason(req.Reason)
}

// maxHolders bounds the holders an unhold names: staff and the teams.
const maxHolders = 64

// validateUnhold: hosts as for a hold, and the holders, if named, as
// protocol.ValidHolder allows them: staff or a team, never the host.
func validateUnhold(req *scheduler.HoldRequest) error {
	if len(req.Holders) > maxHolders {
		return httpjson.BadRequestf("more than %d holders", maxHolders)
	}

	for i, holder := range req.Holders {
		if !protocol.ValidHolder(holder) {
			return httpjson.BadRequestf("invalid holder %q", authz.Echo(holder))
		}

		if slices.Contains(req.Holders[:i], holder) {
			return httpjson.BadRequestf("holder %q is listed twice", authz.Echo(holder))
		}
	}

	return validateHostsRequest(&scheduler.HoldRequest{Hosts: req.Hosts, Reason: req.Reason})
}

// validateEndLease checks the end-lease action: one lease, by its ID.
func validateEndLease(req *scheduler.EndLeaseRequest) error {
	if req.Lease == "" {
		return httpjson.BadRequestf("lease must not be empty")
	}

	if len(req.Lease) > maxNameLength {
		return httpjson.BadRequestf("invalid lease id")
	}

	return validateReason(req.Reason)
}

func validateRolloutID(id string, required bool) error {
	if required && id == "" {
		return httpjson.BadRequestf("rollout must not be empty")
	}

	if len(id) > maxNameLength {
		return httpjson.BadRequestf("invalid rollout id")
	}

	return nil
}

// validateScope checks pause, resume and abort; abort needs a rollout.
func validateScope(rolloutRequired bool) func(*scheduler.ScopeRequest) error {
	return func(req *scheduler.ScopeRequest) error {
		if err := validateRolloutID(req.Rollout, rolloutRequired); err != nil {
			return err
		}

		return validateReason(req.Reason)
	}
}

// validateRolloutHosts checks skip (hosts required) and retry (no hosts
// means every failed host).
func validateRolloutHosts(hostsRequired bool) func(*scheduler.RolloutHostsRequest) error {
	return func(req *scheduler.RolloutHostsRequest) error {
		if err := validateRolloutID(req.Rollout, true); err != nil {
			return err
		}

		if err := validateHosts(req.Hosts, hostsRequired); err != nil {
			return err
		}

		return validateReason(req.Reason)
	}
}
