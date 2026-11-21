package fakehost

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func post(t *testing.T, url string, body any) (int, State) {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(raw))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	var st State

	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	}

	return resp.StatusCode, st
}

func TestControlEndpoint(t *testing.T) {
	h := loadHost(t)

	srv := httptest.NewServer(h.ControlHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/state")
	require.NoError(t, err)

	var st State

	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "test-boot", st.BootID)
	assert.Len(t, st.Containers, 3)
	assert.Equal(t, "sha256:ddd", st.Registry["docker.io/ethpandaops/geth:master"].Digest)
	assert.Equal(t, Tag{Digest: "sha256:aaa", Revision: "r1"},
		st.Local["docker.io/ethpandaops/geth:master"])

	// Move a tag while the agent would be running.
	code, st := post(t, srv.URL+"/registry", map[string]string{
		"ref": "sigp/lighthouse", "digest": "sha256:eee", "revision": "r9",
	})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, Tag{Digest: "sha256:eee", Revision: "r9"}, st.Registry["docker.io/sigp/lighthouse:latest"])

	digest, err := h.Resolve(context.Background(), "sigp/lighthouse:latest")
	require.NoError(t, err)
	assert.Equal(t, "sha256:eee", digest)

	code, st = post(t, srv.URL+"/reboot-required", map[string]bool{"required": false})
	require.Equal(t, http.StatusOK, code)
	assert.False(t, st.RebootRequired)

	code, st = post(t, srv.URL+"/crash", map[string]string{"container": "geth"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 1, st.Containers[0].RestartCount)

	// Bad requests change nothing.
	for path, body := range map[string]any{
		"/registry":        map[string]string{"ref": "x"},
		"/reboot-required": map[string]string{"unknown": "field"},
	} {
		code, _ = post(t, srv.URL+path, body)
		assert.Equal(t, http.StatusBadRequest, code, path)
	}

	code, _ = post(t, srv.URL+"/crash", map[string]string{"container": "nimbus"})
	assert.Equal(t, http.StatusNotFound, code)

	code, _ = post(t, srv.URL+"/state", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, code)

	code, _ = post(t, srv.URL+"/kernel", map[string]string{"newest": "6.13.0"})
	assert.Equal(t, http.StatusNotFound, code, "no such endpoint")
}

func TestServeControl(t *testing.T) {
	h := loadHost(t)

	// The endpoint has no authentication: loopback only.
	for _, addr := range []string{"0.0.0.0:7777", "192.0.2.1:7777", "example.org:80", ":7777", "nonsense"} {
		require.Error(t, h.ServeControl(t.Context(), addr), addr)
	}

	// Find a free loopback port.
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)

	go func() { served <- h.ServeControl(ctx, addr) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/state")
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 5*time.Millisecond)

	cancel()

	select {
	case err := <-served:
		require.NoError(t, err, "ending the context is a clean stop")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the control endpoint did not stop")
	}
}
