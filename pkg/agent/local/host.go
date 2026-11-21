package local

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/agent/needs"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	maxLocalRolls  = 32
	localRollStale = time.Hour
)

// Host is the agent's side of the host socket: what the socket shows of the
// last cycle, and the rolls requested through it, which the sync loop
// follows. It serves the socket (Service) while the agent runs.
type Host interface {
	// Start opens the socket; Stop closes it. An agent whose socket cannot be
	// opened still does everything else: Start says so and returns the error.
	Start(ctx context.Context) error
	Stop() error
	// Publish makes what a cycle saw available to the socket. queue is the
	// controller's last word on the host's place in the queue, syncFailing
	// whether the controller cannot be reached.
	Publish(obs *needs.Observation, req *protocol.SyncRequest, st *journal.State,
		queue protocol.Queue, syncFailing bool)
	// TakeRequests hands the loop the rolls asked for since the last cycle.
	TakeRequests() []Roll
	// RollsOpen reports whether a roll requested on the host is under way.
	RollsOpen() bool
	// SetRoll moves a roll to a state.
	SetRoll(id, state, detail string)
	// ConcludeRoll persists the need of a roll whose image is resolved and
	// local, or rejects the roll. It reports whether the roll is answered.
	ConcludeRoll(id string, need *protocol.Need, reject string) bool
	// Follow brings the rolls up to date with the lease, the operation and
	// the controller's last answer (nil when the sync failed).
	Follow(st *journal.State, resp *protocol.SyncResponse)
	// ManagedGroup returns the roll group of a container that the last cycle
	// saw managed; false for one it did not.
	ManagedGroup(name string) (string, bool)
}

// HostDeps are what the host side of the socket works with.
type HostDeps struct {
	Log       logrus.FieldLogger
	Config    *config.Config
	Overrides overrides.Keeper
	Store     journal.Store
	Audit     auditlog.Log
	Now       func() time.Time
	// Host, Network and Version are what the status says of the agent.
	Host, Network, Version string
	// Changed asks the sync loop for a sync now.
	Changed func()
}

// localRoll is a roll requested through the socket as the loop follows it.
type localRoll struct {
	Roll

	// seenLease is true once the roll's need was part of a lease.
	seenLease bool
}

// host is the Host, and the Service the socket serves.
type host struct {
	log       logrus.FieldLogger
	cfg       *config.Config
	overrides overrides.Keeper
	store     journal.Store
	audit     auditlog.Log
	now       func() time.Time
	host      string
	network   string
	version   string
	changed   func()
	srv       *Server

	// The fields below are what the socket's handlers read and the loop
	// writes.
	mu     sync.Mutex
	status Status
	rolls  []*localRoll
	// requests are the rolls asked for and not yet taken up by the loop.
	requests []*localRoll
	nextRoll int
}

var (
	_ Host    = (*host)(nil)
	_ Service = (*host)(nil)
)

// NewHost returns the agent's side of the host socket.
func NewHost(d *HostDeps) Host {
	return &host{
		log: d.Log, cfg: d.Config, overrides: d.Overrides, store: d.Store, audit: d.Audit, now: d.Now,
		host: d.Host, network: d.Network, version: d.Version, changed: d.Changed,
	}
}

// Start opens the local socket.
func (s *host) Start(ctx context.Context) error {
	socket := Socket{Path: s.cfg.Socket.Path, Group: s.cfg.Socket.Group, EntryUser: s.cfg.EntryUser}
	log := s.log.WithFields(logrus.Fields{"component": "local", "socket": socket.Path})
	srv := NewServer(log, socket, s)

	if err := srv.Start(ctx); err != nil {
		log.WithError(err).Error("cannot open the local socket; the 'upgradoor host' " +
			"commands do not work on this host")

		return err
	}

	s.srv = srv

	log.Info("local socket open")

	return nil
}

// Stop closes the local socket.
func (s *host) Stop() error {
	if s.srv == nil {
		return nil
	}

	return s.srv.Stop()
}

