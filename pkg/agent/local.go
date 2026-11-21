package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	maxLocalRolls  = 32
	localRollStale = time.Hour
)

// localRoll is a roll requested through the socket as the loop follows it.
type localRoll struct {
	local.Roll

	// seenLease is true once the roll's need was part of a lease.
	seenLease bool
}

// localState is what the socket's handlers read and the loop writes.
type localState struct {
	mu     sync.Mutex
	status local.Status
	rolls  []*localRoll
	// requests are the rolls asked for and not yet taken up by the loop.
	requests []*localRoll
	nextRoll int
}

// startLocal opens the local socket. An agent whose socket cannot be opened
// still does everything else: it says so and goes on.
func (s *service) startLocal(ctx context.Context) {
	socket := local.Socket{Path: s.cfg.Socket.Path, Group: s.cfg.Socket.Group, EntryUser: s.cfg.EntryUser}
	log := s.log.WithFields(logrus.Fields{"component": "local", "socket": socket.Path})
	srv := local.NewServer(log, socket, &localService{s: s})

	if err := srv.Start(ctx); err != nil {
		log.WithError(err).Error("cannot open the local socket; the 'upgradoor host' " +
			"commands do not work on this host")

		return
	}

	s.wg.Go(func() {
		<-ctx.Done()

		_ = srv.Stop()
	})

	log.Info("local socket open")
}

// localService is the agent as the local socket sees it.
type localService struct {
	s *service
}

var _ local.Service = (*localService)(nil)

func (l *localService) Status() local.Status {
	return l.s.localStatus()
}

func (l *localService) Override(user string, req *local.OverrideRequest) (string, error) {
	who := &origin{source: auditlog.SourceLocal, user: user}

	var entry *overrides.Entry
	if !req.Clear {
		entry = &overrides.Entry{
			Image: strings.TrimSpace(req.Image), Add: req.AddFlags, Remove: req.RemoveFlags,
			By: strings.TrimPrefix(user, auditlog.SourceLocal+":"), Reason: auditlog.Line(req.Reason), Until: req.Until,
		}
	}

	if err := l.s.setOverride(who, req.Container, entry, auditlog.Line(req.Reason)); err != nil {
		return "", err
	}

	if entry == nil {
		return "", nil
	}

	return entry.Describe(), nil
}

func (l *localService) Hold(user string, req *local.HoldRequest) error {
	rec := auditlog.Record{
		Action: "hold.set", Source: auditlog.SourceLocal, User: user, Reason: auditlog.Line(req.Reason),
		Result: auditlog.Accepted,
	}

	var err error

	if req.Release {
		rec.Action = "hold.release"
		err = l.s.holds.ReleaseLocal()
	} else {
		reason := auditlog.Line(req.Reason)
		if reason == "" {
			reason = "held"
		}

		err = l.s.holds.SetLocal(reason + " (" + strings.TrimPrefix(user, auditlog.SourceLocal+":") + ")")
	}

	if err != nil {
		rec.Result, rec.Error = "failed", err.Error()
		l.s.audit.Record(rec)

		return err
	}

	l.s.audit.Record(rec)
	l.s.requestSync()

	return nil
}

func (l *localService) Roll(user, container string) (*local.Roll, error) {
	return l.s.requestLocalRoll(&origin{source: auditlog.SourceLocal, user: user}, container)
}

func (l *localService) InitOverrides(user string) (string, error) {
	s := l.s
	status := s.localStatus()
	containers := make([]overrides.GeneratedContainer, 0, len(s.cfg.Containers))

	for i := range s.cfg.Containers {
		m := s.cfg.current(&s.cfg.Containers[i])
		g := overrides.GeneratedContainer{Name: m.Name, Image: m.Image}

		for j := range status.Containers {
			if c := &status.Containers[j]; c.Name == m.Name {
				g.Command = c.Command
				if c.BaseCommand != nil {
					g.Command = c.BaseCommand
				}
			}
		}

		containers = append(containers, g)
	}

	if err := s.initOverrides(containers); err != nil {
		return "", err
	}

	s.audit.Record(auditlog.Record{
		Action: "overrides.init", Source: auditlog.SourceLocal, User: user, Result: auditlog.Accepted,
	})

	return s.cfg.OverridesFile, nil
}

// localStatus returns a copy of what the loop last published.
func (s *service) localStatus() local.Status {
	s.localState.mu.Lock()
	defer s.localState.mu.Unlock()

	out := s.localState.status
	out.Containers = slices.Clone(out.Containers)
	out.Needs = cloneNeeds(out.Needs)
	out.Rolls = make([]local.Roll, 0, len(s.localState.rolls)+len(s.localState.requests))

	for _, r := range slices.Concat(s.localState.rolls, s.localState.requests) {
		out.Rolls = append(out.Rolls, r.Roll)
	}

	return out
}

