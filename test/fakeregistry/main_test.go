package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/registry"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestF = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// The controller's resolver reads the tags of a scenario, and a moved tag.
func TestTheControllerResolvesTheScenariosTags(t *testing.T) {
	file := filepath.Join(t.TempDir(), "host.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`
containers:
  - {name: geth, image: "127.0.0.1:5050/ethpandaops/geth:master", digest: "`+digestA+`", group: execution, kind: el}
registry:
  "127.0.0.1:5050/ethpandaops/geth:fix": {digest: "`+digestF+`"}
`), 0o600))

	reg, err := load([]string{file})
	require.NoError(t, err)

	srv := httptest.NewServer(handler(reg))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")

	resolver, err := registry.NewResolver(logrus.New(),
		&registry.Config{PlainHTTP: []string{host}, AllowPrivate: true}, nil)
	require.NoError(t, err)

	for ref, want := range map[string]string{"ethpandaops/geth:master": digestA, "ethpandaops/geth:fix": digestF} {
		got, rerr := resolver.Resolve(t.Context(), host+"/"+ref)
		require.NoError(t, rerr, ref)
		assert.Equal(t, want, got)
	}

	post := func(body string) int {
		req, rerr := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/tags", strings.NewReader(body))
		require.NoError(t, rerr)

		resp, rerr := srv.Client().Do(req)
		require.NoError(t, rerr)
		require.NoError(t, resp.Body.Close())

		return resp.StatusCode
	}

	assert.Equal(t, http.StatusNoContent, post(`{"ref":"ethpandaops/geth:master","digest":"`+digestB+`"}`))

	got, err := resolver.Resolve(t.Context(), host+"/ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, digestB, got)

	for _, bad := range []string{`{"ref":"x"}`, `{"ref":"x","digest":"d","other":1}`, `not json`, `{} {}`} {
		assert.Equal(t, http.StatusBadRequest, post(bad), bad)
	}

	_, err = load([]string{filepath.Join(t.TempDir(), "missing.yaml")})
	require.Error(t, err)
}

func TestRunListensOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "localhost:0", "192.0.2.1:0", "127.0.0.1"} {
		assert.Error(t, run(t.Context(), addr, nil), addr)
	}
}