// Status returns a copy of what the loop last published.
func (s *host) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := s.status
	out.Containers = slices.Clone(out.Containers)
	out.Needs = needs.Clone(out.Needs)
	out.Rolls = make([]Roll, 0, len(s.rolls)+len(s.requests))

	for _, r := range slices.Concat(s.rolls, s.requests) {
		out.Rolls = append(out.Rolls, r.Roll)
	}

	return out
}

// Override sets or clears an override asked for on the host.
func (s *host) Override(user string, req *OverrideRequest) (string, error) {
	who := &overrides.Origin{Source: auditlog.SourceLocal, User: user}

	var entry *overrides.Entry
	if !req.Clear {
		entry = &overrides.Entry{
			Image: strings.TrimSpace(req.Image), Add: req.AddFlags, Remove: req.RemoveFlags,
			By: strings.TrimPrefix(user, auditlog.SourceLocal+":"), Reason: auditlog.Line(req.Reason), Until: req.Until,
		}
	}

	if err := s.overrides.SetOverride(who, req.Container, entry, auditlog.Line(req.Reason)); err != nil {
		return "", err
	}

	if entry == nil {
		return "", nil
	}

	return entry.Describe(), nil
}

// Hold sets or releases the local hold.
func (s *host) Hold(user string, req *HoldRequest) error {
	rec := auditlog.Record{
		Action: "hold.set", Source: auditlog.SourceLocal, User: user, Reason: auditlog.Line(req.Reason),
		Result: auditlog.Accepted,
	}

	var err error

	if req.Release {
		rec.Action = "hold.release"
		err = s.overrides.ReleaseLocalHold(user)
	} else {
		reason := auditlog.Line(req.Reason)
		if reason == "" {
			reason = "held"
		}

		err = s.overrides.SetLocalHold(strings.TrimPrefix(user, auditlog.SourceLocal+":"), reason)
	}

	if err != nil {
		rec.Result, rec.Error = "failed", err.Error()
		s.audit.Record(rec)

		return err
	}

	s.audit.Record(rec)
	s.changed()

	return nil
}

// Roll takes a roll asked for on the host.
func (s *host) Roll(user, container string) (*Roll, error) {
	return s.requestRoll(&overrides.Origin{Source: auditlog.SourceLocal, User: user}, container)
}

// InitOverrides writes the generated block of the overrides file from what
// the containers run.
func (s *host) InitOverrides(user string) (string, error) {
	status := s.Status()
	containers := make([]overrides.GeneratedContainer, 0, len(status.Containers))

	// What the containers are deployed as: their deployed entry, or what
	// they were created with before an override.
	for i := range status.Containers {
		c := &status.Containers[i]
		if !c.Managed {
			continue
		}

		g := overrides.GeneratedContainer{Name: c.Name, Image: c.ConfiguredImage, Command: c.Command}
		if c.BaseCommand != nil {
			g.Command = c.BaseCommand
		}

		containers = append(containers, g)
	}

	if err := s.overrides.Init(containers); err != nil {
		return "", err
	}

	s.audit.Record(auditlog.Record{
		Action: "overrides.init", Source: auditlog.SourceLocal, User: user, Result: auditlog.Accepted,
	})

	return s.cfg.OverridesFile, nil
}

// isManaged reports whether the last cycle saw the container managed.
func (s *host) isManaged(name string) bool {
	_, ok := s.ManagedGroup(name)

	return ok
}

// ManagedGroup returns the roll group of a container that the last cycle saw
// managed; false for one it did not.
func (s *host) ManagedGroup(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.status.Containers {
		if c := &s.status.Containers[i]; c.Name == name && c.Managed {
			return c.Group, true
		}
	}

	return "", false
}

