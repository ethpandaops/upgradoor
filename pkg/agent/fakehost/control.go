package fakehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// State is the answer of GET /state on the control endpoint.
type State struct {
	BootID         string                `json:"bootId"`
	RunningKernel  string                `json:"runningKernel"`
	NewestKernel   string                `json:"newestKernel"`
	RebootRequired bool                  `json:"rebootRequired"`
	Containers     []protocol.Container  `json:"containers"`
	Registry       map[string]Tag        `json:"registry"`
	Local          map[string]string     `json:"local"`
	Upgradable     []protocol.Upgradable `json:"upgradable"`
	Calls          []string              `json:"calls"`
}

// Snapshot returns the host's current state.
func (h *Host) Snapshot() State {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := State{
		BootID:         h.bootIDLocked(),
		RunningKernel:  h.runningKernel,
		NewestKernel:   h.newestKernel,
		RebootRequired: h.rebootRequired,
		Containers:     make([]protocol.Container, 0, len(h.containers)),
		Registry:       make(map[string]Tag, len(h.registry)),
		Local:          make(map[string]string, len(h.local)),
		Upgradable:     append([]protocol.Upgradable{}, h.upgradable...),
		Calls:          append([]string{}, h.calls...),
	}

	for _, c := range h.containers {
		st.Containers = append(st.Containers, *c)
	}

	for ref, tag := range h.registry {
		st.Registry[ref] = tag
	}

	for ref, digest := range h.local {
		st.Local[ref] = digest
	}

	return st
}

// ControlHandler is the control endpoint, for changing the fake while an
// agent runs against it:
//
//	GET  /state
//	POST /registry         {"ref": "...", "digest": "sha256:...", "revision": "..."}
//	POST /upgradable       [{"name": "...", "arch": "...", "current": "...", "candidate": "..."}]
//	POST /reboot-required  {"required": true}
//	POST /kernel           {"newest": "6.12.48-amd64"}
//	POST /crash            {"container": "geth"}
func (h *Host) ControlHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, h.Snapshot())
	})

	mux.HandleFunc("POST /registry", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ref string `json:"ref"`
			Tag
		}

		if !readJSON(w, r, &in) {
			return
		}

		if in.Ref == "" || in.Digest == "" {
			writeError(w, http.StatusBadRequest, errors.New("ref and digest are required"))

			return
		}

		h.SetRegistry(in.Ref, in.Digest, in.Revision)
		writeJSON(w, http.StatusOK, h.Snapshot())
	})

	mux.HandleFunc("POST /upgradable", func(w http.ResponseWriter, r *http.Request) {
		var in []protocol.Upgradable

		if !readJSON(w, r, &in) {
			return
		}

		h.SetUpgradable(in)
		writeJSON(w, http.StatusOK, h.Snapshot())
	})

	mux.HandleFunc("POST /reboot-required", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Required bool `json:"required"`
		}

		if !readJSON(w, r, &in) {
			return
		}

		h.SetRebootRequired(in.Required)
		writeJSON(w, http.StatusOK, h.Snapshot())
	})

	mux.HandleFunc("POST /kernel", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Newest string `json:"newest"`
		}

		if !readJSON(w, r, &in) {
			return
		}

		if in.Newest == "" {
			writeError(w, http.StatusBadRequest, errors.New("newest is required"))

			return
		}

		h.SetNewestKernel(in.Newest)
		writeJSON(w, http.StatusOK, h.Snapshot())
	})

	mux.HandleFunc("POST /crash", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Container string `json:"container"`
		}

		if !readJSON(w, r, &in) {
			return
		}

		if err := h.Crash(in.Container); err != nil {
			writeError(w, http.StatusNotFound, err)

			return
		}

		writeJSON(w, http.StatusOK, h.Snapshot())
	})

	return mux
}

// ServeControl serves the control endpoint on addr until ctx ends. The
// endpoint is unauthenticated, so addr must be a loopback address.
func (h *Host) ServeControl(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("fakehost: control address: %w", err)
	}

	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("fakehost: control address %q is not a loopback IP address", addr)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("fakehost: control listen: %w", err)
	}

	srv := &http.Server{Handler: h.ControlHandler(), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		defer close(done)

		<-ctx.Done()

		shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer stop()

		_ = srv.Shutdown(shutdown)
	}()

	err = srv.Serve(ln)

	cancel()
	<-done

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("fakehost: control endpoint: %w", err)
	}

	return nil
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))

		return false
	}

	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
