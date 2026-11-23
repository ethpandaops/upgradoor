package scheduler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Audit detail keys.
const (
	keyLease    = "lease"
	keyReason   = "reason"
	keyHost     = "host"
	keyHosts    = "hosts"
	keyBefore   = "before"
	keyAfter    = "after"
	keyForce    = "force"
	keyPaused   = "paused"
	keyState    = "state"
	keyKey      = "key"
	keyTitle    = "title"
	keyRollouts = "rollouts"
	// keyIgnoreGate is Rollout.IgnoreGate.
	keyIgnoreGate = "ignoreGate"
	// keyHolder names whose hold an entry of hold or unhold is about.
	keyHolder    = "holder"
	keyDirective = "directive"
	keyBy        = "by"
	keyHeld      = "held"
)

// manualRequest is a roll or reboot request in one shape.
type manualRequest struct {
	verb         protocol.Verb
	hosts        []string
	all          bool
	containers   []string
	staggered    bool
	force        bool
	ignoreBudget bool
	ignoreGate   bool
	reason       string
}

// Roll requests a manual image roll.
func (s *service) Roll(ctx context.Context, actor string, req RollRequest) (*ActionResult, error) { //nolint:gocritic // hugeParam: the signature is the Service contract.
	return s.manual(ctx, actor, &manualRequest{
		verb: protocol.VerbRoll, hosts: req.Hosts, all: req.All, containers: req.Containers,
		staggered: req.Staggered, force: req.Force, ignoreBudget: req.IgnoreBudget, ignoreGate: req.IgnoreGate,
		reason: req.Reason,
	})
}

// Reboot requests a manual reboot.
func (s *service) Reboot(ctx context.Context, actor string, req RebootRequest) (*ActionResult, error) {
	return s.manual(ctx, actor, &manualRequest{
		verb: protocol.VerbReboot, hosts: req.Hosts, all: req.All,
		staggered: req.Staggered, force: req.Force, ignoreBudget: req.IgnoreBudget, ignoreGate: req.IgnoreGate,
		reason: req.Reason,
	})
}

// manual creates a manual rollout with one directive per host (reboot) or
// per container (roll). Hosts that cannot take it are rejected with a reason.
func (s *service) manual(ctx context.Context, actor string, req *manualRequest) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.selectHosts(ctx, req.hosts, req.all)
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
		ID:           s.opts.NewID(),
		Kind:         fleet.RolloutManual,
		State:        fleet.RolloutActive,
		Approved:     true,
		Force:        !req.staggered || req.force,
		IgnoreBudget: req.ignoreBudget,
		IgnoreGate:   req.ignoreGate,
		CreatedBy:    actorName(actor),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	r.Key = prefixManual + r.ID
	changes := &store.Changes{Rollouts: []*fleet.Rollout{r}}

	// Who asked travels with the directives, for the host's own audit log.
	by := &protocol.Actor{User: r.CreatedBy}
	if a, ok := AuthorizationFrom(ctx); ok {
		by.Teams, by.Feature = slices.Clone(a.Teams), a.By
	}

	for _, h := range hosts {
		containers, reason := manualTargets(h, req, now, s.since, &settings)
		if reason != "" {
			res.Rejected[h.Name] = reason

			continue
		}

		res.Hosts = append(res.Hosts, h.Name)
		r.Hosts = append(r.Hosts, fleet.RolloutHost{
			Host: h.Name, State: fleet.HostExpected, Stage: fleet.StageRest, UpdatedAt: now,
		})

		for _, c := range containers {
			kind := protocol.DirectiveReboot
			if req.verb == protocol.VerbRoll {
				kind = protocol.DirectiveRoll
			}

			changes.Directives = append(changes.Directives, &fleet.Directive{
				Directive: protocol.Directive{
					ID: s.opts.NewID(), Kind: kind, Container: c, Reason: req.reason, By: by,
				},
				Host: h.Name, State: fleet.DirectivePending, RolloutID: r.ID,
				CreatedBy: r.CreatedBy, CreatedAt: now, UpdatedAt: now,
			})
		}
	}

	if req.verb == protocol.VerbRoll {
		r.ContainerKind, r.Team = manualUnit(hosts, changes.Directives)
	}

	if len(res.Hosts) == 0 {
		res.Message = "no host can take the " + string(req.verb)

		return res, fmt.Errorf("%w: %s", ErrInvalidAction, res.Message)
	}

	mode := "now"
	if req.staggered {
		mode = "staggered"
	}

	r.Title = fmt.Sprintf("manual %s of %d host(s), %s", req.verb, len(res.Hosts), mode)
	if req.reason != "" {
		r.Title += ": " + req.reason
	}

	changes.Audit = []fleet.AuditEntry{s.entry(actor, "action."+string(req.verb), r.ID, map[string]any{
		keyHosts: res.Hosts, "rejected": res.Rejected, "containers": req.containers,
		"staggered": req.staggered, keyForce: r.Force, "ignoreBudget": r.IgnoreBudget,
		keyIgnoreGate: r.IgnoreGate, keyReason: req.reason,
	})}

	if err := s.commit(ctx, changes); err != nil {
		return nil, err
	}

	res.Rollout = r.ID
	res.Message = r.Title

	return res, nil
}

// manualUnit is the team of a manual roll when every container it rolls is
// of that one team, and their kind when they share one; empty otherwise.
func manualUnit(hosts []*fleet.Host, directives []*fleet.Directive) (kind protocol.ContainerKind, team string) {
	byName := make(map[string]*fleet.Host, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
	}

	for i, d := range directives {
		h := byName[d.Host]
		idx := slices.IndexFunc(h.Status.Containers, func(c protocol.Container) bool { return c.Name == d.Container })
		c := &h.Status.Containers[idx]

		switch {
		case i == 0:
			kind, team = c.Kind, c.Team
		case c.Team != team:
			return "", ""
		case c.Kind != kind:
			kind = ""
		}
	}

	if team == "" {
		return "", ""
	}

	return kind, team
}

