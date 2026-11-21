package local

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
)

const (
	maxBody         = 64 << 10
	shutdownTimeout = 5 * time.Second
)

// Socket is where the socket is and who may use it.
type Socket struct {
	// Path is the socket file.
	Path string
	// Group may use the socket; empty is the primary group of EntryUser.
	Group     string
	EntryUser string
}

type peerKey struct{}

// Server serves the local socket.
type Server struct {
	log    logrus.FieldLogger
	svc    Service
	socket Socket
	server *http.Server
	wg     sync.WaitGroup
}

// NewServer returns a server for socket that answers from svc.
func NewServer(log logrus.FieldLogger, socket Socket, svc Service) *Server {
	return &Server{log: log, svc: svc, socket: socket}
}

// Start opens the socket and serves it until Stop.
func (s *Server) Start(ctx context.Context) error {
	listener, err := listen(ctx, &s.socket)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, s.handleStatus)
	mux.HandleFunc("POST "+PathOverrides, s.handleOverride)
	mux.HandleFunc("POST "+PathHold, s.handleHold)
	mux.HandleFunc("POST "+PathRolls, s.handleRoll)
	mux.HandleFunc("POST "+PathInit, s.handleInit)

	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, peerName(c))
		},
	}

	s.wg.Go(func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.WithError(err).Error("the local socket stopped")
		}
	})

	return nil
}

// Stop closes the socket, gives the requests under way a moment to finish,
// and removes the socket file.
func (s *Server) Stop() error {
	if s.server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	_ = s.server.Shutdown(ctx)
	_ = os.Remove(s.socket.Path)

	s.wg.Wait()

	return nil
}

// listen creates the socket with mode 0660 for root and the group.
func listen(ctx context.Context, socket *Socket) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket.Path), 0o755); err != nil {
		return nil, fmt.Errorf("create the socket directory: %w", err)
	}

	// What is left of a socket of an earlier run is in the way.
	if info, err := os.Lstat(socket.Path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(socket.Path)
	}

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "unix", socket.Path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	mode := os.FileMode(0o600)

	if gid, ok := socketGroup(socket.Group, socket.EntryUser); ok {
		if err := os.Chown(socket.Path, -1, gid); err == nil {
			mode = 0o660
		}
	}

	if err := os.Chmod(socket.Path, mode); err != nil {
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
		return auditlog.SourceLocal + ":unknown"
	}

	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return auditlog.SourceLocal + ":" + u.Username
	}

	return auditlog.SourceLocal + ":uid " + strconv.Itoa(uid)
}

func (s *Server) who(r *http.Request) string {
	name, _ := r.Context().Value(peerKey{}).(string)
	if name == "" {
		name = auditlog.SourceLocal + ":unknown"
	}

	return name
}

func (s *Server) answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.WithError(err).Debug("cannot write a local answer")
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, auditlog.ErrRefused) {
		status = http.StatusConflict
	}

	s.answer(w, status, Answer{Error: strings.TrimPrefix(err.Error(), auditlog.ErrRefused.Error()+": ")})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		s.answer(w, http.StatusBadRequest, Answer{Error: "invalid request: " + err.Error()})

		return false
	}

	return true
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.answer(w, http.StatusOK, s.svc.Status())
}

func (s *Server) handleOverride(w http.ResponseWriter, r *http.Request) {
	var req OverrideRequest
	if !s.decode(w, r, &req) {
		return
	}

	set, err := s.svc.Override(s.who(r), &req)
	if err != nil {
		s.fail(w, err)

		return
	}

	msg := "override of " + req.Container + " cleared; the container is put back under the next lease"
	if !req.Clear {
		msg = "override of " + req.Container + " set (" + set +
			"); the container follows under the next lease"
	}

	s.answer(w, http.StatusOK, Answer{Message: msg})
}

func (s *Server) handleHold(w http.ResponseWriter, r *http.Request) {
	var req HoldRequest
	if !s.decode(w, r, &req) {
		return
	}

	if err := s.svc.Hold(s.who(r), &req); err != nil {
		s.fail(w, err)

		return
	}

	msg := "host held: the agent reports no needs and refuses all work until it is released"
	if req.Release {
		msg = "local hold released"
	}

	s.answer(w, http.StatusOK, Answer{Message: msg})
}

func (s *Server) handleRoll(w http.ResponseWriter, r *http.Request) {
	var req RollRequest
	if !s.decode(w, r, &req) {
		return
	}

	roll, err := s.svc.Roll(s.who(r), req.Container)
	if err != nil {
		s.fail(w, err)

		return
	}

	s.answer(w, http.StatusOK, Answer{
		Roll: roll.ID, Message: "roll of " + req.Container + " requested",
	})
}

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	file, err := s.svc.InitOverrides(s.who(r))
	if err != nil {
		s.fail(w, err)

		return
	}

	s.answer(w, http.StatusOK, Answer{Message: "generated block written to " + file})
}
