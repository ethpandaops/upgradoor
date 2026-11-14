package scheduler

import (
	"context"
	"errors"
	"fmt"
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
	keyHosts    = "hosts"
	keyBefore   = "before"
	keyAfter    = "after"
	keyForce    = "force"
	keyPaused   = "paused"
	keyState    = "state"
	keyKey      = "key"
	keyRollouts = "rollouts"
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
	reason       string
}

// Roll requests a manual image roll.
func (s *service) Roll(ctx context.Context, actor string, req RollRequest) (*ActionResult, error) { //nolint:gocritic // hugeParam: the signature is the Service contract.
	return s.manual(ctx, actor, &manualRequest{
		verb: protocol.VerbRoll, hosts: req.Hosts, all: req.All, containers: req.Containers,
		staggered: req.Staggered, force: req.Force, ignoreBudget: req.IgnoreBudget, reason: req.Reason,
	})
}

// Reboot requests a manual reboot.
func (s *service) Reboot(ctx context.Context, actor string, req RebootRequest) (*ActionResult, error) {
	return s.manual(ctx, actor, &manualRequest{
		verb: protocol.VerbReboot, hosts: req.Hosts, all: req.All,
		staggered: req.Staggered, force: req.Force, ignoreBudget: req.IgnoreBudget, reason: req.Reason,
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
		CreatedBy:    actorName(actor),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	r.Key = prefixManual + r.ID
	changes := &store.Changes{Rollouts: []*fleet.Rollout{r}}

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
					ID: s.opts.NewID(), Kind: kind, Container: c, Reason: req.reason,
				},
				Host: h.Name, State: fleet.DirectivePending, RolloutID: r.ID,
				CreatedBy: r.CreatedBy, CreatedAt: now, UpdatedAt: now,
			})
		}
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
		keyReason: req.reason,
	})}

	if err := s.commit(ctx, changes); err != nil {
		return nil, err
	}

	res.Rollout = r.ID
	res.Message = r.Title

	return res, nil
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
	case h.Held():
		return nil, reasonHeld
	case hostStale(h, now, since, settings.HostStaleAfter.Std()):
		return nil, reasonAgentSilent
	case !slices.Contains(h.Status.Verbs, req.verb):
		return nil, fmt.Sprintf("verb %s not allowed by the agent", req.verb)
	case req.verb != protocol.VerbRoll:
		return []string{""}, ""
	}

	for i := range h.Status.Containers {
		c := &h.Status.Containers[i]
		if !c.Managed || c.Custom {
			continue
		}

		if len(req.containers) == 0 || slices.Contains(req.containers, c.Name) {
			targets = append(targets, c.Name)
		}
	}

	if len(targets) == 0 {
		return nil, "no managed, non-custom container to roll"
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

// Hold puts hosts on controller hold.
func (s *service) Hold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error) {
	return s.setHold(ctx, actor, &req, true)
}

// Unhold lifts the controller hold.
func (s *service) Unhold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error) {
	return s.setHold(ctx, actor, &req, false)
}

func (s *service) setHold(
	ctx context.Context, actor string, req *HoldRequest, wanted bool,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.selectHosts(ctx, req.Hosts, false)
	if err != nil {
		return nil, err
	}

	action, reason := "action.unhold", ""
	if wanted {
		action, reason = "action.hold", req.Reason
	}

	res := &ActionResult{}

	// The store has no transaction over several hosts: when one fails, the
	// earlier ones stay changed and audited, and the result names them.
	for _, h := range hosts {
		if err := s.setHostHold(ctx, actor, action, h, wanted, reason, req.Reason); err != nil {
			res.Message = fmt.Sprintf("stopped at %s; changed before it: %d host(s)", h.Name, len(res.Hosts))

			if len(res.Hosts) > 0 {
				s.Nudge()
			}

			return res, err
		}

		res.Hosts = append(res.Hosts, h.Name)
	}

	res.Message = fmt.Sprintf("hold lifted on %d host(s)", len(res.Hosts))
	if wanted {
		res.Message = fmt.Sprintf("%d host(s) on hold", len(res.Hosts))
	}

	s.Nudge()

	return res, nil
}

