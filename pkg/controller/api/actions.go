package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
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
// feature that allows it, what it acts on (for the feature's team twin) and
// whether the request needs admin as well. This is the only place where an
// action says what it needs; an action cannot be registered without it.
func (s *Server) actions() map[string]userHandler {
	sched := s.opts.Scheduler

	return map[string]userHandler{
		"roll":   action(s, needRoll, validateRoll, sched.Roll),
		"reboot": action(s, needReboot, validateReboot, sched.Reboot),
		"hold":   action(s, needHold, validateHostsRequest, sched.Hold),
		"unhold": action(s, needUnhold, validateHostsRequest, sched.Unhold),
		// A host known to be offline is left out of the gate and the budget.
		"mark-offline":   action(s, needOffline, s.validateMarkOffline, sched.MarkOffline),
		"unmark-offline": action(s, needOffline, validateUnmarkOffline, sched.UnmarkOffline),
		// Instructions for a host's overrides file; its agent decides.
		"image-override": action(s, needImageOverride, validateImageOverride, sched.ImageOverride),
		"flag-override":  action(s, needFlagOverride, validateFlagOverride, sched.FlagOverride),
		// Many hosts at once, each judged alone: see handleClientOverride.
		"client-override": s.handleClientOverride,
		// The image policy of one client.
		"client-policy": action(s, needClientPolicy, validateClientPolicy, sched.SetClientPolicy),
		"forget":        action(s, needAdmin[scheduler.HoldRequest], validateHostsRequest, sched.Forget),
		// Ask for the end of one open lease: it is marked, and ends as failed.
		"end-lease": action(s, needEndLease, validateEndLease, sched.EndLease),
		"pause":     action(s, needScope, validateScope(false), sched.Pause),
		"resume":    action(s, needScope, validateScope(false), sched.Resume),
		"abort":     action(s, needScope, validateScope(true), sched.Abort),
		"skip":      action(s, needRecoverHosts, validateRolloutHosts(true), sched.Skip),
		"retry":     action(s, needRetry, validateRolloutHosts(false), sched.Retry),
	}
}

// needRoll: roll, or team_roll for the containers the roll touches. The
// flags force and ignoreBudget and the selector all need admin as well.
//
// Through the team twin alone a roll is always staggered: a roll "now" skips
// the order and the wait for readiness, which is what force does to a
// staggered one. A login with admin keeps the choice.
func needRoll(req *scheduler.RollRequest) requirement {
	return requirement{
		feature:   FeatureRoll,
		team:      containerTargets(req.Hosts, req.All, req.Containers),
		admin:     adminReason(req.Force, req.IgnoreBudget, req.All),
		teamAdmin: nowReason(req.Staggered),
		teamOnly:  func() { req.Staggered = true },
	}
}

// needReboot: reboot, or team_reboot for the named hosts. The flags force
// and ignoreBudget and the selector all need admin as well. Through the team
// twin alone a reboot is always staggered, like a roll.
func needReboot(req *scheduler.RebootRequest) requirement {
	return requirement{
		feature:   FeatureReboot,
		team:      hostTargets(req.Hosts, req.All),
		admin:     adminReason(req.Force, req.IgnoreBudget, req.All),
		teamAdmin: nowReason(req.Staggered),
		teamOnly:  func() { req.Staggered = true },
	}
}

// nowReason names a roll or reboot that is not staggered, for
// requirement.teamAdmin.
func nowReason(staggered bool) string {
	if staggered {
		return ""
	}

	return "now"
}

// needHold: hold, or team_hold for the named hosts. The twin does not take
// over a hold that was not set through it.
func needHold(req *scheduler.HoldRequest) requirement {
	return requirement{feature: FeatureHold, team: holdTargets(req.Hosts, holdSet)}
}

// needUnhold: hold, or team_hold for the named hosts. The twin lifts only a
// hold that was set through it.
func needUnhold(req *scheduler.HoldRequest) requirement {
	return requirement{feature: FeatureHold, team: holdTargets(req.Hosts, holdLift)}
}

// needOffline: hold, or team_hold for hosts the team owns a container of.
// The scheduler lets the twin change only a mark that was set through it.
func needOffline(req *scheduler.OfflineRequest) requirement {
	return requirement{feature: FeatureHold, team: holdTargets(req.Hosts, holdUntouched)}
}