// publishLocal makes what a cycle saw available to the socket.
func (s *service) publishLocal(obs *observation, req *protocol.SyncRequest, st *persistedState) {
	status := local.Status{
		Host: s.host, Network: s.network, Version: s.version, At: s.now().UTC(),
		Verbs: slices.Clone(s.cfg.Verbs), Hold: obs.hold,
		Containers:    make([]local.Container, 0, len(s.cfg.Containers)),
		Needs:         cloneNeeds(req.Needs),
		Rolls:         []local.Roll{},
		OverridesFile: s.cfg.OverridesFile, OverridesError: s.overridesErr,
	}

	if s.lastQueue != (protocol.Queue{}) {
		q := s.lastQueue
		status.Queue = &q
	}

	if s.syncFailing {
		status.SyncError = "the controller cannot be reached; nothing is granted until it can"
	}

	if cur := st.Current; cur != nil {
		status.Lease = cur.Lease.ID
		op := cur.Operation
		status.Operation = &op
	}

	for i := range obs.containers {
		c := &obs.containers[i]
		if !c.Managed {
			continue
		}

		lc := local.Container{Container: *c}

		if t, ok := obs.targets[c.Name]; ok {
			if c.Base != nil && c.Base.HasCommand && !slices.Equal(c.Base.Command, c.Command) {
				lc.BaseCommand = append([]string{}, c.Base.Command...)
			}

			if !t.InSync {
				lc.Target = t.Image
			}
		}

		status.Containers = append(status.Containers, lc)
	}

	s.localState.mu.Lock()
	s.localState.status = status
	s.localState.mu.Unlock()
}

// requestLocalRoll takes a roll asked for through the socket. The loop
// turns it into a need: the registry is asked and the image pulled first.
func (s *service) requestLocalRoll(who *origin, container string) (*local.Roll, error) {
	now := s.now().UTC()
	rec := auditlog.Record{
		Action: "roll.request", Source: auditlog.SourceLocal, User: who.user, Container: container,
		Result: auditlog.Accepted,
	}

	refuse := func(err error) (*local.Roll, error) {
		rec.Result, rec.Error = auditlog.Refused, err.Error()
		s.audit.Record(rec)

		return nil, err
	}

	switch {
	case !s.cfg.Allows(protocol.VerbRoll):
		return refuse(refusedf("verb roll is not allowed on this host"))
	case s.cfg.Managed(container) == nil:
		return refuse(refusedf("container %s is not managed on this host", container))
	}

	if hold := s.holds.Read(); hold.Active() {
		return refuse(refusedf("%s", holdReason(hold)))
	}

	s.localState.mu.Lock()

	for _, r := range slices.Concat(s.localState.rolls, s.localState.requests) {
		if r.Container == container && !r.Final() {
			s.localState.mu.Unlock()

			return refuse(refusedf("a roll of %s is already under way (%s, %s)", container, r.ID, r.State))
		}
	}

	s.localState.nextRoll++
	roll := &localRoll{Roll: local.Roll{
		ID:        fmt.Sprintf("%s-%d", now.Format("20060102T150405"), s.localState.nextRoll),
		Container: container, RequestedBy: who.user, RequestedAt: now, UpdatedAt: now,
		State: local.RollResolving, Detail: "asking the registry and pulling the image",
	}}
	s.localState.requests = append(s.localState.requests, roll)
	out := roll.Roll

	s.localState.mu.Unlock()

	s.audit.Record(rec)
	s.requestSync()

	return &out, nil
}

// takeLocalRolls starts the rolls asked for since the last cycle: each waits
// for the registry lookup and the pull of its image like a roll directive.
func (s *service) takeLocalRolls(obs *observation) {
	s.localState.mu.Lock()
	requests := s.localState.requests
	s.localState.requests = nil
	s.localState.rolls = append(s.localState.rolls, requests...)
	s.localState.mu.Unlock()

	for _, roll := range requests {
		if reason := s.rollRefusal(roll.Container, obs); reason != "" {
			s.setLocalRoll(roll.ID, local.RollRejected, reason)

			continue
		}

		c := obs.container(roll.Container)
		key := imageref.Normalize(c.Image)

		s.pendingRolls[roll.ID] = pendingRoll{
			container: roll.Container, key: key, ticket: s.watcher.Request(key, c.Image), local: true,
		}
	}
}

// localRollsOpen reports whether a roll requested on the host is under way:
// somebody is waiting for it, so the agent syncs at the pace of a lease.
func (s *service) localRollsOpen() bool {
	s.localState.mu.Lock()
	defer s.localState.mu.Unlock()

	return slices.ContainsFunc(slices.Concat(s.localState.rolls, s.localState.requests),
		func(r *localRoll) bool { return !r.Final() })
}

// setLocalRoll moves a roll to a state.
func (s *service) setLocalRoll(id, state, detail string) {
	s.localState.mu.Lock()
	defer s.localState.mu.Unlock()

	for _, r := range s.localState.rolls {
		if r.ID == id && (r.State != state || r.Detail != detail) {
			r.State, r.Detail, r.UpdatedAt = state, detail, s.now().UTC()
		}
	}
}