// manualTargets returns what a manual action applies to on one host: the
// containers of a roll, or one empty entry for a reboot. A non-empty reason
// rejects the host.
func manualTargets(
	h *fleet.Host, req *manualRequest, now, since time.Time, settings *fleet.Settings,
) (targets []string, reason string) {
	switch {
	case h.Status == nil:
		return nil, "no sync yet"
	case h.KnownOffline(now):
		return nil, reasonOffline
	case h.Held():
		return nil, h.HeldReason()
	case h.Incompatible() != "":
		return nil, h.Incompatible()
	case hostStale(h, now, since, settings.HostStaleAfter.Std()):
		return nil, reasonAgentSilent
	case !slices.Contains(h.Status.Verbs, req.verb):
		return nil, fmt.Sprintf("verb %s not allowed by the agent", req.verb)
	case req.verb != protocol.VerbRoll:
		return []string{""}, ""
	}

	for i := range h.Status.Containers {
		c := &h.Status.Containers[i]
		if !c.Managed {
			continue
		}

		if len(req.containers) == 0 || slices.Contains(req.containers, c.Name) {
			targets = append(targets, c.Name)
		}
	}

	if len(targets) == 0 {
		return nil, "no managed container to roll"
	}

	return targets, ""
}

// selectHosts resolves a host selection. Names must exist.
func (s *service) selectHosts(ctx context.Context, names []string, all bool) ([]*fleet.Host, error) {
	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list hosts: %w", err)
	}

	if len(names) == 0 {
		if !all {
			return nil, fmt.Errorf("%w: no hosts named", ErrInvalidAction)
		}

		return hosts, nil
	}

	byName := make(map[string]*fleet.Host, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
	}

	out := make([]*fleet.Host, 0, len(names))

	for _, name := range names {
		h := byName[name]
		if h == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnknownHost, name)
		}

		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}

	return out, nil
}

// commit writes an action's changes and asks for a reconcile.
func (s *service) commit(ctx context.Context, changes *store.Changes) error {
	if err := s.write(ctx, changes); err != nil {
		return err
	}

	s.Nudge()

	return nil
}

// Hold asks the agents to set holds: staff's, or through team_manage one per
// team of the request that owns a managed container of the host. The agent
// owns its holds; a hold directive is pending until it answers.
func (s *service) Hold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error) {
	return s.setHold(ctx, actor, &req, true)
}

// Unhold asks the agents to lift holds: staff's unless req.Holders names
// others, or through team_manage those of the request's teams (all that own a
// managed container of the host unless req.Holders names some of them).
func (s *service) Unhold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error) {
	return s.setHold(ctx, actor, &req, false)
}

