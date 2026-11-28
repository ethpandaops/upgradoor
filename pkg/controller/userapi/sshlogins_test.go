package userapi

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// recordSSH stores a closed group of identity on host, starting at start.
func recordSSH(t *testing.T, st store.Store, host string, seq int64, identity, user string, start time.Time) {
	t.Helper()

	end := start.Add(time.Minute)

	_, err := st.RecordSSHLogins(context.Background(), host, start, &protocol.SSHReport{
		Closed: []protocol.SSHLoginGroup{{
			Seq: seq, Identity: identity, Fingerprint: "SHA256:" + identity, User: user, From: "10.0.0.5",
			FirstLogin: start, LastLogout: &end, Sessions: 3,
		}},
	})
	require.NoError(t, err)
}

func TestSSHLogins(t *testing.T) {
	h, provider, st := realHarness(t)

	run := controllertest.Epoch.Add(-time.Hour)
	for i, host := range []string{"node-1", "node-2", "node-3"} {
		recordSSH(t, st, host, 1, "github:pk910", "devops", run.Add(time.Duration(i)*time.Second))
	}

	recordSSH(t, st, "node-1", 2, "", "root", run.Add(-3*time.Hour))

	audit := provider.Grant(t, "+upgradoor:audit")

	get := func(query url.Values, authorization string) (*views.SSHLogins, int) {
		rec := h.serve(userRoute{method: http.MethodGet, path: "/api/v1/ssh-logins?" + query.Encode()}.
			request(authorization))
		if rec.Code != http.StatusOK {
			return nil, rec.Code
		}

		body := controllertest.DecodeJSON[views.SSHLogins](t, rec)

		return &body, rec.Code
	}

	_, code := get(nil, provider.Grant(t, "+upgradoor"))
	assert.Equal(t, http.StatusForbidden, code, "the SSH logins need the feature audit")

	fleetPage, code := get(nil, audit)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, fleetPage.Clusters, 2, "the fleet view by default")
	assert.NotNil(t, fleetPage.Groups)
	assert.Empty(t, fleetPage.Groups)
	assert.Equal(t, 3, fleetPage.Clusters[0].Hosts)

	for _, tt := range []struct {
		query url.Values
		want  int
	}{
		{url.Values{"view": {"groups"}}, 4},
		{url.Values{"view": {"groups"}, "host": {"node-1"}}, 2},
		{url.Values{"view": {"groups"}, "identity": {"github:pk910"}}, 3},
		{url.Values{"view": {"groups"}, "user": {"root"}}, 1},
		{url.Values{"view": {"groups"}, "unknown": {"1"}}, 1},
		{url.Values{"view": {"groups"}, "fingerprint": {"SHA256:"}}, 1},
		{url.Values{"view": {"groups"}, "open": {"1"}}, 0},
		{url.Values{"view": {"groups"}, "from": {run.Format(time.RFC3339Nano)}}, 3},
		{url.Values{"view": {"groups"}, "to": {run.Add(time.Second).Format(time.RFC3339Nano)}}, 2},
	} {
		page, code := get(tt.query, audit)
		require.Equal(t, http.StatusOK, code, tt.query.Encode())
		assert.Len(t, page.Groups, tt.want, tt.query.Encode())
		assert.NotNil(t, page.Clusters)
	}

	page, _ := get(url.Values{"view": {"groups"}, "limit": {"3"}}, audit)
	require.Len(t, page.Groups, 3)
	require.NotEmpty(t, page.Next)

	page, _ = get(url.Values{"view": {"groups"}, "limit": {"3"}, "cursor": {page.Next}}, audit)
	require.Len(t, page.Groups, 1)
	assert.Equal(t, "root", page.Groups[0].User)
	assert.Empty(t, page.Next)

	for _, bad := range []url.Values{
		{"view": {"hosts"}}, {"from": {"yesterday"}}, {"cursor": {"x"}}, {"limit": {"0"}},
	} {
		_, code := get(bad, audit)
		assert.Equal(t, http.StatusBadRequest, code, bad.Encode())
	}
}

// An open API serves them to everyone.
func TestSSHLoginsOpen(t *testing.T) {
	h := newHarness(t, nil)
	h.start()

	rec := h.get("/api/v1/ssh-logins?view=groups&host=node-1&identity=github:pk910&user=devops&unknown=1&open=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, store.SSHLoginFilter{
		View: store.SSHViewGroups, Host: "node-1", Identity: "github:pk910", User: "devops",
		Unknown: true, Open: true, Limit: store.DefaultSSHLoginLimit,
	}, h.store.LastSSHFilter)
}
