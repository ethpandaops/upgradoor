// Command fakeregistry serves the tags of scenario files as a registry, for a
// local controller with fake agents (test/fakeagent): the controller looks
// the tags up here, and the fake hosts take the digest of a pull from here.
// It serves no images.
//
//	go run ./test/fakeregistry --listen 127.0.0.1:5050 --scenario host.yaml [--scenario ...]
//
// The images of the scenarios are named under the listen address
// ("127.0.0.1:5050/ethpandaops/geth:master"). A tag moves with
//
//	POST /tags  {"ref": "ethpandaops/geth:master", "digest": "sha256:..."}
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
)

// maxBody bounds a request body.
const maxBody = 64 << 10

type scenarios []string

func (s *scenarios) String() string { return fmt.Sprint(*s) }

func (s *scenarios) Set(v string) error {
	*s = append(*s, v)

	return nil
}

func main() {
	var (
		listen string
		files  scenarios
	)

	flag.StringVar(&listen, "listen", "127.0.0.1:5050", "loopback address to serve on")
	flag.Var(&files, "scenario", "scenario file whose images and registry tags are served (repeatable)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, listen, files)

	stop()

	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeregistry:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, listen string, files []string) error {
	ip, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}

	if parsed := net.ParseIP(ip); parsed == nil || !parsed.IsLoopback() {
		return fmt.Errorf("listen address %q is not a loopback IP address", listen)
	}

	reg, err := load(files)
	if err != nil {
		return err
	}

	srv := &http.Server{Addr: listen, Handler: handler(reg), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdown)
	}()

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// load returns a registry with the container images and the registry tags of
// the scenario files.
func load(files []string) (*agenttest.Registry, error) {
	reg := agenttest.NewRegistry()

	for _, file := range files {
		sc, err := agenttest.LoadScenario(file)
		if err != nil {
			return nil, err
		}

		for i := range sc.Containers {
			reg.Set(sc.Containers[i].Image, sc.Containers[i].Digest)
		}

		for ref, tag := range sc.Registry {
			reg.Set(ref, tag.Digest)
		}
	}

	return reg, nil
}

// handler serves the registry and POST /tags.
func handler(reg *agenttest.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v2/", reg)

	mux.HandleFunc("POST /tags", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ref    string `json:"ref"`
			Digest string `json:"digest"`
		}

		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		dec.DisallowUnknownFields()

		if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "the body is not one JSON object of ref and digest", http.StatusBadRequest)

			return
		}

		if in.Digest == "" || !reg.Set(in.Ref, in.Digest) {
			http.Error(w, "ref (a tag reference) and digest are required", http.StatusBadRequest)

			return
		}

		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}
