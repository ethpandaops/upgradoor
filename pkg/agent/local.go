package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The local socket is how a person on the host talks to the running agent:
// the 'upgradoor host' commands. It is a Unix socket for root and one group
// (the entry user's), and it can do what that person could do anyway with
// SSH and the overrides file, plus ask for a roll: list the managed
// containers, set and clear an override (the agent writes the file), set and
// release the local hold, request a roll and follow it.
//
// Nothing that comes through the socket acts by itself. An override and a
// roll become needs, served under a lease of the controller like any other,
// within the verbs the configuration allows and never under a hold.

// Paths of the local API.
const (
	LocalPathStatus    = "/v1/status"
	LocalPathOverrides = "/v1/overrides"
	LocalPathHold      = "/v1/hold"
	LocalPathRolls     = "/v1/rolls"
	LocalPathInit      = "/v1/init-overrides"

	maxLocalBody   = 64 << 10
	maxLocalRolls  = 32
	localRollStale = time.Hour
)

// LocalStatus is the answer of GET /v1/status.
type LocalStatus struct {
	Host    string `json:"host"`
	Network string `json:"network"`
	Version string `json:"version"`
	// At is when the agent last looked at the host.
	At    time.Time       `json:"at"`
	Verbs []protocol.Verb `json:"verbs"`
	Hold  protocol.Hold   `json:"hold"`
	// Containers are the managed containers.
	Containers []LocalContainer `json:"containers"`
	Needs      []protocol.Need  `json:"needs"`
	// Queue is the controller's last word on why the host waits.
	Queue *protocol.Queue `json:"queue,omitempty"`
	// Lease and Operation are the lease the agent holds and its work on it.
	Lease     string              `json:"lease,omitempty"`
	Operation *protocol.Operation `json:"operation,omitempty"`
	// Rolls are the rolls requested through the socket, oldest first.
	Rolls []LocalRoll `json:"rolls"`
	// OverridesFile is the overrides file; OverridesError says why it cannot
	// be read.
	OverridesFile  string `json:"overridesFile"`
	OverridesError string `json:"overridesError,omitempty"`
	// SyncError is set while the controller cannot be reached.
	SyncError string `json:"syncError,omitempty"`
}

// LocalContainer is one managed container in the local status.
type LocalContainer struct {
	protocol.Container

	// BaseCommand is the command line without the override's flags, when an
	// override changed it.
	BaseCommand []string `json:"baseCommand,omitempty"`
	// Target is the image the container is to run, when that is not the one
	// it runs.
	Target string `json:"target,omitempty"`
}

