package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
)

// fakeSocket answers POST /v1/deploy with res and keeps the requests.
func fakeSocket(t *testing.T, res *local.DeployResult) (string, func() []local.DeployRequest) {
	t.Helper()

	dir, err := os.MkdirTemp(os.TempDir(), "s")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "agent.sock")

	var lc net.ListenConfig

	l, err := lc.Listen(t.Context(), "unix", path)
	require.NoError(t, err)

	var (
		mu  sync.Mutex
		got []local.DeployRequest
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+local.PathDeploy, func(w http.ResponseWriter, r *http.Request) {
		var req local.DeployRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		mu.Lock()
		got = append(got, req)
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(res)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()

	t.Cleanup(func() { _ = srv.Close() })

	return path, func() []local.DeployRequest {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(got)
	}
}

func run(args ...string) (string, error) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	var out bytes.Buffer

	root := newRootCmd(log)
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs(args)

	err := root.Execute()

	return out.String(), err
}

func TestDeploy(t *testing.T) {
	res := &local.DeployResult{
		Container: "geth", Exists: true, Managed: true, Changed: true, Roll: local.DeployRollRequested,
		Ignore: []string{"image", "command", "labels"},
	}
	socket, got := fakeSocket(t, res)

	out, err := run("deploy", "geth", "--socket", socket, "--image", "ethpandaops/geth:master",
		"--command", `["--http","--port=30303"]`, "--reset")
	require.NoError(t, err)
	assert.JSONEq(t, `{"container":"geth","exists":true,"managed":true,"changed":true,`+
		`"roll":"requested","ignore":["image","command","labels"],"pull":false}`, out)

	_, err = run("deploy", "geth", "--socket", socket, "--image", "ethpandaops/geth:master")
	require.NoError(t, err)

	assert.Equal(t, []local.DeployRequest{
		{Container: "geth", Image: "ethpandaops/geth:master", Command: []string{"--http", "--port=30303"}, Reset: true},
		{Container: "geth", Image: "ethpandaops/geth:master"},
	}, got())

	_, err = run("deploy", "geth", "--socket", socket, "--image", "x:1", "--command", `"--http"`)
	require.EqualError(t, err, "--command is a JSON array of strings")

	_, err = run("deploy", "geth", "--socket", socket)
	require.ErrorContains(t, err, `"image" not set`)
	assert.Len(t, got(), 2)

	_, err = run("deploy", "geth", "--socket", filepath.Join(t.TempDir(), "none.sock"), "--image", "x:1")
	require.ErrorContains(t, err, "is the agent running")
}
