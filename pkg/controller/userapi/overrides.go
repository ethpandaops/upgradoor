package userapi

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// handleClientOverride serves POST /api/v1/actions/client-override: one part
// of an override (image or flags) for the container of one kind of a client,
// on many hosts at once. It is the single-host override for each host: the
// same feature (image_override or flag_override), the same team twin, judged
// per host, and the same checks in the scheduler. A host the login may not
// act on is listed in the answer's Rejected with the reason, like a host the
// scheduler refuses; the request is refused as a whole only when the login
// may act on none of them.
func (s *Handler) handleClientOverride(w http.ResponseWriter, r *http.Request, u *authz.User) {
	var req scheduler.ClientOverrideRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.writeBodyError(w, err)

		return
	}

	feature := authz.FeatureImageOverride
	if req.Part == protocol.OverrideFlags {
		feature = authz.FeatureFlagOverride
	}

	ctx, refused, ok := s.allowEach(w, r, u, feature, &req)
	if !ok {
		return
	}

	if err := validateClientOverride(&req); err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	asked := len(req.Hosts)
	req.Hosts = slices.DeleteFunc(req.Hosts, func(h string) bool {
		_, no := refused[h]

		return no
	})

	result := &scheduler.ActionResult{Rejected: map[string]string{}}

	if len(req.Hosts) > 0 {
		var err error

		if result, err = s.opts.Scheduler.ClientOverride(ctx, u.Actor, req); err != nil {
			s.writeSchedulerError(w, r, err)

			return
		}

		if result.Rejected == nil {
			result.Rejected = make(map[string]string, len(refused))
		}
	}

	maps.Copy(result.Rejected, refused)

	if len(refused) > 0 {
		result.Message = fmt.Sprintf(
			"sent to %d of %d hosts: each agent decides, and its container follows under the host's next lease",
			len(result.Hosts), asked)
	}

	s.opts.Notify()
	httpjson.Write(s.log, w, http.StatusOK, result)
}

// allowEach authorizes a client override host by host. With the plain
// feature (or open, or without the team twin either) it is the one decision
// of allow. With only the team twin each host is judged alone, by the rule
// of the single-host override (its container of the client and kind owned by
// a team of the token): the hosts it does not cover come back in refused,
// and when it covers none the request is refused as a whole.
func (s *Handler) allowEach(
	w http.ResponseWriter, r *http.Request, u *authz.User, feature string, req *scheduler.ClientOverrideRequest,
) (context.Context, map[string]string, bool) {
	twin := authz.TeamFeaturePrefix + feature

	if u.Open || s.authz.HasFeature(u, feature) || !s.authz.HasFeature(u, twin) {
		ctx, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: feature, Team: authz.HostTargets(req.Hosts, false)})

		return ctx, nil, ok
	}

	all, err := s.opts.Store.ListHosts(r.Context())
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return nil, nil, false
	}

	byName := make(map[string]*fleet.Host, len(all))
	for _, h := range all {
		byName[h.Name] = h
	}

	known := authz.OwnershipOf(all)
	refused := make(map[string]string)
	first := ""

	for _, name := range req.Hosts {
		h := byName[name]
		if h == nil {
			// The scheduler names what is wrong with it.
			continue
		}

		container := scheduler.ClientContainer(h, req.Client, req.Kind)
		if container == "" {
			continue
		}

		t := authz.ContainerTargets([]string{name}, false, []string{container})
		if reason := authz.UncoveredOn(u.Teams, name, known[name], t); reason != "" {
			refused[name] = s.opts.AuthService + ":" + twin + " " + reason
			first = cmp.Or(first, refused[name])
		}
	}

	if len(req.Hosts) == 0 || len(refused) == len(req.Hosts) {
		msg := "forbidden: " + cmp.Or(first, s.opts.AuthService+":"+twin+
			" needs a target that one of your teams owns, and the request names none")

		s.authz.Refuse(w, r, u, msg)

		return nil, nil, false
	}

	return scheduler.WithAuthorization(r.Context(), scheduler.Authorization{By: twin, Teams: u.Teams}), refused, true
}

// validateClientOverride checks a client override like the single-host one.
func validateClientOverride(req *scheduler.ClientOverrideRequest) error {
	if req.Client == "" || len(req.Client) > maxNameLength {
		return httpjson.BadRequestf("invalid client name")
	}

	if !slices.Contains([]protocol.ContainerKind{protocol.KindEL, protocol.KindCL, protocol.KindVC}, req.Kind) {
		return httpjson.BadRequestf("kind must be el, cl or vc")
	}

	if err := validateHosts(req.Hosts, true); err != nil {
		return err
	}

	switch req.Part {
	case protocol.OverrideImage:
		if len(req.AddFlags)+len(req.RemoveFlags) > 0 {
			return httpjson.BadRequestf("an image override carries no flags")
		}

		return validateImageOverride(&scheduler.ImageOverrideRequest{
			Host: req.Hosts[0], Container: string(req.Kind), Image: req.Image, Reason: req.Reason,
		})
	case protocol.OverrideFlags:
		if req.Image != "" {
			return httpjson.BadRequestf("a flag override carries no image")
		}

		return validateFlagOverride(&scheduler.FlagOverrideRequest{
			Host: req.Hosts[0], Container: string(req.Kind), AddFlags: req.AddFlags,
			RemoveFlags: req.RemoveFlags, Reason: req.Reason,
		})
	default:
		return httpjson.BadRequestf("part must be image or flags")
	}
}
