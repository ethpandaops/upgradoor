package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// maxBody bounds a request body of the control endpoint.
const maxBody = 64 << 10

// state is the answer of GET /state and of every change.
type state struct {
	BootID         string                `json:"bootId"`
	RunningKernel  string                `json:"runningKernel"`
	NewestKernel   string                `json:"newestKernel"`
	RebootRequired bool                  `json:"rebootRequired"`
	Containers     []protocol.Container  `json:"containers"`
	Upgradable     []protocol.Upgradable `json:"upgradable"`
	Calls          []string              `json:"calls"`
}

// control is the unauthenticated control endpoint of the in-memory host, on
// a loopback address only:
//
//	GET  /state
//	POST /reboot-required  {"required": true}
//	POST /crash            {"container": "geth"}
type control struct {
	host *agenttest.Host
	ln   net.Listener
}

// newControl listens on addr, which must be a loopback IP address and port.
func newControl(host *agenttest.Host, addr string) (*control, error) {
	ip, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("control address: %w", err)
	}

	if parsed := net.ParseIP(ip); parsed == nil || !parsed.IsLoopback() {
		return nil, fmt.Errorf("control address %q is not a loopback IP address", addr)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("control listen: %w", err)
	}

	return &control{host: host, ln: ln}, nil
}

// serve answers requests until ctx ends.
func (c *control) serve(ctx context.Context) error {
	srv := &http.Server{Handler: c.handler(), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})

	go func() {
		defer close(done)

		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdown)
	}()

	err := srv.Serve(c.ln)

	<-done

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("control endpoint: %w", err)
	}

	return nil
}

func (c *control) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		c.writeState(w, r)
	})

	mux.HandleFunc("POST /reboot-required", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Required bool `json:"required"`
		}

		if !readJSON(w, r, &in) {
			return
		}

		c.host.SetRebootRequired(in.Required)
		c.writeState(w, r)
	})

	mux.HandleFunc("POST /crash", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Container string `json:"container"`
		}

		if !readJSON(w, r, &in) {
			return
		}

		if err := c.host.Crash(in.Container); err != nil {
			writeError(w, http.StatusNotFound, err)

			return
		}

		c.writeState(w, r)
	})

	return mux
}

// writeState answers with what the host holds now.
func (c *control) writeState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	containers, err := c.host.Containers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)

		return
	}

	sys, err := c.host.System(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)

		return
	}

	inv, err := c.host.Inventory(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)

		return
	}

	writeJSON(w, http.StatusOK, state{
		BootID:         sys.BootID,
		RunningKernel:  sys.OS.RunningKernel,
		NewestKernel:   sys.OS.NewestKernel,
		RebootRequired: sys.OS.RebootRequired,
		Containers:     containers,
		Upgradable:     inv.Upgradable,
		Calls:          c.host.Calls(),
	})
}

// readJSON decodes a bounded body with unknown fields refused; on failure it
// has answered 400.
func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))

		return false
	}

	if dec.More() {
		writeError(w, http.StatusBadRequest, errors.New("bad request body: data after the JSON object"))

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
