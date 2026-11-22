package userapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// authorizedByAction is the authorization the audit log records for a host
// action; the basis per host is in the entry's detail.
const authorizedByAction = "action"

// handleHostAction serves POST /api/v1/actions/host-action: one configured
// host action on named hosts, the hosts of a client or all hosts. A person
// may start it on a host with admin, with the feature the host's agent.yaml
// names for it (manage when it names none), or as a member of one of its
// teams (authz.ActionBasis). A
// selection by client or all takes the permitted hosts that report the
// action and leaves the others out; a named host that is not permitted is
// rejected with the reason. The request is refused when no host is
// permitted, before its body is validated.
func (s *Handler) handleHostAction(w http.ResponseWriter, r *http.Request, u *authz.User) {
	var req scheduler.HostActionRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.writeBodyError(w, err)

		return
	}

	// Checked on the request as sent; answered after the permission.
	invalid := validateHostAction(&req)

	hosts, err := s.opts.Store.ListHosts(r.Context())
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	refused := s.selectActionHosts(u, &req, hosts)

	if len(req.Basis) == 0 && !u.Open && !s.authz.HasFeature(u, authz.FeatureAdmin) {
		s.authz.Refuse(w, r, u, fmt.Sprintf(
			"forbidden: this login lacks the feature %s:%s, and the feature and the teams of the action %s "+
				"on every selected host", s.opts.AuthService, authz.FeatureAdmin, authz.Echo(req.Action)))

		return
	}

	if invalid != nil {
		httpjson.BadRequest(s.log, w, invalid)

		return
	}

	ctx := scheduler.WithAuthorization(r.Context(), scheduler.Authorization{By: authorizedByAction, Teams: u.Teams})

	result, err := s.opts.Scheduler.HostAction(ctx, u.Actor, req)
	if err != nil {
		s.writeSchedulerError(w, r, u, err)

		return
	}

	if result.Rejected == nil {
		result.Rejected = make(map[string]string, len(refused))
	}

	for host, reason := range refused {
		result.Rejected[host] = reason
	}

	s.opts.Notify()
	httpjson.Write(s.log, w, http.StatusOK, result)
}

// selectActionHosts resolves the selection of req to req.Hosts and the
// basis of each permitted host in req.Basis. It returns the named hosts the
// login may not start the action on, with the reason.
func (s *Handler) selectActionHosts(
	u *authz.User, req *scheduler.HostActionRequest, hosts []*fleet.Host,
) map[string]string {
	named := len(req.Hosts) > 0
	byName := make(map[string]*fleet.Host, len(hosts))

	for _, h := range hosts {
		byName[h.Name] = h
	}

	if !named {
		req.Hosts = make([]string, 0, len(hosts))

		for _, h := range hosts {
			if req.All || hostRunsClient(h, req.Client) {
				req.Hosts = append(req.Hosts, h.Name)
			}
		}
	}

	req.Basis = make(map[string]string, len(req.Hosts))
	refused := make(map[string]string, 1)
	kept := make([]string, 0, len(req.Hosts))

	for _, name := range req.Hosts {
		h := byName[name]
		info := scheduler.HostActionOf(h, req.Action)

		switch {
		case h == nil:
			// The scheduler names the unknown host.
			kept = append(kept, name)
		case info == nil && !named:
		case info == nil:
			kept = append(kept, name)
			req.Basis[name] = s.authz.ActionBasis(u, &protocol.ActionInfo{})
		default:
			basis := s.authz.ActionBasis(u, info)
			if basis != "" {
				kept = append(kept, name)
				req.Basis[name] = basis
			} else if named {
				refused[name] = "this login may not start the action " + authz.Echo(req.Action) + " on this host"
			}
		}
	}

	req.Hosts = kept

	for name, basis := range req.Basis {
		if basis == "" {
			delete(req.Basis, name)
		}
	}

	return refused
}

// hostRunsClient reports whether the host reports a managed container of
// the client.
func hostRunsClient(h *fleet.Host, client string) bool {
	if h.Status == nil || client == "" {
		return false
	}

	return slices.ContainsFunc(h.Status.Containers, func(c protocol.Container) bool {
		return c.Managed && c.Team == client
	})
}

// validateHostAction: an action name, one selection (hosts, client or all)
// and a reason within bounds.
func validateHostAction(req *scheduler.HostActionRequest) error {
	if !protocol.ValidActionName(req.Action) {
		return httpjson.BadRequestf("invalid action name %q", authz.Echo(req.Action))
	}

	selections := 0

	for _, set := range []bool{len(req.Hosts) > 0, req.Client != "", req.All} {
		if set {
			selections++
		}
	}

	if selections != 1 {
		return httpjson.BadRequestf("give exactly one of hosts, client and all")
	}

	if req.Client != "" && !protocol.ValidClient(req.Client) {
		return httpjson.BadRequestf("invalid client name %q", authz.Echo(req.Client))
	}

	if err := validateHosts(req.Hosts, false); err != nil {
		return err
	}

	return validateReason(req.Reason)
}

// handleResume serves the resume action. A rollout of a host action that
// waits for approval may also be approved by a login that may start the
// action on every member host; everything else needs what needScope says.
func (s *Handler) handleResume(w http.ResponseWriter, r *http.Request, u *authz.User) {
	var req scheduler.ScopeRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.writeBodyError(w, err)

		return
	}

	ctx, ok := s.allowApproval(w, r, u, req.Rollout)
	if !ok {
		return
	}

	if err := validateScope(false)(&req); err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	result, err := s.opts.Scheduler.Resume(ctx, u.Actor, req)
	if err != nil {
		s.writeSchedulerError(w, r, u, err)

		return
	}

	s.opts.Notify()
	httpjson.Write(s.log, w, http.StatusOK, result)
}

// allowActionApproval reports whether the login may resume the rollout
// through the host action it runs: the rollout is one of a host action and
// the login may start that action on each of its members.
func (s *Handler) allowActionApproval(ctx context.Context, u *authz.User, id string) (context.Context, bool) {
	if id == "" || u.Open || s.authz.HasFeature(u, authz.FeatureManage) {
		return nil, false
	}

	ro, err := s.opts.Store.GetRollout(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.WithError(err).Warn("Could not read the rollout of a resume")
		}

		return nil, false
	}

	if ro.Kind != fleet.RolloutAction || len(ro.Hosts) == 0 {
		return nil, false
	}

	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return nil, false
	}

	byName := make(map[string]*fleet.Host, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
	}

	by := ""

	for i := range ro.Hosts {
		info := scheduler.HostActionOf(byName[ro.Hosts[i].Host], ro.Action)
		if info == nil {
			return nil, false
		}

		basis := s.authz.ActionBasis(u, info)
		if basis == "" {
			return nil, false
		}

		by = basis
	}

	return scheduler.WithAuthorization(ctx, scheduler.Authorization{
		By: authorizedByAction + ":" + by, Teams: u.Teams,
	}), true
}
