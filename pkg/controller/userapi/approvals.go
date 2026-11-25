package userapi

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
)

// allowApproval authorizes a person's decision on one rollout (resume, which
// approves, ignore-gate, ignore-budget, approve-lease): the host action on every member of
// an action rollout (allowActionApproval), or what needScope says.
func (s *Handler) allowApproval(
	w http.ResponseWriter, r *http.Request, u *authz.User, rollout string,
) (context.Context, bool) {
	if ctx, ok := s.allowActionApproval(r.Context(), u, rollout); ok {
		return ctx, true
	}

	required := needScope(&scheduler.ScopeRequest{Rollout: rollout})

	return s.authz.Allow(w, r, u, &required)
}

// handleIgnoreGate serves POST /api/v1/actions/ignore-gate: the leases of one
// rollout pass a closed network gate from now on.
func (s *Handler) handleIgnoreGate(w http.ResponseWriter, r *http.Request, u *authz.User) {
	s.handleRolloutDecision(w, r, u, s.opts.Scheduler.IgnoreGate)
}

// handleIgnoreBudget serves POST /api/v1/actions/ignore-budget: the leases of
// one rollout pass the budget and a closed network gate from now on.
func (s *Handler) handleIgnoreBudget(w http.ResponseWriter, r *http.Request, u *authz.User) {
	s.handleRolloutDecision(w, r, u, s.opts.Scheduler.IgnoreBudget)
}

// handleRolloutDecision serves a person's decision on one rollout that takes
// a ScopeRequest.
func (s *Handler) handleRolloutDecision(
	w http.ResponseWriter, r *http.Request, u *authz.User,
	decide func(context.Context, string, scheduler.ScopeRequest) (*scheduler.ActionResult, error),
) {
	var req scheduler.ScopeRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.writeBodyError(w, err)

		return
	}

	ctx, ok := s.allowApproval(w, r, u, req.Rollout)
	if !ok {
		return
	}

	if err := validateScope(true)(&req); err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	s.answer(w, r, u)(decide(ctx, u.Actor, req))
}

// handleApproveLease serves POST /api/v1/actions/approve-lease: the
// verifying lease of one member is released as ready now. The lease may
// serve other rollouts of the host as well: the login must be allowed to
// approve each of them.
func (s *Handler) handleApproveLease(w http.ResponseWriter, r *http.Request, u *authz.User) {
	var req scheduler.ApproveLeaseRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.writeBodyError(w, err)

		return
	}

	ctx, ok := s.allowApproval(w, r, u, req.Rollout)
	if !ok {
		return
	}

	if err := validateApproveLease(&req); err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	others, err := s.leaseRollouts(r.Context(), &req)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	for _, id := range others {
		if _, ok := s.allowApproval(w, r, u, id); !ok {
			return
		}
	}

	s.answer(w, r, u)(s.opts.Scheduler.ApproveLease(ctx, u.Actor, req))
}

// leaseRollouts returns the other rollouts that the lease of the member
// serves. A rollout, member or lease that is not there yields none: the
// scheduler answers for it.
func (s *Handler) leaseRollouts(ctx context.Context, req *scheduler.ApproveLeaseRequest) ([]string, error) {
	ro, err := s.opts.Store.GetRollout(ctx, req.Rollout)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}

	m := ro.Host(req.Host)
	if m == nil || m.LeaseID == "" {
		return nil, nil
	}

	lease, err := s.opts.Store.GetLease(ctx, m.LeaseID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}

	others := make([]string, 0, len(lease.Rollouts))

	for _, id := range lease.Rollouts {
		if id != req.Rollout && !slices.Contains(others, id) {
			others = append(others, id)
		}
	}

	slices.Sort(others)

	return others, nil
}

// validateApproveLease: a rollout, one host and a reason within bounds.
func validateApproveLease(req *scheduler.ApproveLeaseRequest) error {
	if err := validateRolloutID(req.Rollout, true); err != nil {
		return err
	}

	if err := validateHosts([]string{req.Host}, true); err != nil {
		return err
	}

	return validateReason(req.Reason)
}

// answer writes the scheduler's result of an action, or its error.
func (s *Handler) answer(
	w http.ResponseWriter, r *http.Request, u *authz.User,
) func(*scheduler.ActionResult, error) {
	return func(result *scheduler.ActionResult, err error) {
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
