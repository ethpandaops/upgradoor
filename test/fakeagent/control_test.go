package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
)

func TestNewControlLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "localhost:0", "192.0.2.1:0", "127.0.0.1"} {
		_, err := newControl(nil, addr)
		assert.Error(t, err, addr)
	}

	c, err := newControl(nil, "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, c.ln.Close())
}

func TestControlHandler(t *testing.T) {
	host, err := agenttest.NewHost(agenttest.BaseScenario(), nil)
	require.NoError(t, err)

	h := (&control{host: host}).handler()

	call := func(method, path, body string) (int, state) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))

		var st state
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		}

		return rec.Code, st
	}

	code, st := call(http.MethodGet, "/state", "")
	require.Equal(t, http.StatusOK, code)
	assert.NotEmpty(t, st.Containers)
	assert.False(t, st.RebootRequired)

	code, st = call(http.MethodPost, "/reboot-required", `{"required":true}`)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, st.RebootRequired)

	for _, bad := range []struct{ path, body string }{
		{"/reboot-required", `{"required":true,"other":1}`},
		{"/reboot-required", `not json`},
		{"/reboot-required", `{"required":true} {"required":false}`},
	} {
		code, _ = call(http.MethodPost, bad.path, bad.body)
		assert.Equal(t, http.StatusBadRequest, code, bad.body)
	}

	code, _ = call(http.MethodPost, "/crash", `{"container":"nosuch"}`)
	assert.Equal(t, http.StatusNotFound, code)
}