func (s *service) setHold(
	ctx context.Context, actor string, req *HoldRequest, wanted bool,
) (*ActionResult, error) {
	if wanted && len(req.Holders) > 0 {
		return nil, fmt.Errorf("%w: a hold is set for staff or the teams of the login, not for named holders",
			ErrInvalidAction)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.selectHosts(ctx, req.Hosts, false)
	if err != nil {
		return nil, err
	}

	pending, err := s.opts.Store.ListDirectives(ctx, store.DirectiveFilter{PendingOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list directives: %w", err)
	}

	// The holders are decided for every host before anything changes: one
	// refused host refuses the request. The API asked the same before; this
	// asks again under the lock, against the hosts as they are now.
	holders := make([][]string, len(hosts))

	for i, h := range hosts {
		holders[i], err = holdHolders(ctx, h, req.Holders, wanted)
		if err != nil {
			return nil, err
		}
	}

	action := "action.unhold"
	if wanted {
		action = "action.hold"
	}

	res := &ActionResult{}
	unchanged := 0

	// Each host is one transaction: when one fails, the earlier ones stay
	// changed and audited, and the result names them.
	for i, h := range hosts {
		changes := s.holdChanges(ctx, actor, action, h, holders[i], wanted, req.Reason, pending)
		if changes.Empty() {
			unchanged++

			continue
		}

		if err := s.write(ctx, changes); err != nil {
			res.Message = fmt.Sprintf("stopped at %s; changed before it: %d host(s)", h.Name, len(res.Hosts))

			if len(res.Hosts) > 0 {
				s.Nudge()
			}

			return res, err
		}

		res.Hosts = append(res.Hosts, h.Name)
	}

	res.Message = fmt.Sprintf("release requested on %d host(s), waiting for the hosts", len(res.Hosts))
	if wanted {
		res.Message = fmt.Sprintf("hold requested on %d host(s), waiting for the hosts", len(res.Hosts))
	}

	if !wanted && unchanged > 0 {
		res.Message += fmt.Sprintf("; %d host(s) had no such hold", unchanged)
	}

	if len(res.Hosts) > 0 {
		s.Nudge()
	}

	return res, nil
}

// holdHolders are the holders whose holds a hold or unhold changes on one
// host. Through team_manage they are the request's teams that own a managed
// container of the host, or those of them that named is limited to;
// otherwise staff, or for an unhold the holders named.
func holdHolders(ctx context.Context, h *fleet.Host, named []string, wanted bool) ([]string, error) {
	twin, viaTeam := teamFeatureOf(ctx)
	if !viaTeam {
		if len(named) > 0 {
			return named, nil
		}

		return []string{fleet.HolderStaff}, nil
	}

	a, _ := AuthorizationFrom(ctx)
	own := holdTeams(h, a.Teams)

	if len(own) == 0 {
		return nil, fmt.Errorf("%w: %s does not cover host %s (owners: none of your teams)", ErrForbidden, twin, h.Name)
	}

	if wanted || len(named) == 0 {
		return own, nil
	}

	for _, holder := range named {
		if !slices.Contains(own, holder) {
			return nil, fmt.Errorf("%w: %s does not lift the hold of %s on host %s", ErrForbidden, twin, holder, h.Name)
		}
	}

	return named, nil
}

// holdChanges adds a hold or release-hold directive per holder of one host,
// with an audit entry each, and cancels the unanswered hold directives of
// those holders it replaces. Lifting a hold that is neither reported nor
// requested changes nothing.
func (s *service) holdChanges(
	ctx context.Context, actor, action string, h *fleet.Host, holders []string, wanted bool, reason string,
	pending []*fleet.Directive,
) *store.Changes {
	changes := &store.Changes{}
	now := s.opts.Now()

	by := &protocol.Actor{User: actorName(actor)}
	if a, ok := AuthorizationFrom(ctx); ok {
		by.Teams, by.Feature = slices.Clone(a.Teams), a.By
	}

	kind := protocol.DirectiveReleaseHold
	if wanted {
		kind = protocol.DirectiveHold
	}

	for _, holder := range holders {
		before := h.HoldOf(holder)
		requested := false

		for _, d := range pending {
			if d.Host != h.Name || d.Holder != holder ||
				(d.Kind != protocol.DirectiveHold && d.Kind != protocol.DirectiveReleaseHold) {
				continue
			}

			requested = requested || d.Kind == protocol.DirectiveHold
			d.State, d.Error, d.UpdatedAt = fleet.DirectiveCancelled, "replaced by a later request", now
			changes.Directives = append(changes.Directives, d)
		}

		if !wanted && before == nil && !requested {
			continue
		}

		d := &fleet.Directive{
			Directive: protocol.Directive{ID: s.opts.NewID(), Kind: kind, Holder: holder, Reason: reason, By: by},
			Host:      h.Name, State: fleet.DirectivePending,
			CreatedBy: by.User, CreatedAt: now, UpdatedAt: now,
		}

		changes.Directives = append(changes.Directives, d)
		changes.Audit = append(changes.Audit, s.entry(actor, action, h.Name, map[string]any{
			keyHolder: holder, keyDirective: d.ID, keyBefore: holdDetail(before),
			keyAfter: map[string]any{keyHeld: wanted, keyBy: by.User, keyReason: reason}, keyReason: reason,
		}))
	}

	return changes
}

// holdTeams are the teams among teams that own a managed container of the
// host, sorted. A team named like staff's holder holds nothing.
func holdTeams(h *fleet.Host, teams []string) []string {
	out := make([]string, 0, len(teams))

	if h.Status != nil {
		for i := range h.Status.Containers {
			c := &h.Status.Containers[i]
			if c.Managed && c.Team != fleet.HolderStaff && slices.Contains(teams, c.Team) &&
				!slices.Contains(out, c.Team) {
				out = append(out, c.Team)
			}
		}
	}

	slices.Sort(out)

	return out
}

// holdDetail is one hold in the before or after of an audit entry.
func holdDetail(hold *protocol.HostHold) map[string]any {
	if hold == nil {
		return map[string]any{keyHeld: false}
	}

	return map[string]any{keyHeld: true, keyBy: hold.By, keyReason: hold.Reason}
}

// MarkOffline marks hosts as known offline: their validators are left out of
// the gate and the budget, and they get no lease, until the mark is lifted or
// its end passes.
func (s *service) MarkOffline(
	ctx context.Context, actor string, req OfflineRequest,
) (*ActionResult, error) {
	return s.setOffline(ctx, actor, req.Hosts, &fleet.OfflineMark{Reason: req.Reason, Until: req.Until}, req.Reason)
}

// UnmarkOffline lifts the offline mark.
func (s *service) UnmarkOffline(
	ctx context.Context, actor string, req OfflineRequest,
) (*ActionResult, error) {
	return s.setOffline(ctx, actor, req.Hosts, nil, req.Reason)
}

// setOffline sets (mark) or lifts (nil) the offline mark of hosts, one
// transaction with its audit entry per host.
func (s *service) setOffline(
	ctx context.Context, actor string, names []string, mark *fleet.OfflineMark, given string,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.selectHosts(ctx, names, false)
	if err != nil {
		return nil, err
	}

	action := "action.unmark-offline"
	if mark != nil {
		action = "action.mark-offline"
	}

	res := &ActionResult{}

	for _, h := range hosts {
		if mark == nil && h.Offline == nil {
			continue
		}

		err := s.write(ctx, &store.Changes{
			Offline: []store.HostOffline{{Host: h.Name, Mark: mark}},
			Audit: []fleet.AuditEntry{s.entry(actor, action, h.Name, map[string]any{
				keyBefore: offlineDetail(h.Offline),
				keyAfter:  offlineDetail(mark),
				keyReason: given,
			})},
		})
		if err != nil {
			res.Message = fmt.Sprintf("stopped at %s; changed before it: %d host(s)", h.Name, len(res.Hosts))

			if len(res.Hosts) > 0 {
				s.Nudge()
			}

			return res, err
		}

		res.Hosts = append(res.Hosts, h.Name)
	}

	res.Message = fmt.Sprintf("offline mark lifted on %d host(s)", len(res.Hosts))
	if mark != nil {
		res.Message = fmt.Sprintf("%d host(s) marked known offline", len(res.Hosts))
	}

	if len(res.Hosts) > 0 {
		s.Nudge()
	}

	return res, nil
}

// offlineDetail is an offline mark in the before or after of an audit entry.
func offlineDetail(mark *fleet.OfflineMark) map[string]any {
	if mark == nil {
		return map[string]any{"offline": false}
	}

	detail := map[string]any{"offline": true, keyReason: mark.Reason}
	if mark.Until != nil {
		detail["until"] = mark.Until.UTC().Format(time.RFC3339)
	}

	return detail
}

// Pause pauses everything (no Rollout) or one rollout.
func (s *service) Pause(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Rollout == "" {
		return s.setGlobalPause(ctx, actor, "action.pause", true, req.Reason)
	}

	r, err := s.openRollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	res := &ActionResult{Rollout: r.ID, Message: "rollout paused"}
	if r.Paused {
		return res, nil
	}

	r.Paused = true

	return res, s.commitRollout(ctx, actor, "action.pause", r, map[string]any{
		keyBefore: map[string]any{keyPaused: false},
		keyAfter:  map[string]any{keyPaused: true},
		keyReason: req.Reason,
	})
}

// Resume lifts the global pause or the pause of one rollout, and approves a
// rollout that waits on a manual policy.
func (s *service) Resume(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Rollout == "" {
		return s.setGlobalPause(ctx, actor, "action.resume", false, req.Reason)
	}

	r, err := s.openRollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}

	before := map[string]any{keyPaused: r.Paused, "approved": r.Approved}
	res := &ActionResult{Rollout: r.ID, Message: "rollout resumed"}
	approve := !r.Approved && settings.RolloutPolicy(r) == fleet.PolicyManual

	if !r.Paused && !approve {
		res.Message = "rollout is not paused and needs no approval"

		return res, nil
	}

	r.Paused = false
	r.PauseReason = ""

	if approve {
		r.Approved = true
		res.Message = "rollout approved"
	}

	return res, s.commitRollout(ctx, actor, "action.resume", r, map[string]any{
		keyBefore: before,
		keyAfter:  map[string]any{keyPaused: r.Paused, "approved": r.Approved},
		keyReason: req.Reason,
	})
}

// IgnoreGate lets the leases of an open rollout pass a closed network gate.
func (s *service) IgnoreGate(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.openRollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	res := &ActionResult{Rollout: r.ID, Message: "rollout passes the network gate"}
	if r.PassesGate() {
		return res, nil
	}

	r.IgnoreGate = true

	return res, s.commitRollout(ctx, actor, "action.ignore-gate", r, map[string]any{
		keyBefore: map[string]any{keyIgnoreGate: false},
		keyAfter:  map[string]any{keyIgnoreGate: true},
		keyReason: req.Reason,
	})
}

// ApproveLease marks the verifying lease of one member of a rollout as
// approved; the next reconcile releases it as ready (pass.verify).
func (s *service) ApproveLease(
	ctx context.Context, actor string, req ApproveLeaseRequest,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.rollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	m := r.Host(req.Host)

	switch {
	case m == nil:
		return nil, fmt.Errorf("%w: %s is no host of rollout %s", ErrUnknownHost, req.Host, r.ID)
	case m.State != fleet.HostLeased || m.LeaseID == "":
		return nil, fmt.Errorf("%w: %s holds no lease in rollout %s", ErrInvalidAction, m.Host, r.ID)
	}

	l, err := s.opts.Store.GetLease(ctx, m.LeaseID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("%w: %s", ErrUnknownLease, m.LeaseID)
	case err != nil:
		return nil, fmt.Errorf("scheduler: get lease %s: %w", m.LeaseID, err)
	case l.State != fleet.LeaseVerifying:
		return nil, fmt.Errorf("%w: lease %s is %s, not verifying", ErrInvalidAction, l.ID, l.State)
	case l.Cancelled != nil:
		return nil, fmt.Errorf("%w: lease %s was ended by %s", ErrInvalidAction, l.ID, l.Cancelled.By)
	}

	res := &ActionResult{
		Message: fmt.Sprintf("lease %s of %s approved", l.ID, l.Host),
		Rollout: r.ID,
		Hosts:   []string{l.Host},
	}

	if l.Approved != nil {
		return res, nil
	}

	l.Approved = &fleet.LeaseApproval{At: s.opts.Now(), By: actorName(actor), Reason: req.Reason}

	return res, s.commit(ctx, &store.Changes{
		Leases: []*fleet.Lease{l},
		Audit: []fleet.AuditEntry{s.entry(actor, "action.approve-lease", l.Host, map[string]any{
			keyLease: l.ID, keyRollouts: slices.Compact(slices.Sorted(maps.Values(l.Rollouts))),
			"readiness": l.Readiness, keyReason: req.Reason,
		})},
	})
}

// ImageOverride sends the instruction to set or take away an image override.
func (s *service) ImageOverride(
	ctx context.Context, actor string, req ImageOverrideRequest,
) (*ActionResult, error) {
	return s.override(ctx, actor, "action.image-override", req.Host, req.Container, req.Reason,
		&protocol.OverrideInstruction{Part: protocol.OverrideImage, Image: req.Image, Until: req.Until},
		req.Image != "")
}

// FlagOverride sends the instruction to set or take away a flag override.
func (s *service) FlagOverride(
	ctx context.Context, actor string, req FlagOverrideRequest, //nolint:gocritic // hugeParam: the signature is the Service contract.
) (*ActionResult, error) {
	return s.override(ctx, actor, "action.flag-override", req.Host, req.Container, req.Reason,
		&protocol.OverrideInstruction{
			Part: protocol.OverrideFlags, AddFlags: req.AddFlags, RemoveFlags: req.RemoveFlags,
			Until: req.Until,
		}, len(req.AddFlags)+len(req.RemoveFlags) > 0)
}

// override queues an override directive for one host. It creates no
// rollout: the host reports the need once its agent has written the file,
// and that need is served in the host's own rollout. A host that cannot take
// the instruction is refused here with the reason; what its agent refuses is
// audited when it answers (syncDirectives).
func (s *service) override(
	ctx context.Context, actor, action, host, container, reason string,
	inst *protocol.OverrideInstruction, sets bool,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.selectHosts(ctx, []string{host}, false)
	if err != nil {
		return nil, err
	}

	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}

	now := s.opts.Now()
	h := hosts[0]

	if refusal := overrideRefusal(h, container, inst, sets, now, s.since, &settings); refusal != "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidAction, refusal)
	}

	if inst.Until, err = overrideUntil(inst.Until, sets, now); err != nil {
		return nil, err
	}

	changes := &store.Changes{}
	s.overrideDirective(ctx, changes, actor, action, h.Name, container, reason, inst, sets, now)

	if err := s.commit(ctx, changes); err != nil {
		return nil, err
	}

	return &ActionResult{
		Message: "sent to " + h.Name + ": its agent decides, and the container follows under the host's next lease",
		Hosts:   []string{h.Name},
	}, nil
}