// Publish makes what a cycle saw available to the socket.
func (s *host) Publish(
	obs *needs.Observation, req *protocol.SyncRequest, st *journal.State,
	queue protocol.Queue, syncFailing bool,
) {
	status := Status{
		Host: s.host, Network: s.network, Version: s.version, At: s.now().UTC(),
		Verbs: slices.Clone(s.cfg.Verbs), Hold: obs.Hold,
		Containers:    make([]Container, 0, len(obs.Containers)),
		Needs:         needs.Clone(req.Needs),
		Rolls:         []Roll{},
		OverridesFile: s.cfg.OverridesFile, OverridesError: s.overrides.Err(),
	}

	if queue != (protocol.Queue{}) {
		q := queue
		status.Queue = &q
	}

	if syncFailing {
		status.SyncError = "the controller cannot be reached; nothing is granted until it can"
	}

	if cur := st.Current; cur != nil {
		status.Lease = cur.Lease.ID
		op := cur.Operation
		status.Operation = &op
	}

	for i := range obs.Containers {
		c := &obs.Containers[i]
		if !c.Managed {
			continue
		}

		lc := Container{Container: *c, Unlabelled: c.Unlabelled}

		if t, ok := obs.Targets[c.Name]; ok {
			if c.Base != nil && c.Base.HasCommand && !slices.Equal(c.Base.Command, c.Command) {
				lc.BaseCommand = append([]string{}, c.Base.Command...)
			}

			if !t.InSync {
				lc.Target = t.Image
			}
		}

		status.Containers = append(status.Containers, lc)
	}

	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// requestRoll takes a roll asked for through the socket. The loop turns it
// into a need: the registry is asked and the image pulled first.
func (s *host) requestRoll(who *overrides.Origin, container string) (*Roll, error) {
	now := s.now().UTC()
	rec := auditlog.Record{
		Action: "roll.request", Source: auditlog.SourceLocal, User: who.User, Container: container,
		Result: auditlog.Accepted,
	}

	refuse := func(err error) (*Roll, error) {
		rec.Result, rec.Error = auditlog.Refused, err.Error()
		s.audit.Record(rec)

		return nil, err
	}

	switch {
	case !s.cfg.Allows(protocol.VerbRoll):
		return refuse(auditlog.Refusal("verb roll is not allowed on this host"))
	case !s.isManaged(container):
		return refuse(auditlog.Refusal("container %s is not managed on this host", container))
	}

	if hold := s.overrides.Hold(); hold.Active() {
		return refuse(auditlog.Refusal("%s", overrides.HoldReason(hold)))
	}

	s.mu.Lock()

	for _, r := range slices.Concat(s.rolls, s.requests) {
		if r.Container == container && !r.Final() {
			s.mu.Unlock()

			return refuse(auditlog.Refusal("a roll of %s is already under way (%s, %s)", container, r.ID, r.State))
		}
	}

	s.nextRoll++
	roll := &localRoll{Roll: Roll{
		ID:        fmt.Sprintf("%s-%d", now.Format("20060102T150405"), s.nextRoll),
		Container: container, RequestedBy: who.User, RequestedAt: now, UpdatedAt: now,
		State: RollResolving, Detail: "asking the registry and pulling the image",
	}}
	s.requests = append(s.requests, roll)
	out := roll.Roll

	s.mu.Unlock()

	s.audit.Record(rec)
	s.changed()

	return &out, nil
}

// TakeRequests hands the loop the rolls asked for since the last cycle; it
// follows them from now on.
func (s *host) TakeRequests() []Roll {
	s.mu.Lock()
	defer s.mu.Unlock()

	requests := s.requests
	s.requests = nil
	s.rolls = append(s.rolls, requests...)

	out := make([]Roll, 0, len(requests))
	for _, r := range requests {
		out = append(out, r.Roll)
	}

	return out
}

// RollsOpen reports whether a roll requested on the host is under way:
// somebody is waiting for it, so the agent syncs at the pace of a lease.
func (s *host) RollsOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.ContainsFunc(slices.Concat(s.rolls, s.requests),
		func(r *localRoll) bool { return !r.Final() })
}

// SetRoll moves a roll to a state.
func (s *host) SetRoll(id, state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.rolls {
		if r.ID == id && (r.State != state || r.Detail != detail) {
			r.State, r.Detail, r.UpdatedAt = state, detail, s.now().UTC()
		}
	}
}

