package scheduler

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// HostActionRequest is the body of POST /api/v1/actions/host-action: one
// configured host action on a selection of hosts, as one staggered rollout.
// Hosts, Client (the hosts that run a managed container of that client) and
// All select the hosts; the API resolves them to the hosts the person may
// start the action on.
type HostActionRequest struct {
	Action string   `json:"action"`
	Hosts  []string `json:"hosts"`
	Client string   `json:"client"`
	All    bool     `json:"all"`
	// PauseAfterCanary pauses the rollout at its first success.
	PauseAfterCanary bool   `json:"pauseAfterCanary"`
	Reason           string `json:"reason"`
	// Basis is what permits the person on each selected host (admin,
	// feature, team or open), set by the API; a host without one is refused.
	Basis map[string]string `json:"-"`
}

// HostActionOf returns the action of that name as the host last reported
// it, or nil.
func HostActionOf(h *fleet.Host, name string) *protocol.ActionInfo {
	if h == nil || h.Status == nil {
		return nil
	}

	for i := range h.Status.Actions {
		if h.Status.Actions[i].Name == name {
			return &h.Status.Actions[i]
		}
	}

	return nil
}

// HostAction starts a host action on the permitted hosts as one staggered
// rollout of the action's impact: one directive per host, which its agent
// turns into a need of its own configuration. Hosts that cannot take it are
// rejected with the reason.
func (s *service) HostAction(ctx context.Context, actor string, req HostActionRequest) (*ActionResult, error) { //nolint:gocritic // hugeParam: the signature is the Service contract.
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.selectHosts(ctx, req.Hosts, false)
	if err != nil {
		return nil, err
	}

	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}

	now := s.opts.Now()
	res := &ActionResult{Rejected: make(map[string]string, len(hosts))}
	r := &fleet.Rollout{
		ID:               s.opts.NewID(),
		Kind:             fleet.RolloutAction,
		Action:           req.Action,
		State:            fleet.RolloutActive,
		Approved:         true,
		PauseAfterCanary: req.PauseAfterCanary,
		CreatedBy:        actorName(actor),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	r.Key = prefixManual + r.ID
	changes := &store.Changes{Rollouts: []*fleet.Rollout{r}}

	by := &protocol.Actor{User: r.CreatedBy}
	if a, ok := AuthorizationFrom(ctx); ok {
		by.Teams, by.Feature = slices.Clone(a.Teams), a.By
	}

	basis := make(map[string]string, len(hosts))

	for _, h := range hosts {
		if reason := hostActionRefusal(h, &req, now, s.since, &settings); reason != "" {
			res.Rejected[h.Name] = reason

			continue
		}

		basis[h.Name] = req.Basis[h.Name]
		res.Hosts = append(res.Hosts, h.Name)
		r.Hosts = append(r.Hosts, fleet.RolloutHost{
			Host: h.Name, State: fleet.HostExpected, Stage: fleet.StageRest, UpdatedAt: now,
		})

		hostBy := *by
		hostBy.Feature = "action:" + req.Basis[h.Name]

		changes.Directives = append(changes.Directives, &fleet.Directive{
			Directive: protocol.Directive{
				ID: s.opts.NewID(), Kind: protocol.DirectiveAction, Action: req.Action,
				Reason: req.Reason, By: &hostBy,
			},
			Host: h.Name, State: fleet.DirectivePending, RolloutID: r.ID,
			CreatedBy: r.CreatedBy, CreatedAt: now, UpdatedAt: now,
		})
	}

	if len(res.Hosts) == 0 {
		res.Message = "no host can take the action " + req.Action

		return res, fmt.Errorf("%w: %s", ErrInvalidAction, res.Message)
	}

	r.Title = fmt.Sprintf("action %s on %d host(s)", req.Action, len(res.Hosts))
	if req.Reason != "" {
		r.Title += ": " + req.Reason
	}

	changes.Audit = []fleet.AuditEntry{s.entry(actor, "action.host-action", r.ID, map[string]any{
		"action": req.Action, keyHosts: res.Hosts, "rejected": res.Rejected, "basis": basis,
		"pauseAfterCanary": req.PauseAfterCanary, keyReason: req.Reason,
	})}

	if err := s.commit(ctx, changes); err != nil {
		return nil, err
	}

	res.Rollout = r.ID
	res.Message = r.Title

	return res, nil
}

// hostActionRefusal says why a host cannot take a host action; empty when
// it can.
func hostActionRefusal(
	h *fleet.Host, req *HostActionRequest, now, since time.Time, settings *fleet.Settings,
) string {
	switch {
	case req.Basis[h.Name] == "":
		return "not permitted"
	case h.Status == nil:
		return "no sync yet"
	case HostActionOf(h, req.Action) == nil:
		return "the host has no action " + req.Action
	case h.KnownOffline(now):
		return reasonOffline
	case h.Held():
		return reasonHeld
	case hostStale(h, now, since, settings.HostStaleAfter.Std()):
		return reasonAgentSilent
	default:
		return ""
	}
}