// ClientOverride sends one override instruction to each named host, for its
// container of the client and kind, in one commit. A host that cannot take
// it is listed in Rejected with the reason; the others are sent theirs.
func (s *service) ClientOverride(
	ctx context.Context, actor string, req ClientOverrideRequest, //nolint:gocritic // hugeParam: the signature is the Service contract.
) (*ActionResult, error) {
	sets := req.Sets()
	inst := protocol.OverrideInstruction{Part: req.Part}

	action := "action.flag-override"
	if req.Part == protocol.OverrideImage {
		action, inst.Image = "action.image-override", req.Image
	} else {
		inst.AddFlags, inst.RemoveFlags = req.AddFlags, req.RemoveFlags
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list hosts: %w", err)
	}

	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}

	now := s.opts.Now()

	if inst.Until, err = overrideUntil(req.Until, sets, now); err != nil {
		return nil, err
	}

	byName := make(map[string]*fleet.Host, len(all))
	for _, h := range all {
		byName[h.Name] = h
	}

	res := &ActionResult{Hosts: make([]string, 0, len(req.Hosts)), Rejected: make(map[string]string)}
	changes := &store.Changes{}

	for _, name := range req.Hosts {
		h := byName[name]
		if h == nil {
			res.Rejected[name] = "unknown host"

			continue
		}

		container := ClientContainer(h, req.Client, req.Kind)
		if container == "" {
			res.Rejected[name] = fmt.Sprintf("host %s runs no %s container of %s", name, req.Kind, req.Client)

			continue
		}

		one := inst
		if refusal := overrideRefusal(h, container, &one, sets, now, s.since, &settings); refusal != "" {
			res.Rejected[name] = refusal

			continue
		}

		s.overrideDirective(ctx, changes, actor, action, name, container, req.Reason, &one, sets, now)
		res.Hosts = append(res.Hosts, name)
	}

	if len(changes.Directives) > 0 {
		if err := s.commit(ctx, changes); err != nil {
			return nil, err
		}
	}

	res.Message = fmt.Sprintf("sent to %d of %d hosts: each agent decides, and its container follows under the host's next lease",
		len(res.Hosts), len(req.Hosts))

	return res, nil
}

