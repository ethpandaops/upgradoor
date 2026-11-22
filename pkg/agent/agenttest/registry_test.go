package agenttest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	require.True(t, r.Set("127.0.0.1:5050/ethpandaops/geth:master", GethV1))
	require.True(t, r.Set("nginx", "sha256:nginx"))
	require.False(t, r.Set("ethpandaops/geth@sha256:"+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GethV2))

	call := func(method, path string) (int, string) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody))

		return rec.Code, rec.Header().Get("Docker-Content-Digest")
	}

	for _, method := range []string{http.MethodHead, http.MethodGet} {
		code, digest := call(method, "/v2/ethpandaops/geth/manifests/master")
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, GethV1, digest)
	}

	code, digest := call(http.MethodHead, "/v2/library/nginx/manifests/latest")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "sha256:nginx", digest)

	code, _ = call(http.MethodGet, "/v2/")
	assert.Equal(t, http.StatusOK, code)

	for _, path := range []string{"/v2/ethpandaops/geth/manifests/other", "/v2/ethpandaops/geth", "/other"} {
		code, _ = call(http.MethodGet, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}

	code, _ = call(http.MethodPost, "/v2/ethpandaops/geth/manifests/master")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestHostPullFrom(t *testing.T) {
	h, err := NewHost(BaseScenario())
	require.NoError(t, err)

	h.PullFrom(func(context.Context, string) (string, error) { return GethV3, nil })

	digest, _, err := h.Pull(t.Context(), GethImage)
	require.NoError(t, err)
	assert.Equal(t, GethV3, digest)

	local, _, err := h.LocalImage(t.Context(), GethImage)
	require.NoError(t, err)
	assert.Equal(t, GethV3, local)

	h.PullFrom(func(context.Context, string) (string, error) { return "", assert.AnError })

	_, _, err = h.Pull(t.Context(), GethImage)
	require.ErrorIs(t, err, assert.AnError)
}