// validateMarkOffline: hosts as for a hold, a reason, and an end, if any, in
// the future.
func (s *Server) validateMarkOffline(req *scheduler.OfflineRequest) error {
	if err := validateUnmarkOffline(req); err != nil {
		return err
	}

	if strings.TrimSpace(req.Reason) == "" {
		return badRequestf("reason is required")
	}

	if req.Until != nil && !req.Until.After(s.now()) {
		return badRequestf("until must be in the future")
	}

	return nil
}

// validateUnmarkOffline: hosts as for a hold.
func validateUnmarkOffline(req *scheduler.OfflineRequest) error {
	return validateHostsRequest(&scheduler.HoldRequest{Hosts: req.Hosts, Reason: req.Reason})
}

// needImageOverride: image_override, or team_image_override for that one
// container of the host.
func needImageOverride(req *scheduler.ImageOverrideRequest) requirement {
	return requirement{
		feature: FeatureImageOverride,
		team:    containerTargets([]string{req.Host}, false, []string{req.Container}),
	}
}

// needFlagOverride: flag_override, or team_flag_override for that one
// container of the host.
func needFlagOverride(req *scheduler.FlagOverrideRequest) requirement {
	return requirement{
		feature: FeatureFlagOverride,
		team:    containerTargets([]string{req.Host}, false, []string{req.Container}),
	}
}

// needClientPolicy: client_policy, or team_client_policy for a client whose
// reported containers are all owned by a team of the token.
func needClientPolicy(req *scheduler.ClientPolicyRequest) requirement {
	return requirement{feature: FeatureClientPolicy, team: &targets{client: req.Client}}
}

// needAdmin: admin and nothing else (forget).
func needAdmin[T any](*T) requirement {
	return requirement{feature: FeatureAdmin}
}

// needEndLease: recover, or team_recover for the host of the lease.
func needEndLease(req *scheduler.EndLeaseRequest) requirement {
	return requirement{feature: FeatureRecover, team: leaseTarget(req.Lease)}
}

// needScope: supervise for one rollout (pause, resume, abort); without a
// rollout the request is the global switch and needs admin.
func needScope(req *scheduler.ScopeRequest) requirement {
	if req.Rollout == "" {
		return requirement{feature: FeatureAdmin}
	}

	return requirement{feature: FeatureSupervise}
}

// needRecoverHosts: recover, or team_recover for the named hosts (skip).
func needRecoverHosts(req *scheduler.RolloutHostsRequest) requirement {
	return requirement{feature: FeatureRecover, team: hostTargets(req.Hosts, false)}
}

// needRetry: without hosts it reopens the whole rollout and needs supervise.
// With hosts it is the recovery of those hosts, like skip, unless the rollout
// is aborted: retrying an aborted rollout reopens its cause for every host,
// whatever hosts the request names, and needs supervise (authorize reads the
// rollout).
func needRetry(req *scheduler.RolloutHostsRequest) requirement {
	if len(req.Hosts) == 0 {
		return requirement{feature: FeatureSupervise}
	}

	need := needRecoverHosts(req)
	need.reopens = req.Rollout

	return need
}

// action builds the handler of one action: decode, authorize, validate, call,
// answer. The authorization comes before the validation, so a login that
// lacks the feature is told so whatever the body says, and before the
// scheduler, so a refused request changes nothing.
func action[T any](
	s *Server,
	need func(*T) requirement,
	validate func(*T) error,
	call func(ctx context.Context, actor string, req T) (*scheduler.ActionResult, error),
) userHandler {
	return func(w http.ResponseWriter, r *http.Request, u *user) {
		var req T
		if err := decodeBody(w, r, &req); err != nil {
			s.writeBodyError(w, err)

			return
		}

		required := need(&req)

		ctx, ok := s.allow(w, r, u, &required)
		if !ok {
			return
		}

		if err := validate(&req); err != nil {
			s.writeBadRequest(w, err)

			return
		}

		result, err := call(ctx, u.actor, req)
		if err != nil {
			s.writeSchedulerError(w, r, err)

			return
		}

		if result == nil {
			result = &scheduler.ActionResult{Message: "ok"}
		}

		s.Notify()
		s.writeJSON(w, http.StatusOK, result)
	}
}

