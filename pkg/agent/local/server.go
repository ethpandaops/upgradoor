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

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

// Server serves the CLI socket; the caller is identified by SO_PEERCRED as
// "local:<user>".
type Server interface {
	// Start creates the socket (0660 for its group, 0600 when the group is
	// unknown) and serves it until ctx ends or Stop.
	Start(ctx context.Context) error
	// Stop closes the socket, lets requests under way finish briefly and
	// removes the file. It is safe after a failed Start.
	Stop() error
}

const (
	maxBody = 64 << 10
	// shutdownTimeout is what requests under way get at Stop.
	shutdownTimeout = 5 * time.Second
	// Timeouts of one request.
	readHeaderTimeout = 10 * time.Second
	requestTimeout    = 30 * time.Second
	// socketDirMode is the mode of a socket directory the agent creates.
	socketDirMode = 0o755
	// socketMode is for root and the group; rootOnlyMode when the group is
	// unknown.
	socketMode   = 0o660
	rootOnlyMode = 0o600
)

// peerKey is the context key of the caller's name.
type peerKey struct{}

type server struct {
	log   logrus.FieldLogger
	cfg   *Config
	agent Agent

	mu   sync.Mutex
	http *http.Server
	stop func() bool
	wg   sync.WaitGroup
}

var _ Server = (*server)(nil)

// NewServer returns the server of the configured socket.
func NewServer(log logrus.FieldLogger, cfg *Config, agent Agent) Server {
	return &server{
		log: log.WithFields(logrus.Fields{"component": "local", "socket": cfg.Socket.Path}),
		cfg: cfg, agent: agent,
	}
}

// Start implements Server.
func (s *server) Start(ctx context.Context) error {
	listener, err := s.listen(ctx)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, s.handleStatus)
	mux.HandleFunc("POST "+PathOverrides, s.handleOverride)
	mux.HandleFunc("POST "+PathHold, s.handleHold)
	mux.HandleFunc("POST "+PathRolls, s.handleRoll)
	mux.HandleFunc("POST "+PathInit, s.handleInit)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       requestTimeout,
		WriteTimeout:      requestTimeout,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, peerName(c))
		},
	}

	s.mu.Lock()
	s.http = srv
	s.stop = context.AfterFunc(ctx, func() { _ = srv.Close() })
	s.mu.Unlock()

	s.wg.Go(func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.WithError(err).Error("the local socket stopped")
		}
	})

	s.log.Info("local socket open")

	return nil
}

// Stop implements Server.
func (s *server) Stop() error {
	s.mu.Lock()
	srv, stop := s.http, s.stop
	s.http = nil
	s.mu.Unlock()

	if srv == nil {
		return nil
	}

	stop()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	_ = srv.Shutdown(ctx)
	_ = os.Remove(s.cfg.Socket.Path)

	s.wg.Wait()

	return nil
}

func (s *server) listen(ctx context.Context) (net.Listener, error) {
	path := s.cfg.Socket.Path

	if err := os.MkdirAll(filepath.Dir(path), socketDirMode); err != nil {
		return nil, fmt.Errorf("create the socket directory: %w", err)
	}

	// Only a stale socket is removed, nothing else at that path.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	mode := os.FileMode(rootOnlyMode)

	if gid, ok := socketGroup(s.cfg.Socket.Group, s.cfg.EntryUser); ok {
		if err := os.Chown(path, -1, gid); err == nil {
			mode = socketMode
		}
	}

	if err := os.Chmod(path, mode); err != nil {
		_ = listener.Close()

		return nil, fmt.Errorf("set the socket mode: %w", err)
	}

	return listener, nil
}

// socketGroup is the configured group, or the entry user's primary group.
func socketGroup(name, entryUser string) (int, bool) {
	var raw string

	if name != "" {
		g, err := user.LookupGroup(name)
		if err != nil {
			return 0, false
		}

		raw = g.Gid
	} else {
		u, err := user.Lookup(entryUser)
		if err != nil {
			return 0, false
		}

		raw = u.Gid
	}

	gid, err := strconv.Atoi(raw)

	return gid, err == nil
}

// peerName is "local:<user>" for the kernel-reported peer of a connection.
func peerName(c net.Conn) string {
	uid, ok := peerUID(c)
	if !ok {
		return SourceLocal + ":unknown"
	}

	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return SourceLocal + ":" + u.Username
	}

	return SourceLocal + ":uid " + strconv.Itoa(uid)
}

func who(r *http.Request) string {
	if name, _ := r.Context().Value(peerKey{}).(string); name != "" {
		return name
	}

	return SourceLocal + ":unknown"
}

func (s *server) answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.WithError(err).Debug("cannot write a local answer")
	}
}

// fail answers a refusal with 409 and anything else with 500.
func (s *server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, plan.ErrRefused) {
		status = http.StatusConflict
	}

	s.answer(w, status, Answer{Error: strings.TrimPrefix(err.Error(), plan.ErrRefused.Error()+": ")})
}

// decode reads a bounded body with unknown fields refused; false when it
// answered 400.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		s.answer(w, http.StatusBadRequest, Answer{Error: "invalid request: " + err.Error()})

		return false
	}

	return true
}

func (s *server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.answer(w, http.StatusOK, s.agent.Status())
}

func (s *server) handleOverride(w http.ResponseWriter, r *http.Request) {
	var req OverrideRequest
	if !s.decode(w, r, &req) {
		return
	}

	set, err := s.agent.Override(who(r), &req)
	if err != nil {
		s.fail(w, err)

		return
	}

	msg := "override of " + req.Container + " cleared; the container is put back under the next lease"
	if !req.Clear {
		msg = "override of " + req.Container + " set (" + set + "); the container follows under the next lease"
	}

	s.answer(w, http.StatusOK, Answer{Message: msg})
}

func (s *server) handleHold(w http.ResponseWriter, r *http.Request) {
	var req HoldRequest
	if !s.decode(w, r, &req) {
		return
	}

	if err := s.agent.Hold(who(r), &req); err != nil {
		s.fail(w, err)

		return
	}

	msg := "host held: the agent reports no needs and refuses all work until it is released"
	if req.Release {
		msg = "local hold released"
	}

	s.answer(w, http.StatusOK, Answer{Message: msg})
}

func (s *server) handleRoll(w http.ResponseWriter, r *http.Request) {
	var req RollRequest
	if !s.decode(w, r, &req) {
		return
	}

	roll, err := s.agent.Roll(who(r), req.Container)
	if err != nil {
		s.fail(w, err)

		return
	}

	s.answer(w, http.StatusOK, Answer{Roll: roll.ID, Message: "roll of " + req.Container + " requested"})
}

func (s *server) handleInit(w http.ResponseWriter, r *http.Request) {
	file, err := s.agent.InitOverrides(who(r))
	if err != nil {
		s.fail(w, err)

		return
	}

	s.answer(w, http.StatusOK, Answer{Message: "generated block written to " + file})
}