// overrideUntil is the end of an override that is set: by default
// DefaultOverrideDuration from now, at most MaxOverrideDuration. One that is
// taken away has none.
func overrideUntil(asked *time.Time, sets bool, now time.Time) (*time.Time, error) {
	if !sets {
		return nil, nil //nolint:nilnil // no end is the answer for an override that is taken away.
	}

	until := now.Add(DefaultOverrideDuration)
	if asked != nil {
		until = *asked
	}

	switch {
	case !until.After(now):
		return nil, fmt.Errorf("%w: until must lie in the future", ErrInvalidAction)
	case until.After(now.Add(MaxOverrideDuration)):
		return nil, fmt.Errorf("%w: an override ends at most %s from now",
			ErrInvalidAction, fleet.Duration(MaxOverrideDuration).Std())
	}

	until = until.UTC().Truncate(time.Second)

	return &until, nil
}

// overrideDirective adds the directive of one override instruction, and its
// audit entry, to changes.
func (s *service) overrideDirective(
	ctx context.Context, changes *store.Changes, actor, action, host, container, reason string,
	inst *protocol.OverrideInstruction, sets bool, now time.Time,
) {
	by := &protocol.Actor{User: actorName(actor)}
	if a, ok := AuthorizationFrom(ctx); ok {
		by.Teams, by.Feature = slices.Clone(a.Teams), a.By
	}

	d := &fleet.Directive{
		Directive: protocol.Directive{
			ID: s.opts.NewID(), Kind: protocol.DirectiveOverride, Container: container,
			Reason: reason, By: by, Override: inst,
		},
		Host: host, State: fleet.DirectivePending,
		CreatedBy: by.User, CreatedAt: now, UpdatedAt: now,
	}

	detail := map[string]any{
		"container": container, "directive": d.ID, keyReason: reason, "part": inst.Part,
	}

	switch {
	case !sets:
		detail["clear"] = true
	case inst.Part == protocol.OverrideImage:
		detail["image"], detail["until"] = inst.Image, inst.Until
	default:
		detail["addFlags"], detail["removeFlags"], detail["until"] = inst.AddFlags, inst.RemoveFlags, inst.Until
	}

	changes.Directives = append(changes.Directives, d)
	changes.Audit = append(changes.Audit, s.entry(actor, action, host, detail))
}

// overrideRefusal says why a host cannot take an override instruction, by
// what its agent last reported; empty when it can. The agent has the last
// word: it checks the instruction against its own configuration.
func overrideRefusal(
	h *fleet.Host, container string, inst *protocol.OverrideInstruction, sets bool,
	now, since time.Time, settings *fleet.Settings,
) string {
	switch {
	case h.Status == nil:
		return "host " + h.Name + ": no sync yet"
	case h.Incompatible() != "":
		return "host " + h.Name + ": " + h.Incompatible()
	case hostStale(h, now, since, settings.HostStaleAfter.Std()):
		return "host " + h.Name + ": " + reasonAgentSilent
	case !slices.Contains(h.Status.Verbs, protocol.VerbRoll):
		return "host " + h.Name + ": verb roll not allowed by the agent"
	}

	i := slices.IndexFunc(h.Status.Containers, func(c protocol.Container) bool {
		return c.Managed && c.Name == container
	})
	if i < 0 {
		return "host " + h.Name + " manages no container " + container
	}

	c := &h.Status.Containers[i]

	switch {
	case !sets:
		// Taking an override away is always allowed: it brings the
		// container back to what Ansible deployed.
		return ""
	case !c.Overridable && c.Team == "":
		return "container " + container + " on host " + h.Name + " cannot be overridden: it has no team"
	case !c.Overridable:
		return "container " + container + " on host " + h.Name + " cannot be overridden: overrides are off on the host"
	default:
		return ""
	}
}

// SetClientPolicy sets or clears the image policy of one client. Open image
// rollouts of the client answer to it from the next reconcile on: one that
// was approved stays approved.
func (s *service) SetClientPolicy(
	ctx context.Context, actor string, req ClientPolicyRequest,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}

	next := settings
	next.PolicyClients = maps.Clone(settings.PolicyClients)

	if req.Policy == "" {
		delete(next.PolicyClients, req.Client)
	} else {
		if next.PolicyClients == nil {
			next.PolicyClients = make(fleet.ClientPolicies, 1)
		}

		next.PolicyClients[req.Client] = req.Policy
	}

	if len(next.PolicyClients) == 0 {
		next.PolicyClients = nil
	}

	if err := next.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAction, err)
	}

	res := &ActionResult{Message: fmt.Sprintf("image policy of %s: %s", req.Client, req.Policy)}
	if req.Policy == "" {
		res.Message = fmt.Sprintf("image policy of %s: the default of each kind", req.Client)
	}

	if next.Equal(&settings) {
		return res, nil
	}

	if err := s.saveSettings(ctx, actor, "action.client-policy", &next, req.Reason); err != nil {
		return nil, err
	}

	s.Nudge()

	return res, nil
}

func (s *service) setGlobalPause(
	ctx context.Context, actor, action string, paused bool, reason string,
) (*ActionResult, error) {
	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}

	res := &ActionResult{Message: "resumed"}
	if paused {
		res.Message = "paused"
	}

	if settings.Paused == paused {
		return res, nil
	}

	settings.Paused = paused

	if err := s.saveSettings(ctx, actor, action, &settings, reason); err != nil {
		return nil, err
	}

	s.Nudge()

	return res, nil
}

// openRollout loads a rollout that an action may still change.
func (s *service) openRollout(ctx context.Context, id string) (*fleet.Rollout, error) {
	r, err := s.rollout(ctx, id)
	if err != nil {
		return nil, err
	}

	switch {
	case r.State == fleet.RolloutAborted:
		return nil, fmt.Errorf("%w: rollout %s is aborted, which is final", ErrInvalidAction, id)
	case r.State.Terminal():
		return nil, fmt.Errorf("%w: rollout %s is %s", ErrInvalidAction, id, r.State)
	}

	return r, nil
}