// writeSchedulerError maps a scheduler error to its status: an unknown host,
// rollout or lease is 404, an action that cannot apply now is 409, one the
// request's authorization does not allow on the state the scheduler finds is
// 403. Those messages
// are written by the scheduler for people; anything else stays internal.
func (s *Server) writeSchedulerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, scheduler.ErrUnknownHost), errors.Is(err, scheduler.ErrUnknownRollout),
		errors.Is(err, scheduler.ErrUnknownLease):
		s.writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, scheduler.ErrInvalidAction):
		s.writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, scheduler.ErrForbidden):
		// The scheduler asked again, under its lock, what authorize asked
		// before: the answer has the same shape.
		msg := strings.TrimPrefix(err.Error(), scheduler.ErrForbidden.Error()+": ")
		s.log.WithField("reason", msg).Debug("Refused user request")
		s.writeError(w, http.StatusForbidden, "forbidden: "+s.opts.AuthService+":"+msg)
	default:
		s.internalError(w, r, err)
	}
}

func validateReason(reason string) error {
	if utf8.RuneCountInString(reason) > maxReasonLength {
		return badRequestf("reason is longer than %d characters", maxReasonLength)
	}

	return nil
}

// validateHosts checks a host list. Host names are certificate subjects, so
// they obey the identity name rules.
func validateHosts(hosts []string, required bool) error {
	if required && len(hosts) == 0 {
		return badRequestf("hosts must not be empty")
	}

	if len(hosts) > maxActionHosts {
		return badRequestf("more than %d hosts", maxActionHosts)
	}

	seen := make(map[string]struct{}, len(hosts))

	for _, host := range hosts {
		if err := identity.ValidateName(host); err != nil {
			return badRequestf("invalid host name %q", host)
		}

		if _, dup := seen[host]; dup {
			return badRequestf("host %q is listed twice", host)
		}

		seen[host] = struct{}{}
	}

	return nil
}

// validateSelection checks the "these hosts or all" pair of roll and reboot.
func validateSelection(hosts []string, all bool) error {
	switch {
	case all && len(hosts) > 0:
		return badRequestf("give hosts or all, not both")
	case !all && len(hosts) == 0:
		return badRequestf("give hosts, or set all")
	}

	return validateHosts(hosts, false)
}

func validateRoll(req *scheduler.RollRequest) error {
	if err := validateSelection(req.Hosts, req.All); err != nil {
		return err
	}

	if len(req.Containers) > maxActionContainers {
		return badRequestf("more than %d containers", maxActionContainers)
	}

	for _, name := range req.Containers {
		if name == "" || len(name) > maxNameLength {
			return badRequestf("invalid container name %q", name)
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
		return badRequestf("invalid container name %q", container)
	}

	return validateReason(reason)
}

func validateImageOverride(req *scheduler.ImageOverrideRequest) error {
	if len(req.Image) > maxOverrideImage || strings.ContainsAny(req.Image, " \t\r\n") {
		return badRequestf("image must be an image reference")
	}

	return validateOverrideTarget(req.Host, req.Container, req.Reason)
}

func validateFlagOverride(req *scheduler.FlagOverrideRequest) error {
	if len(req.AddFlags)+len(req.RemoveFlags) > maxOverrideFlags {
		return badRequestf("more than %d flags", maxOverrideFlags)
	}

	for _, flag := range slices.Concat(req.AddFlags, req.RemoveFlags) {
		if flag == "" || len(flag) > maxOverrideFlagLen || strings.TrimSpace(flag) != flag ||
			strings.ContainsAny(flag, "\x00\r\n") || strings.Count(flag, " ") > 1 {
			return badRequestf("invalid flag %q", flag)
		}
	}

	return validateOverrideTarget(req.Host, req.Container, req.Reason)
}

func validateClientPolicy(req *scheduler.ClientPolicyRequest) error {
	if !protocol.ValidClient(req.Client) {
		return badRequestf("invalid client name %q", req.Client)
	}

	switch req.Policy {
	case "", fleet.PolicyAuto, fleet.PolicyManual:
	default:
		return badRequestf("policy must be auto, manual or empty (the default)")
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
	if err := validateHosts(req.Hosts, true); err != nil {
		return err
	}

	return validateReason(req.Reason)
}

// validateEndLease checks the end-lease action: one lease, by its ID.
func validateEndLease(req *scheduler.EndLeaseRequest) error {
	if req.Lease == "" {
		return badRequestf("lease must not be empty")
	}

	if len(req.Lease) > maxNameLength {
		return badRequestf("invalid lease id")
	}

	return validateReason(req.Reason)
}

func validateRolloutID(id string, required bool) error {
	if required && id == "" {
		return badRequestf("rollout must not be empty")
	}

	if len(id) > maxNameLength {
		return badRequestf("invalid rollout id")
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
