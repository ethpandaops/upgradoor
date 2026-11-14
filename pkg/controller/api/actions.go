package api

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
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
func (s *Server) actions() map[string]userHandler {
	sched := s.opts.Scheduler

	return map[string]userHandler{
		"roll":   action(s, validateRoll, sched.Roll),
		"reboot": action(s, validateReboot, sched.Reboot),
		"hold":   action(s, validateHostsRequest, sched.Hold),
		"unhold": action(s, validateHostsRequest, sched.Unhold),
		"forget": action(s, validateHostsRequest, sched.Forget),
		"pause":  action(s, validateScope(false), sched.Pause),
		"resume": action(s, validateScope(false), sched.Resume),
		"abort":  action(s, validateScope(true), sched.Abort),
		"skip":   action(s, validateRolloutHosts(true), sched.Skip),
		"retry":  action(s, validateRolloutHosts(false), sched.Retry),
	}
}

// action builds the handler of one action: decode, validate, call, answer.
func action[T any](
	s *Server,
	validate func(*T) error,
	call func(ctx context.Context, actor string, req T) (*scheduler.ActionResult, error),
) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor string) {
		var req T
		if err := decodeBody(w, r, &req); err != nil {
			s.writeBodyError(w, err)

			return
		}

		if err := validate(&req); err != nil {
			s.writeBadRequest(w, err)

			return
		}

		result, err := call(r.Context(), actor, req)
		if err != nil {
			s.writeSchedulerError(w, r, err)

			return
		}

		if result == nil {
			result = &scheduler.ActionResult{Message: "ok"}
		}

		s.writeJSON(w, http.StatusOK, result)
	}
}

// writeSchedulerError maps a scheduler error to its status: an unknown host
// or rollout is 404, an action that cannot apply now is 409. Those messages
// are written by the scheduler for people; anything else stays internal.
func (s *Server) writeSchedulerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, scheduler.ErrUnknownHost), errors.Is(err, scheduler.ErrUnknownRollout):
		s.writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, scheduler.ErrInvalidAction):
		s.writeError(w, http.StatusConflict, err.Error())
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