// ConcludeRoll persists the need of a local roll whose image is resolved and
// local, or rejects the roll.
func (s *host) ConcludeRoll(id string, need *protocol.Need, reject string) bool {
	if reject != "" {
		s.SetRoll(id, RollRejected, reject)
		s.audit.Record(auditlog.Record{
			Action: "roll.result", Source: auditlog.SourceAgent, Result: auditlog.Refused, Error: reject,
			Reason: "requested on the host as " + id,
		})

		return true
	}

	need.ID, need.DirectiveID, need.Local = protocol.LocalNeedID(id), "", true

	err := s.store.Update(func(st *journal.State) {
		st.ManualNeeds = append(st.ManualNeeds, *need)
	})
	if err != nil {
		s.log.WithError(err).Error("cannot persist the need of a local roll; it is tried again")

		return false
	}

	s.SetRoll(id, RollQueued, "waiting for the controller")

	return true
}

// Follow brings the rolls up to date with the lease, the operation and the
// controller's last answer.
func (s *host) Follow(st *journal.State, resp *protocol.SyncResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()

	for _, r := range s.rolls {
		if r.Final() || r.State == RollResolving {
			continue
		}

		state, detail := s.rollState(r, st, resp)
		if state != r.State || detail != r.Detail {
			r.State, r.Detail, r.UpdatedAt = state, detail, now
		}

		r.Position = 0
		if r.State == RollQueued && resp != nil && resp.Queue != nil {
			r.Position = resp.Queue.Position
		}
	}

	// The list stays short: the oldest finished rolls go.
	s.rolls = slices.DeleteFunc(s.rolls, func(r *localRoll) bool {
		return r.Final() && now.Sub(r.UpdatedAt) > localRollStale
	})

	for len(s.rolls) > maxLocalRolls {
		s.rolls = s.rolls[1:]
	}
}

// localRollState says where a roll stands.
func (s *host) rollState(
	r *localRoll, st *journal.State, resp *protocol.SyncResponse,
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
		return RollQueued, resp.Queue.Reason
	case open:
		return RollQueued, cmp.Or(holdReasonIfHeld(s.overrides.Hold()), "waiting for the controller")
	case !r.seenLease:
		return RollSuperseded, "the roll was dropped: the container no longer qualifies"
	case resp != nil && resp.Ended != nil && resp.Ended.ID == r.Lease && resp.Ended.State != "released":
		return RollFailed, cmp.Or(resp.Ended.Reason, "the controller ended the lease as "+resp.Ended.State)
	case r.State == RollVerified || (resp != nil && resp.Ended != nil && resp.Ended.ID == r.Lease):
		return RollDone, "the controller released the slot"
	default:
		// The operation is forgotten and it was not seen to be verified.
		return r.State, r.Detail
	}
}

func holdReasonIfHeld(h protocol.Hold) string {
	if !h.Active() {
		return ""
	}

	return overrides.HoldReason(h)
}

// leasedRollState is the state of a roll whose need is part of the lease the
// agent holds.
func leasedRollState(cur *journal.Record, step *protocol.Step) (state, detail string) {
	op := &cur.Operation

	switch step.Status {
	case protocol.StepFailed:
		return RollFailed, step.Error
	case protocol.StepRefused:
		return RollRejected, step.Error
	case protocol.StepSuperseded, protocol.StepCancelled:
		return RollSuperseded, step.Error
	case protocol.StepDone:
		switch op.Phase {
		case protocol.PhaseDone:
			return RollVerified, "the container stayed up; the controller now checks that the node is back"
		case protocol.PhaseFailed:
			return RollFailed, op.Error
		default:
			return RollRecreated, "watching that the container stays up"
		}
	case protocol.StepRunning:
		return RollRecreating, ""
	case protocol.StepPending:
	}

	switch {
	case op.Phase == protocol.PhaseFailed:
		return RollFailed, cmp.Or(step.Error, op.Error)
	case !cur.Started:
		return RollGranted, "lease " + cur.Lease.ID + " granted"
	default:
		return RollRecreating, "lease " + cur.Lease.ID + ": " + string(op.Phase)
	}
}
