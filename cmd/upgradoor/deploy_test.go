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
		Container: "geth", Exists: true, Managed: true, Roll: local.DeployRollRequested,
		Ignore: []string{"image", "command", "labels"},
	}
	socket, got := fakeSocket(t, res)
	managed := filepath.Join(t.TempDir(), "managed.yaml")

	out, err := run("deploy", "geth", "--socket", socket, "--managed-file", managed, "--image", "ethpandaops/geth:master",
		"--command", `["--http","--port=30303"]`, "--reset")
	require.NoError(t, err)
	assert.JSONEq(t, `{"container":"geth","exists":true,"managed":true,"changed":true,`+
		`"roll":"requested","ignore":["image","command","labels"],"pull":false}`, out, "managed.yaml changed")

	raw, err := os.ReadFile(managed)
	require.NoError(t, err)
	assert.Equal(t, "# written by upgradoor deploy, do not edit\ncontainers:\n  geth:\n"+
		"    image: ethpandaops/geth:master\n    command:\n      - --http\n      - --port=30303\n", string(raw))

	out, err = run("deploy", "geth", "--socket", socket, "--managed-file", managed, "--image", "ethpandaops/geth:master",
		"--command", `["--http","--port=30303"]`)
	require.NoError(t, err)
	assert.Contains(t, out, `"changed":false`, "neither managed.yaml nor the override changed")

	assert.Equal(t, []local.DeployRequest{{Container: "geth", Reset: true}, {Container: "geth"}}, got())

	for _, args := range [][]string{
		{"--image", "x:1", "--command", `"--http"`},
		{},
		{"--image", "Not An Image"},
	} {
		_, err = run(append([]string{"deploy", "geth", "--socket", socket, "--managed-file", managed}, args...)...)
		require.Error(t, err, args)
	}

	assert.Len(t, got(), 2)

	raw2, err := os.ReadFile(managed)
	require.NoError(t, err)
	assert.Equal(t, raw, raw2, "a refused deploy writes nothing")

	_, err = run("deploy", "geth", "--socket", filepath.Join(t.TempDir(), "none.sock"), "--managed-file", managed,
		"--image", "x:1")
	require.ErrorContains(t, err, "is the agent running")

	if os.Geteuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		require.NoError(t, os.Mkdir(ro, 0o500))

		_, err = run("deploy", "geth", "--socket", socket, "--managed-file", filepath.Join(ro, "managed.yaml"),
			"--image", "x:1")
		require.EqualError(t, err, "deploy is for root only: cannot write "+ro)
	}
}