// setHostHold changes the hold switch of one host and audits it. A change
// that could not be audited is taken back.
func (s *service) setHostHold(
	ctx context.Context, actor, action string, h *fleet.Host, wanted bool, reason, given string,
) error {
	if err := s.opts.Store.SetHostHold(ctx, h.Name, wanted, reason); err != nil {
		return fmt.Errorf("scheduler: set hold of %s: %w", h.Name, err)
	}

	err := s.appendAudit(ctx, s.entry(actor, action, h.Name, map[string]any{
		keyBefore: map[string]any{"holdWanted": h.HoldWanted, "holdReason": h.HoldReason},
		keyAfter:  map[string]any{"holdWanted": wanted, "holdReason": reason},
		keyReason: given,
	}))
	if err == nil {
		return nil
	}

	if undo := s.opts.Store.SetHostHold(ctx, h.Name, h.HoldWanted, h.HoldReason); undo != nil {
		return errors.Join(err, fmt.Errorf("scheduler: restore hold of %s: %w", h.Name, undo))
	}

	return err
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
	approve := !r.Approved && rolloutPolicy(&settings, r.Kind) == fleet.PolicyManual

	if !r.Paused && !approve {
		res.Message = "rollout is not paused and needs no approval"

		return res, nil
	}

	r.Paused = false

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

// rolloutPolicy is the policy that governs a rollout kind. Manual rollouts
// are requested by a person and answer to no policy.
func rolloutPolicy(settings *fleet.Settings, kind fleet.RolloutKind) fleet.Policy {
	switch kind {
	case fleet.RolloutImage:
		return settings.PolicyImage
	case fleet.RolloutPackages:
		return settings.PolicyPackages
	case fleet.RolloutReboot:
		return settings.PolicyReboot
	default:
		return fleet.PolicyAuto
	}
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

	if r.State.Terminal() {
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
// or are done cannot be skipped.
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
			m.Error = strings.TrimSpace(skippedPrefix + " " + req.Reason)
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

// Retry reopens a halted rollout: the failed hosts (all, or the listed
// ones) return to pending. A listed host that a person skipped returns to
// pending as well. For an aborted rollout it lifts the block on the cause by
// opening a new rollout for the same key; the aborted one stays closed. For
// any other closed rollout it lifts the skips (all, or the listed hosts)
// that still keep hosts from being served for its key.
func (s *service) Retry(
	ctx context.Context, actor string, req RolloutHostsRequest,
) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.rollout(ctx, req.Rollout)
	if err != nil {
		return nil, err
	}

	if r.State == fleet.RolloutAborted {
		return s.retryAborted(ctx, actor, r, req.Reason)
	}

	if r.State.Terminal() {
		return s.retrySkipped(ctx, actor, r, &req)
	}

	now := s.opts.Now()
	res := &ActionResult{Rollout: r.ID, Rejected: make(map[string]string, len(req.Hosts))}

	for i := range r.Hosts {
		m := &r.Hosts[i]
		listed := slices.Contains(req.Hosts, m.Host)

		// Failed hosts are retried unless others are listed; a skip is only
		// taken back for a host that is named.
		if (m.State != fleet.HostFailed || (len(req.Hosts) > 0 && !listed)) &&
			(!operatorSkipped(m) || !listed) {
			continue
		}

		m.State = fleet.HostPending
		m.Error = ""
		m.LeaseID = ""
		m.UpdatedAt = now
		res.Hosts = append(res.Hosts, m.Host)
	}

	for _, name := range req.Hosts {
		if !slices.Contains(res.Hosts, name) {
			res.Rejected[name] = "has neither failed nor been skipped in the rollout"
		}
	}

	if len(res.Hosts) == 0 && r.State != fleet.RolloutHalted {
		res.Message = "nothing to retry"

		return res, fmt.Errorf("%w: rollout %s is %s and has no failed host to retry",
			ErrInvalidAction, r.ID, r.State)
	}

	before := map[string]any{keyState: r.State, keyReason: r.Reason}
	r.State = fleet.RolloutActive
	r.Reason = ""
	res.Message = fmt.Sprintf("rollout reopened, %d host(s) retried", len(res.Hosts))

	return res, s.commitRollout(ctx, actor, "action.retry", r, map[string]any{
		keyHosts: res.Hosts, keyBefore: before,
		keyAfter: map[string]any{keyState: r.State}, keyReason: req.Reason,
	})
}

// retrySkipped lifts the skips a closed rollout still holds for its key.
// The rollout stays closed; the hosts open a new one with their next sync.
func (s *service) retrySkipped(
	ctx context.Context, actor string, r *fleet.Rollout, req *RolloutHostsRequest,
) (*ActionResult, error) {
	now := s.opts.Now()
	res := &ActionResult{Rollout: r.ID, Rejected: make(map[string]string, len(req.Hosts))}

	for i := range r.Hosts {
		m := &r.Hosts[i]
		if !operatorSkipped(m) || (len(req.Hosts) > 0 && !slices.Contains(req.Hosts, m.Host)) {
			continue
		}

		m.Error = strings.TrimSpace(skipLiftedPrefix + req.Reason)
		m.UpdatedAt = now
		res.Hosts = append(res.Hosts, m.Host)
	}

	for _, name := range req.Hosts {
		if !slices.Contains(res.Hosts, name) {
			res.Rejected[name] = "is not skipped in the rollout"
		}
	}

	if len(res.Hosts) == 0 || r.Kind == fleet.RolloutManual {
		res.Message = "nothing to retry"

		return res, fmt.Errorf("%w: rollout %s is %s and holds no skip to lift",
			ErrInvalidAction, r.ID, r.State)
	}

	// Only the latest rollout of the key carries the skips that hold.
	if err := s.loadHistory(ctx); err != nil {
		return nil, err
	}

	if latest := s.hist.closed[r.Key]; latest != nil && latest.ID != r.ID {
		return nil, fmt.Errorf("%w: rollout %s followed for the same cause, retry the hosts there",
			ErrInvalidAction, latest.ID)
	}

	open, err := s.opts.Store.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list open rollouts: %w", err)
	}

	for _, o := range open {
		if o.Key == r.Key && !o.State.Terminal() {
			return nil, fmt.Errorf("%w: rollout %s is open for the same cause, retry the hosts there",
				ErrInvalidAction, o.ID)
		}
	}

	res.Message = fmt.Sprintf("skip lifted for %d host(s)", len(res.Hosts))

	return res, s.commitRollout(ctx, actor, "action.retry", r, map[string]any{
		keyHosts: res.Hosts, keyKey: r.Key, "skipLifted": true, keyReason: req.Reason,
	})
}

// retryAborted opens a new rollout for the key of an aborted one. Hosts
// join it as they report the need; hosts that a person skipped for the key
// stay skipped in it.
func (s *service) retryAborted(
	ctx context.Context, actor string, aborted *fleet.Rollout, reason string,
) (*ActionResult, error) {
	if aborted.Kind == fleet.RolloutManual {
		return nil, fmt.Errorf("%w: an aborted manual rollout cannot be retried, request it again",
			ErrInvalidAction)
	}

	open, err := s.opts.Store.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list open rollouts: %w", err)
	}

	for _, r := range open {
		if r.Key == aborted.Key && !r.State.Terminal() {
			return nil, fmt.Errorf("%w: rollout %s is already open for the same cause",
				ErrInvalidAction, r.ID)
		}
	}

	now := s.opts.Now()
	r := &fleet.Rollout{
		ID:        s.opts.NewID(),
		Kind:      aborted.Kind,
		Key:       aborted.Key,
		Title:     aborted.Title,
		Image:     aborted.Image,
		Digest:    aborted.Digest,
		Revision:  aborted.Revision,
		Packages:  slices.Clone(aborted.Packages),
		State:     fleet.RolloutActive,
		CreatedBy: actorName(actor),
		CreatedAt: now,
	}

	// The skips that hold for the key move into the new rollout right away:
	// it may close again before a reconcile has seen it.
	if err := s.loadHistory(ctx); err != nil {
		return nil, err
	}

	if latest := s.hist.closed[r.Key]; latest != nil {
		for i := range latest.Hosts {
			if m := latest.Hosts[i]; operatorSkipped(&m) {
				m.LeaseID = ""
				m.UpdatedAt = now
				r.Hosts = append(r.Hosts, m)
			}
		}
	}

	res := &ActionResult{Rollout: r.ID, Message: "cause reopened as rollout " + r.ID}

	return res, s.commitRollout(ctx, actor, "action.retry", r, map[string]any{
		"aborted": aborted.ID, keyKey: r.Key, keyReason: reason,
	})
}