func (s *service) rollout(ctx context.Context, id string) (*fleet.Rollout, error) {
	r, err := s.opts.Store.GetRollout(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRollout, id)
	}

	if err != nil {
		return nil, fmt.Errorf("scheduler: get rollout %s: %w", id, err)
	}

	return r, nil
}

// commitRollout writes one changed rollout with its audit entry.
func (s *service) commitRollout(
	ctx context.Context, actor, action string, r *fleet.Rollout, detail map[string]any,
) error {
	r.UpdatedAt = s.opts.Now()

	return s.commit(ctx, &store.Changes{
		Rollouts: []*fleet.Rollout{r},
		Audit:    []fleet.AuditEntry{s.entry(actor, action, r.ID, detail)},
	})
}

// Skip marks hosts as skipped in a rollout. Hosts that hold a lease for it
// or are done cannot be skipped. In a rollout of an observed cause the skip
// holds, and the rollout stays open, for as long as the host has the need
// (syncSkipped).
func (s *service) Skip(
	ctx context.Context, actor string, req RolloutHostsRequest,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.openRollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	if len(req.Hosts) == 0 {
		return nil, fmt.Errorf("%w: no hosts named", ErrInvalidAction)
	}

	now := s.opts.Now()
	res := &ActionResult{Rollout: r.ID, Rejected: make(map[string]string, len(req.Hosts))}

	for _, name := range req.Hosts {
		m := r.Host(name)

		switch {
		case m == nil:
			res.Rejected[name] = "not a member of the rollout"
		case m.State == fleet.HostLeased:
			res.Rejected[name] = "holds a lease for the rollout"
		case m.State.Settled():
			res.Rejected[name] = "already " + string(m.State)
		default:
			m.State = fleet.HostSkipped
			m.SkippedBy = actorName(actor)
			m.Error = req.Reason
			m.UpdatedAt = now

			res.Hosts = append(res.Hosts, name)
		}
	}

	if len(res.Hosts) == 0 {
		res.Message = "no host could be skipped"

		return res, fmt.Errorf("%w: %s", ErrInvalidAction, res.Message)
	}

	res.Message = fmt.Sprintf("%d host(s) skipped", len(res.Hosts))

	return res, s.commitRollout(ctx, actor, "action.skip", r, map[string]any{
		keyHosts: res.Hosts, keyReason: req.Reason,
	})
}

// Retry returns failed and cancelled hosts of an open rollout to pending.
//
// Without hosts it is about the rollout: every failed or cancelled member
// returns to pending and a halted rollout is active again, also when no
// member is failed.
//
// With hosts it is about those hosts: the named members that are failed or
// cancelled, or that a person skipped, return to pending. The rollout leaves its halt only
// when one of the hosts returned to pending had failed and no member is
// failed after that. A failed host that was not named keeps it halted, and
// taking back a skip lifts no halt: a halt whose failed hosts were all
// skipped is lifted by a retry of the rollout. When none of the named hosts
// could be retried nothing changes and the action is refused, with the reason
// for each host.
//
// A failed host whose lease did its steps and whose need is gone is not
// done by that: it gets a recheck lease, which repeats the readiness check.
//
// A rollout that is closed cannot be retried; an abort is final.
func (s *service) Retry(
	ctx context.Context, actor string, req RolloutHostsRequest,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.openRollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	now := s.opts.Now()
	retriedFailed := false
	res := &ActionResult{Rollout: r.ID, Rejected: make(map[string]string, len(req.Hosts))}
	failedLeases := make(map[string]string, len(r.Hosts))

	for i := range r.Hosts {
		m := &r.Hosts[i]
		listed := slices.Contains(req.Hosts, m.Host)

		// Failed and cancelled hosts are retried unless others are listed; a
		// skip is only taken back for a host that is named.
		retryable := m.State == fleet.HostFailed || m.State == fleet.HostCancelled
		if (!retryable || (len(req.Hosts) > 0 && !listed)) && (!operatorSkipped(m) || !listed) {
			continue
		}

		retriedFailed = retriedFailed || m.State == fleet.HostFailed

		if retryable && m.LeaseID != "" {
			failedLeases[m.Host] = m.LeaseID
		}

		m.State = fleet.HostPending
		m.Error = ""
		m.SkippedBy = ""
		m.LeaseID = ""
		m.UpdatedAt = now
		res.Hosts = append(res.Hosts, m.Host)
	}

	for _, name := range req.Hosts {
		if !slices.Contains(res.Hosts, name) {
			res.Rejected[name] = "has neither failed, been cancelled nor been skipped in the rollout"
		}
	}

	before := map[string]any{keyState: r.State, keyReason: r.Reason}

	if len(req.Hosts) > 0 {
		// A retry of hosts is about those hosts. When none of them could be
		// retried nothing changes; and the rollout leaves its halt only when
		// no member is left failed: somebody else's failed host keeps it
		// halted.
		if len(res.Hosts) == 0 {
			res.Message = "no host could be retried: " + rejectedList(req.Hosts, res.Rejected)

			return res, fmt.Errorf("%w: %s", ErrInvalidAction, res.Message)
		}

		res.Message = fmt.Sprintf("%d host(s) retried", len(res.Hosts))

		// The halt is lifted by retrying what failed: at least one of the
		// retried hosts was failed, and none is failed now. Taking back a
		// skip retries no failure; a halt whose failed hosts were all skipped
		// is lifted by a retry of the rollout.
		switch failed := failedMembers(r); {
		case r.State != fleet.RolloutHalted:
		case len(failed) > 0:
			res.Message += fmt.Sprintf("; the rollout stays halted: %s still failed",
				strings.Join(failed, ", "))
		case !retriedFailed:
			res.Message += "; the rollout stays halted: none of the retried hosts had failed " +
				"(retry the rollout to lift its halt)"
		default:
			r.State = fleet.RolloutActive
			r.Reason = ""
		}
	} else {
		if len(res.Hosts) == 0 && r.State != fleet.RolloutHalted {
			res.Message = "nothing to retry"

			return res, fmt.Errorf("%w: rollout %s is %s and has no failed host to retry",
				ErrInvalidAction, r.ID, r.State)
		}

		r.State = fleet.RolloutActive
		r.Reason = ""
		res.Message = fmt.Sprintf("rollout reopened, %d host(s) retried", len(res.Hosts))
	}

	// A pause that a cancelled member caused ends with the last of them.
	if r.Paused && strings.Contains(r.PauseReason, " "+cancelPauseMark+" ") &&
		!slices.ContainsFunc(r.Hosts, func(m fleet.RolloutHost) bool { return m.State == fleet.HostCancelled }) {
		r.Paused = false
		r.PauseReason = ""
	}

	r.UpdatedAt = now
	changes := &store.Changes{
		Rollouts: []*fleet.Rollout{r},
		Audit: []fleet.AuditEntry{s.entry(actor, "action.retry", r.ID, map[string]any{
			keyHosts: res.Hosts, keyBefore: before,
			keyAfter: map[string]any{keyState: r.State}, keyReason: req.Reason,
		})},
	}

	for _, host := range sortedKeys(failedLeases) {
		l, err := s.recheck(ctx, actor, r, host, failedLeases[host], now)
		if err != nil {
			return nil, err
		}

		if l != nil {
			changes.Leases = append(changes.Leases, l)
			changes.Audit = append(changes.Audit, s.entry(actor, "lease.granted", host, map[string]any{
				keyLease: l.ID, keyRollouts: []string{r.ID}, "recheck": true,
			}))
		}
	}

	if err := s.commit(ctx, changes); err != nil {
		return nil, err
	}

	return res, nil
}