// LocalRoll is one roll requested through the socket, and how far it got.
type LocalRoll struct {
	ID          string    `json:"id"`
	Container   string    `json:"container"`
	RequestedBy string    `json:"requestedBy"`
	RequestedAt time.Time `json:"requestedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// State is one of the Roll* states.
	State string `json:"state"`
	// Detail says more: the controller's waiting reason, the error.
	Detail string `json:"detail,omitempty"`
	// Position is the host's place in the controller's queue while queued.
	Position int    `json:"position,omitempty"`
	Lease    string `json:"lease,omitempty"`

	// seenLease is true once the roll's need was part of a lease.
	seenLease bool
}

// States of a local roll. RollDone, RollFailed, RollRejected and
// RollSuperseded are final.
const (
	// RollResolving: the registry is asked and the image pulled.
	RollResolving = "resolving"
	// RollQueued: the need is reported; the host waits for a slot.
	RollQueued = "queued"
	// RollGranted: the controller granted a lease.
	RollGranted = "granted"
	// RollRecreating: the container is being recreated.
	RollRecreating = "recreating"
	// RollRecreated: the container is recreated; the agent watches that it
	// stays up.
	RollRecreated = "recreated"
	// RollVerified: the agent's check passed; the controller checks that the
	// node is back before it gives the slot away.
	RollVerified = "verified"
	// RollDone: the controller released the slot.
	RollDone = "done"
	// RollFailed: the roll failed; Detail says why.
	RollFailed = "failed"
	// RollRejected: the host's own rules refuse the roll.
	RollRejected = "rejected"
	// RollSuperseded: the roll was not needed any more, or was dropped.
	RollSuperseded = "superseded"
)

// Final reports whether the roll will not change any more.
func (r *LocalRoll) Final() bool {
	return slices.Contains([]string{RollDone, RollFailed, RollRejected, RollSuperseded}, r.State)
}

// OverrideRequest is the body of POST /v1/overrides.
type OverrideRequest struct {
	Container   string   `json:"container"`
	Image       string   `json:"image,omitempty"`
	AddFlags    []string `json:"addFlags,omitempty"`
	RemoveFlags []string `json:"removeFlags,omitempty"`
	// Until is when the override ends; absent for one without an end.
	Until  *time.Time `json:"until,omitempty"`
	Reason string     `json:"reason,omitempty"`
	// Clear removes the override instead.
	Clear bool `json:"clear,omitempty"`
}

// HoldRequest is the body of POST /v1/hold.
type HoldRequest struct {
	Reason string `json:"reason"`
	// Release lifts the local hold instead.
	Release bool `json:"release,omitempty"`
}

// RollRequest is the body of POST /v1/rolls.
type RollRequest struct {
	Container string `json:"container"`
}

// LocalAnswer is the answer to every local request that changes something.
type LocalAnswer struct {
	Message string `json:"message,omitempty"`
	// Roll is the ID of a requested roll.
	Roll  string `json:"roll,omitempty"`
	Error string `json:"error,omitempty"`
}

// localState is what the socket's handlers read and the loop writes.
type localState struct {
	mu     sync.Mutex
	status LocalStatus
	rolls  []*LocalRoll
	// requests are the rolls asked for and not yet taken up by the loop.
	requests []*LocalRoll
	nextRoll int
}

type peerKey struct{}

// localServer serves the local socket.
type localServer struct {
	log      logrus.FieldLogger
	svc      *service
	listener net.Listener
	server   *http.Server
}

// startLocal opens the local socket. An agent whose socket cannot be opened
// still does everything else: it says so and goes on.
func (s *service) startLocal(ctx context.Context) {
	path := s.cfg.Socket.Path
	log := s.log.WithFields(logrus.Fields{"component": "local", "socket": path})

	listener, err := listenLocal(ctx, &s.cfg.Socket, s.cfg.EntryUser)
	if err != nil {
		log.WithError(err).Error("cannot open the local socket; the 'upgradoor host' " +
			"commands do not work on this host")

		return
	}

	srv := &localServer{log: log, svc: s, listener: listener}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LocalPathStatus, srv.handleStatus)
	mux.HandleFunc("POST "+LocalPathOverrides, srv.handleOverride)
	mux.HandleFunc("POST "+LocalPathHold, srv.handleHold)
	mux.HandleFunc("POST "+LocalPathRolls, srv.handleRoll)
	mux.HandleFunc("POST "+LocalPathInit, srv.handleInit)

	srv.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, peerName(c))
		},
	}

	s.local = srv

	s.wg.Go(func() {
		if err := srv.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Error("the local socket stopped")
		}
	})

	s.wg.Go(func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		_ = srv.server.Shutdown(shutdown)
		_ = os.Remove(path)
	})

	log.Info("local socket open")
}

// listenLocal creates the socket with mode 0660 for root and the group.
func listenLocal(ctx context.Context, cfg *SocketConfig, entryUser string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("create the socket directory: %w", err)
	}

	// What is left of a socket of an earlier run is in the way.
	if info, err := os.Lstat(cfg.Path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(cfg.Path)
	}

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "unix", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	mode := os.FileMode(0o600)

	if gid, ok := socketGroup(cfg.Group, entryUser); ok {
		if err := os.Chown(cfg.Path, -1, gid); err == nil {
			mode = 0o660
		}
	}

	if err := os.Chmod(cfg.Path, mode); err != nil {
		_ = listener.Close()

		return nil, fmt.Errorf("set the socket mode: %w", err)
	}

	return listener, nil
}

// socketGroup is the group that may use the socket: the configured one, or
// the primary group of the entry user.
func socketGroup(name, entryUser string) (int, bool) {
	var raw string

	switch {
	case name != "":
		g, err := user.LookupGroup(name)
		if err != nil {
			return 0, false
		}

		raw = g.Gid
	default:
		u, err := user.Lookup(entryUser)
		if err != nil {
			return 0, false
		}

		raw = u.Gid
	}

	gid, err := strconv.Atoi(raw)

	return gid, err == nil
}

// peerName is "local:<user>" for the Unix user at the other end of a
// connection of the socket.
func peerName(c net.Conn) string {
	uid, ok := peerUID(c)
	if !ok {
		return auditLocal + ":unknown"
	}

	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return auditLocal + ":" + u.Username
	}

	return auditLocal + ":uid " + strconv.Itoa(uid)
}

func (l *localServer) who(r *http.Request) *origin {
	name, _ := r.Context().Value(peerKey{}).(string)
	if name == "" {
		name = auditLocal + ":unknown"
	}

	return &origin{source: auditLocal, user: name}
}

func (l *localServer) answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		l.log.WithError(err).Debug("cannot write a local answer")
	}
}

func (l *localServer) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, errRefused) {
		status = http.StatusConflict
	}

	l.answer(w, status, LocalAnswer{Error: strings.TrimPrefix(err.Error(), errRefused.Error()+": ")})
}

func (l *localServer) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLocalBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		l.answer(w, http.StatusBadRequest, LocalAnswer{Error: "invalid request: " + err.Error()})

		return false
	}

	return true
}

func (l *localServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	l.answer(w, http.StatusOK, l.svc.localStatus())
}

func (l *localServer) handleOverride(w http.ResponseWriter, r *http.Request) {
	var req OverrideRequest
	if !l.decode(w, r, &req) {
		return
	}

	who := l.who(r)

	var entry *Override
	if !req.Clear {
		entry = &Override{
			Image: strings.TrimSpace(req.Image), Add: req.AddFlags, Remove: req.RemoveFlags,
			By: strings.TrimPrefix(who.user, auditLocal+":"), Reason: firstLine(req.Reason), Until: req.Until,
		}
	}

	if err := l.svc.setOverride(who, req.Container, entry, firstLine(req.Reason)); err != nil {
		l.fail(w, err)

		return
	}

	msg := "override of " + req.Container + " cleared; the container is put back under the next lease"
	if entry != nil {
		msg = "override of " + req.Container + " set (" + entry.describe() +
			"); the container follows under the next lease"
	}

	l.answer(w, http.StatusOK, LocalAnswer{Message: msg})
}

func (l *localServer) handleHold(w http.ResponseWriter, r *http.Request) {
	var req HoldRequest
	if !l.decode(w, r, &req) {
		return
	}

	who := l.who(r)
	rec := AuditRecord{
		Action: "hold.set", Source: auditLocal, User: who.user, Reason: firstLine(req.Reason),
		Result: auditAccepted,
	}

	var err error

	if req.Release {
		rec.Action = "hold.release"
		err = l.svc.holds.ReleaseLocal()
	} else {
		reason := firstLine(req.Reason)
		if reason == "" {
			reason = "held"
		}

		err = l.svc.holds.SetLocal(reason + " (" + strings.TrimPrefix(who.user, auditLocal+":") + ")")
	}

	if err != nil {
		rec.Result, rec.Error = "failed", err.Error()
		l.svc.audit.record(rec)
		l.fail(w, err)

		return
	}

	l.svc.audit.record(rec)
	l.svc.requestSync()

	msg := "host held: the agent reports no needs and refuses all work until it is released"
	if req.Release {
		msg = "local hold released"
	}

	l.answer(w, http.StatusOK, LocalAnswer{Message: msg})
}

func (l *localServer) handleRoll(w http.ResponseWriter, r *http.Request) {
	var req RollRequest
	if !l.decode(w, r, &req) {
		return
	}

	who := l.who(r)

	roll, err := l.svc.requestLocalRoll(who, req.Container)
	if err != nil {
		l.fail(w, err)

		return
	}

	l.answer(w, http.StatusOK, LocalAnswer{
		Roll: roll.ID, Message: "roll of " + req.Container + " requested",
	})
}

func (l *localServer) handleInit(w http.ResponseWriter, r *http.Request) {
	status := l.svc.localStatus()
	containers := make([]GeneratedContainer, 0, len(l.svc.cfg.Containers))

	for i := range l.svc.cfg.Containers {
		m := l.svc.cfg.current(&l.svc.cfg.Containers[i])
		g := GeneratedContainer{Name: m.Name, Image: m.Image}

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

	if err := l.svc.initOverrides(containers); err != nil {
		l.fail(w, err)

		return
	}

	l.svc.audit.record(AuditRecord{
		Action: "overrides.init", Source: auditLocal, User: l.who(r).user, Result: auditAccepted,
	})
	l.answer(w, http.StatusOK, LocalAnswer{
		Message: "generated block written to " + l.svc.cfg.OverridesFile,
	})
}

// localStatus returns a copy of what the loop last published.
func (s *service) localStatus() LocalStatus {
	s.localState.mu.Lock()
	defer s.localState.mu.Unlock()

	out := s.localState.status
	out.Containers = slices.Clone(out.Containers)
	out.Needs = cloneNeeds(out.Needs)
	out.Rolls = make([]LocalRoll, 0, len(s.localState.rolls)+len(s.localState.requests))

	for _, r := range slices.Concat(s.localState.rolls, s.localState.requests) {
		out.Rolls = append(out.Rolls, *r)
	}

	return out
}

// publishLocal makes what a cycle saw available to the socket.
func (s *service) publishLocal(obs *observation, req *protocol.SyncRequest, st *persistedState) {
	status := LocalStatus{
		Host: s.host, Network: s.network, Version: s.version, At: s.now().UTC(),
		Verbs: slices.Clone(s.cfg.Verbs), Hold: obs.hold,
		Containers:    make([]LocalContainer, 0, len(s.cfg.Containers)),
		Needs:         cloneNeeds(req.Needs),
		Rolls:         []LocalRoll{},
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

		lc := LocalContainer{Container: *c}

		if t, ok := obs.targets[c.Name]; ok {
			if c.Base != nil && c.Base.HasCommand && !slices.Equal(c.Base.Command, c.Command) {
				lc.BaseCommand = nonNilStrings(c.Base.Command)
			}

			if !t.inSync {
				lc.Target = t.image
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
func (s *service) requestLocalRoll(who *origin, container string) (*LocalRoll, error) {
	now := s.now().UTC()
	rec := AuditRecord{
		Action: "roll.request", Source: auditLocal, User: who.user, Container: container,
		Result: auditAccepted,
	}

	refuse := func(err error) (*LocalRoll, error) {
		rec.Result, rec.Error = auditRefused, err.Error()
		s.audit.record(rec)

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
	roll := &LocalRoll{
		ID:        fmt.Sprintf("%s-%d", now.Format("20060102T150405"), s.localState.nextRoll),
		Container: container, RequestedBy: who.user, RequestedAt: now, UpdatedAt: now,
		State: RollResolving, Detail: "asking the registry and pulling the image",
	}
	s.localState.requests = append(s.localState.requests, roll)
	out := *roll

	s.localState.mu.Unlock()

	s.audit.record(rec)
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
			s.setLocalRoll(roll.ID, RollRejected, reason)

			continue
		}

		c := obs.container(roll.Container)
		key := NormalizeReference(c.Image)

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
		func(r *LocalRoll) bool { return !r.Final() })
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
		s.setLocalRoll(id, RollRejected, reject)
		s.audit.record(AuditRecord{
			Action: "roll.result", Source: auditAgent, Result: auditRefused, Error: reject,
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

	s.setLocalRoll(id, RollQueued, "waiting for the controller")

	return true
}

// followLocalRolls brings the rolls up to date with the lease, the
// operation and the controller's last answer.
func (s *service) followLocalRolls(st *persistedState, resp *protocol.SyncResponse) {
	s.localState.mu.Lock()
	defer s.localState.mu.Unlock()

	now := s.now().UTC()

	for _, r := range s.localState.rolls {
		if r.Final() || r.State == RollResolving {
			continue
		}

		state, detail := s.localRollState(r, st, resp)
		if state != r.State || detail != r.Detail {
			r.State, r.Detail, r.UpdatedAt = state, detail, now
		}

		r.Position = 0
		if r.State == RollQueued && resp != nil && resp.Queue != nil {
			r.Position = resp.Queue.Position
		}
	}

	// The list stays short: the oldest finished rolls go.
	s.localState.rolls = slices.DeleteFunc(s.localState.rolls, func(r *LocalRoll) bool {
		return r.Final() && now.Sub(r.UpdatedAt) > localRollStale
	})

	for len(s.localState.rolls) > maxLocalRolls {
		s.localState.rolls = s.localState.rolls[1:]
	}
}

// localRollState says where a roll stands.
func (s *service) localRollState(
	r *LocalRoll, st *persistedState, resp *protocol.SyncResponse,
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
		return RollQueued, cmpOr(holdReasonIfHeld(s.holds.Read()), "waiting for the controller")
	case !r.seenLease:
		return RollSuperseded, "the roll was dropped: the container no longer qualifies"
	case resp != nil && resp.Ended != nil && resp.Ended.ID == r.Lease && resp.Ended.State != "released":
		return RollFailed, cmpOr(resp.Ended.Reason, "the controller ended the lease as "+resp.Ended.State)
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
		return RollFailed, cmpOr(step.Error, op.Error)
	case !cur.Started:
		return RollGranted, "lease " + cur.Lease.ID + " granted"
	default:
		return RollRecreating, "lease " + cur.Lease.ID + ": " + string(op.Phase)
	}
}