// concludeLocalRoll persists the need of a local roll whose image is
// resolved and local, or rejects the roll.
func (s *service) concludeLocalRoll(id string, need *protocol.Need, reject string) bool {
	if reject != "" {
		s.setLocalRoll(id, local.RollRejected, reject)
		s.audit.Record(auditlog.Record{
			Action: "roll.result", Source: auditlog.SourceAgent, Result: auditlog.Refused, Error: reject,
			Reason: "requested on the host as " + id,
		})

		return true
	}

	need.ID, need.DirectiveID, need.Local = protocol.LocalNeedID(id), "", true

	err := s.store.Update(func(st *persistedState) {
		st.ManualNeeds = append(st.ManualNeeds, *need)
	})
	if err != nil {
		s.log.WithError(err).Error("cannot persist the need of a local roll; it is tried again")

		return false
	}

	s.setLocalRoll(id, local.RollQueued, "waiting for the controller")

	return true
}

// followLocalRolls brings the rolls up to date with the lease, the
// operation and the controller's last answer.
func (s *service) followLocalRolls(st *persistedState, resp *protocol.SyncResponse) {
	s.localState.mu.Lock()
	defer s.localState.mu.Unlock()

	now := s.now().UTC()

	for _, r := range s.localState.rolls {
		if r.Final() || r.State == local.RollResolving {
			continue
		}

		state, detail := s.localRollState(r, st, resp)
		if state != r.State || detail != r.Detail {
			r.State, r.Detail, r.UpdatedAt = state, detail, now
		}

		r.Position = 0
		if r.State == local.RollQueued && resp != nil && resp.Queue != nil {
			r.Position = resp.Queue.Position
		}
	}

	// The list stays short: the oldest finished rolls go.
	s.localState.rolls = slices.DeleteFunc(s.localState.rolls, func(r *localRoll) bool {
		return r.Final() && now.Sub(r.UpdatedAt) > localRollStale
	})

	for len(s.localState.rolls) > maxLocalRolls {
		s.localState.rolls = s.localState.rolls[1:]
	}
}

// localRollState says where a roll stands.
func (s *service) localRollState(
	r *localRoll, st *persistedState, resp *protocol.SyncResponse,
) (state, detail string) {
	needID := protocol.LocalNeedID(r.ID)

	if cur := st.Current; cur != nil {
		for _, step := range cur.Operation.Steps {
			if step.NeedID != needID {
				continue
			}

			r.seenLease = true
			r.Lease = cur.Lease.ID

			return leasedRollState(cur, &step)
		}
	}

	open := slices.ContainsFunc(st.ManualNeeds, func(n protocol.Need) bool { return n.ID == needID })

	switch {
	case open && resp != nil && resp.Queue != nil:
		return local.RollQueued, resp.Queue.Reason
	case open:
		return local.RollQueued, cmpOr(holdReasonIfHeld(s.holds.Read()), "waiting for the controller")
	case !r.seenLease:
		return local.RollSuperseded, "the roll was dropped: the container no longer qualifies"
	case resp != nil && resp.Ended != nil && resp.Ended.ID == r.Lease && resp.Ended.State != "released":
		return local.RollFailed, cmpOr(resp.Ended.Reason, "the controller ended the lease as "+resp.Ended.State)
	case r.State == local.RollVerified || (resp != nil && resp.Ended != nil && resp.Ended.ID == r.Lease):
		return local.RollDone, "the controller released the slot"
	default:
		// The operation is forgotten and it was not seen to be verified.
		return r.State, r.Detail
	}
}

func holdReasonIfHeld(h protocol.Hold) string {
	if !h.Active() {
		return ""
	}

	return holdReason(h)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

// leasedRollState is the state of a roll whose need is part of the lease the
// agent holds.
func leasedRollState(cur *operationRecord, step *protocol.Step) (state, detail string) {
	op := &cur.Operation

	switch step.Status {
	case protocol.StepFailed:
		return local.RollFailed, step.Error
	case protocol.StepRefused:
		return local.RollRejected, step.Error
	case protocol.StepSuperseded, protocol.StepCancelled:
		return local.RollSuperseded, step.Error
	case protocol.StepDone:
		switch op.Phase {
		case protocol.PhaseDone:
			return local.RollVerified, "the container stayed up; the controller now checks that the node is back"
		case protocol.PhaseFailed:
			return local.RollFailed, op.Error
		default:
			return local.RollRecreated, "watching that the container stays up"
		}
	case protocol.StepRunning:
		return local.RollRecreating, ""
	case protocol.StepPending:
	}

	switch {
	case op.Phase == protocol.PhaseFailed:
		return local.RollFailed, cmpOr(step.Error, op.Error)
	case !cur.Started:
		return local.RollGranted, "lease " + cur.Lease.ID + " granted"
	default:
		return local.RollRecreating, "lease " + cur.Lease.ID + ": " + string(op.Phase)
	}
}