// recheck returns the recheck lease of a retried member whose failed or cancelled lease
// did the rollout's steps and whose host no longer reports their needs, and
// moves the member to it; nil when the member waits for a lease as usual.
func (s *service) recheck(
	ctx context.Context, actor string, r *fleet.Rollout, host, leaseID string, now time.Time,
) (*fleet.Lease, error) {
	failed, err := s.opts.Store.GetLease(ctx, leaseID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil //nolint:nilnil // no lease to recheck.
	case err != nil:
		return nil, fmt.Errorf("scheduler: get lease %s: %w", leaseID, err)
	case (failed.State != fleet.LeaseFailed && failed.State != fleet.LeaseCancelled) || !stepsDone(failed, r.ID):
		return nil, nil //nolint:nilnil // nothing was done to recheck.
	}

	open, err := s.opts.Store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true, Host: host})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list leases of %s: %w", host, err)
	}

	h, err := s.opts.Store.GetHost(ctx, host)
	if errors.Is(err, store.ErrNotFound) || len(open) > 0 {
		return nil, nil //nolint:nilnil // the reconcile settles the member.
	}

	if err != nil {
		return nil, fmt.Errorf("scheduler: get host %s: %w", host, err)
	}

	l := &fleet.Lease{
		ID: s.opts.NewID(), Host: host, State: fleet.LeaseVerifying,
		Rollouts: make(map[string]string, 2), Weight: failed.Weight, Charged: failed.Charged,
		GrantedBy: actorName(actor), GrantedAt: now,
		ExpiresAt: now.Add(failed.ExpiresAt.Sub(failed.GrantedAt)), AgentDoneAt: &now,
		Operation: cloneOperation(failed.Operation), Recheck: true,
	}

	for i := range failed.Needs {
		n := &failed.Needs[i]
		if failed.Rollouts[n.ID] != r.ID {
			continue
		}

		if h.Status != nil && slices.ContainsFunc(h.Status.Needs, func(o protocol.Need) bool { return o.ID == n.ID }) {
			return nil, nil //nolint:nilnil // still reported: served again by a lease.
		}

		l.Needs = append(l.Needs, *n)
		l.Rollouts[n.ID] = r.ID
	}

	// Its class is that of the needs it repeats the check of.
	l.Impact = needsImpact(h, l.Needs)
	if l.Impact != protocol.ImpactNode {
		l.Charged = 0
	}

	m := r.Host(host)
	m.State = fleet.HostLeased
	m.Served = true
	m.LeaseID = l.ID
	m.NeedID = l.Needs[0].ID

	return l, nil
}

// failedMembers names the members of a rollout that are failed.
func failedMembers(r *fleet.Rollout) []string {
	failed := make([]string, 0, len(r.Hosts))

	for i := range r.Hosts {
		if r.Hosts[i].State == fleet.HostFailed {
			failed = append(failed, r.Hosts[i].Host)
		}
	}

	return failed
}

// rejectedList writes why each named host was rejected, in the order given.
func rejectedList(hosts []string, rejected map[string]string) string {
	parts := make([]string, 0, len(hosts))

	for _, name := range hosts {
		if why, ok := rejected[name]; ok {
			parts = append(parts, name+" "+why)
		}
	}

	return strings.Join(parts, "; ")
}

// Abort closes a rollout for good: its unanswered directives are cancelled,
// its members that wait are aborted and its open leases finish, each member
// with the outcome of its lease. The target of an aborted group or packages
// rollout is not rolled again (open skips its key) until it changes or
// someone rolls it by hand; the manual needs a host took from it are
// withdrawn (syncWithdrawals).
//
// The reboot rollout and the rollout of what a host asks for itself have a
// key that does not change with their target: aborting one pauses it, with
// a reason that says so, until a person resumes it.
func (s *service) Abort(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Rollout == "" {
		return nil, fmt.Errorf("%w: abort needs a rollout", ErrInvalidAction)
	}

	r, err := s.openRollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	if r.Kind == fleet.RolloutReboot || r.Kind == fleet.RolloutLocal || (r.Kind == fleet.RolloutAction && !r.Directed()) {
		before := map[string]any{keyPaused: r.Paused}
		r.Paused = true
		r.PauseReason = abortedReason(req.Reason)

		return &ActionResult{
			Rollout: r.ID,
			Message: "rollout paused: its cause is still reported, so it stays open; resume to continue",
		}, s.commitRollout(ctx, actor, "action.abort", r, map[string]any{
			keyBefore: before,
			keyAfter:  map[string]any{keyPaused: true},
			keyReason: req.Reason,
		})
	}

	pending, err := s.opts.Store.ListDirectives(ctx, store.DirectiveFilter{
		PendingOnly: true, Rollouts: []string{r.ID},
	})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list directives: %w", err)
	}

	now := s.opts.Now()
	before := r.State
	r.State = fleet.RolloutAborted
	r.Reason = cmpOr(req.Reason, "aborted")
	r.EndedAt = &now
	r.UpdatedAt = now

	for i := range r.Hosts {
		if m := &r.Hosts[i]; !m.State.Settled() && m.State != fleet.HostLeased && m.State != fleet.HostFailed {
			m.State = fleet.HostAborted
			m.UpdatedAt = now
		}
	}

	changes := &store.Changes{
		Rollouts: []*fleet.Rollout{r},
		Audit: []fleet.AuditEntry{s.entry(actor, "action.abort", r.ID, map[string]any{
			keyBefore: map[string]any{keyState: before},
			keyAfter:  map[string]any{keyState: r.State}, keyReason: req.Reason,
		})},
	}

	for _, d := range pending {
		d.State = fleet.DirectiveCancelled
		d.UpdatedAt = now
		changes.Directives = append(changes.Directives, d)
	}

	if err := s.commit(ctx, changes); err != nil {
		return nil, err
	}

	msg := "rollout aborted"
	if !r.Directed() {
		msg += ": its target is not rolled again until it changes or someone rolls it by hand"
	}

	return &ActionResult{Rollout: r.ID, Message: msg}, nil
}

