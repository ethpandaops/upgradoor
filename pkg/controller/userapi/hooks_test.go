package userapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
)

const hookSecret = "a-secret-of-the-build-hook"

// countingUpstream counts the calls of Refresh.
type countingUpstream struct{ refreshed int }

func (c *countingUpstream) Refresh() int {
	c.refreshed++

	return 2
}

func hookRequest(body string, header map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+PathBuildHook, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}

	return req
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write([]byte(body))

	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func TestBuildHook(t *testing.T) {
	up := &countingUpstream{}
	h := newHarness(t, func(o *Options) {
		o.HookSecret = []byte(hookSecret)
		o.Upstream = up
	})

	done := `{"action":"completed","workflow_run":{"id":1}}`

	cases := []struct {
		name      string
		body      string
		header    map[string]string
		status    int
		refreshed bool
	}{
		{name: "no secret", status: http.StatusUnauthorized},
		{name: "wrong bearer", header: map[string]string{"Authorization": "Bearer nope"}, status: http.StatusUnauthorized},
		{
			name: "wrong signature", body: done,
			header: map[string]string{headerGitHubSignature: sign(done + " ")}, status: http.StatusUnauthorized,
		},
		{
			name: "a signature that is no hex", body: done,
			header: map[string]string{headerGitHubSignature: signaturePrefix + "zz"}, status: http.StatusUnauthorized,
		},
		{
			name:   "bearer",
			header: map[string]string{"Authorization": "Bearer " + hookSecret}, status: http.StatusOK, refreshed: true,
		},
		{
			name: "a GitHub run that completed", body: done,
			header: map[string]string{headerGitHubSignature: sign(done), headerGitHubEvent: "workflow_run"},
			status: http.StatusOK, refreshed: true,
		},
		{
			name: "a GitHub run that only started", body: `{"action":"requested"}`,
			header: map[string]string{
				headerGitHubSignature: sign(`{"action":"requested"}`), headerGitHubEvent: "workflow_run",
			},
			status: http.StatusOK,
		},
		{
			name: "a GitHub ping", body: `{"zen":"x"}`,
			header: map[string]string{headerGitHubSignature: sign(`{"zen":"x"}`), headerGitHubEvent: "ping"},
			status: http.StatusOK,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := up.refreshed

			rec := h.serve(hookRequest(c.body, c.header))
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			require.Equal(t, c.refreshed, up.refreshed > before)

			if c.status == http.StatusOK {
				require.Contains(t, rec.Body.String(), `"refreshed":`+map[bool]string{true: "true", false: "false"}[c.refreshed])
			}
		})
	}

	require.Equal(t, 2, h.lookups.Refreshed)
}

func TestBuildHookIsOffWithoutASecret(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.serve(hookRequest("", map[string]string{"Authorization": "Bearer "}))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Zero(t, h.lookups.Refreshed)
}