// Abort closes a rollout. Open leases finish.
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

	pending, err := s.opts.Store.ListDirectives(ctx, store.DirectiveFilter{PendingOnly: true})
	if err != nil {
		return nil, fmt.Errorf("scheduler: list directives: %w", err)
	}

	now := s.opts.Now()
	before := r.State
	r.State = fleet.RolloutAborted
	r.Reason = cmpOr(req.Reason, "aborted")
	r.EndedAt = &now
	r.UpdatedAt = now

	changes := &store.Changes{
		Rollouts: []*fleet.Rollout{r},
		Audit: []fleet.AuditEntry{s.entry(actor, "action.abort", r.ID, map[string]any{
			keyBefore: map[string]any{keyState: before},
			keyAfter:  map[string]any{keyState: r.State}, keyReason: req.Reason,
		})},
	}

	for _, d := range pending {
		if d.RolloutID == r.ID && d.State == fleet.DirectivePending {
			d.State = fleet.DirectiveCancelled
			d.UpdatedAt = now
			changes.Directives = append(changes.Directives, d)
		}
	}

	if err := s.commit(ctx, changes); err != nil {
		return nil, err
	}

	return &ActionResult{Rollout: r.ID, Message: "rollout aborted"}, nil
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
			return l.Host == h.Name && !l.State.Terminal()
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
		if d.Host == name && d.State == fleet.DirectivePending {
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