// abortedReason is the pause reason of a reboot or local rollout that a
// person aborted.
func abortedReason(reason string) string {
	if reason == "" {
		return reasonAborted + reasonResume
	}

	return reasonAborted + ": " + reason + reasonResume
}

// EndLease asks for the end of one open lease. It is what a person does
// about a lease that nobody expects to end by itself any more (an agent that
// reports progress for hours, a host that is being taken away): while the
// lease is open the host counts as unavailable.
//
// The action ends nothing by itself. It marks the lease as cancelled. The
// lease stays open: it keeps its place in the budget, in the host limit and
// in the gate, because the host is as far down as it was a moment ago. From
// the next sync on the agent is told (protocol.Lease.Cancel) and begins no
// further step. The lease then ends as cancelled unless the agent reports a
// failed step: when the agent reports the end of its operation, at once when
// the lease is being verified, and for a lease that was only granted with the
// agent's next sync: the agent declines it, or never had it. An agent that
// stays silent fails the lease as it fails any other. A cancelled lease
// pauses its rollouts and leaves its members cancelled: the host is offered
// nothing for them until a person retries it; a resume goes on with the
// others. A person who wants the host left alone beyond that holds it.
//
// Ending a lease that is already cancelled changes nothing and succeeds.
func (s *service) EndLease(
	ctx context.Context, actor string, req EndLeaseRequest,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Lease == "" {
		return nil, fmt.Errorf("%w: no lease named", ErrInvalidAction)
	}

	l, err := s.opts.Store.GetLease(ctx, req.Lease)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("%w: %s", ErrUnknownLease, req.Lease)
	case err != nil:
		return nil, fmt.Errorf("scheduler: get lease %s: %w", req.Lease, err)
	case l.State.Terminal():
		return nil, fmt.Errorf("%w: lease %s is %s", ErrInvalidAction, l.ID, l.State)
	case l.Cancelled != nil:
		return &ActionResult{
			Message: fmt.Sprintf("lease %s of %s was already ended by %s; it is open until the agent reports",
				l.ID, l.Host, l.Cancelled.By),
			Hosts: []string{l.Host},
		}, nil
	}

	state := l.State
	l.Cancelled = &fleet.LeaseCancel{
		At: s.opts.Now(), By: actorName(actor), State: state, Reason: req.Reason,
	}

	if err := s.commit(ctx, &store.Changes{
		Leases: []*fleet.Lease{l},
		Audit: []fleet.AuditEntry{s.entry(actor, "action.end-lease", l.Host, map[string]any{
			keyLease: l.ID, keyState: state, keyReason: req.Reason,
		})},
	}); err != nil {
		return nil, err
	}

	return &ActionResult{
		Message: fmt.Sprintf("lease %s of %s is ending", l.ID, l.Host),
		Hosts:   []string{l.Host},
	}, nil
}

// Forget deletes hosts that are gone. A host that holds a lease is refused.
func (s *service) Forget(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.selectHosts(ctx, req.Hosts, false)
	if err != nil {
		return nil, err
	}

	leases, err := s.opts.Store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list open leases: %w", err)
	}

	pending, err := s.opts.Store.ListDirectives(ctx, store.DirectiveFilter{PendingOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list directives: %w", err)
	}

	res := &ActionResult{Rejected: make(map[string]string, len(hosts))}

	// The store has no transaction over several hosts: when one fails, the
	// result names the hosts that are already gone.
	for _, h := range hosts {
		if slices.ContainsFunc(leases, func(l *fleet.Lease) bool {
			return l.Host == h.Name
		}) {
			res.Rejected[h.Name] = "holds a lease"

			continue
		}

		if err := s.forgetHost(ctx, actor, h.Name, req.Reason, pending); err != nil {
			res.Message = fmt.Sprintf("stopped at %s; forgotten before it: %d host(s)", h.Name, len(res.Hosts))

			return res, err
		}

		res.Hosts = append(res.Hosts, h.Name)
	}

	if len(res.Hosts) == 0 {
		res.Message = "no host could be forgotten"

		return res, fmt.Errorf("%w: %s", ErrInvalidAction, res.Message)
	}

	res.Message = fmt.Sprintf("%d host(s) forgotten", len(res.Hosts))

	return res, nil
}

// forgetHost deletes one host. The audit entry is written first, so a host
// never disappears without one; a delete that then fails is audited as
// failed. The host's pending directives are cancelled last: the reconcile
// cancels them as well, so a failure there is only logged.
func (s *service) forgetHost(
	ctx context.Context, actor, name, reason string, pending []*fleet.Directive,
) error {
	if err := s.appendAudit(ctx, s.entry(actor, "action.forget", name, map[string]any{
		keyReason: reason,
	})); err != nil {
		return err
	}

	if err := s.opts.Store.DeleteHost(ctx, name); err != nil {
		err = fmt.Errorf("scheduler: delete host %s: %w", name, err)

		if auditErr := s.appendAudit(ctx, s.entry(actor, "action.forget.failed", name, map[string]any{
			"error": err.Error(),
		})); auditErr != nil {
			return errors.Join(err, auditErr)
		}

		return err
	}

	now := s.opts.Now()
	changes := &store.Changes{}

	for _, d := range pending {
		if d.Host == name {
			d.State = fleet.DirectiveCancelled
			d.Error = reasonHostForgotten
			d.UpdatedAt = now
			changes.Directives = append(changes.Directives, d)
		}
	}

	if !changes.Empty() {
		if err := s.write(ctx, changes); err != nil {
			s.log.WithError(err).WithField("host", name).
				Warn("Could not cancel the directives of a forgotten host; the next reconcile does")
		}
	}

	s.Nudge()

	return nil
}
