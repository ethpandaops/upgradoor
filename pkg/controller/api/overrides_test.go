package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func postClientOverride(h *harness, u *user, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, testBaseURL+"/api/v1/actions/client-override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.srv.handleClientOverride(rec, req, u)

	return rec
}

// TestClientOverrideEachHost: through the team twin every host is judged
// alone. The hosts the teams cover go to the scheduler, the others come back
// as rejected with the reason; none covered refuses the request.
func TestClientOverrideEachHost(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	team := &user{actor: userEmail, teams: []string{"lighthouse"}, services: "+upgradoor:team_image_override"}

	// teku-geth-1 runs no consensus container of lighthouse: the scheduler
	// names that. ghost-1 is unknown: so does the scheduler.
	body := fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q,%q,%q],"part":"image","image":"sigp/lighthouse:fix"}`,
		hostMixed, hostTeku, hostGhost)

	rec := postClientOverride(h, team, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls := h.sched.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, scheduler.Authorization{By: "team_image_override", Teams: team.teams}, calls[0].Auth)
	assert.Equal(t, []string{hostMixed, hostTeku, hostGhost}, overrideReq(t, calls[0]).Hosts)

	// The teku team: lighthouse-geth-1's lighthouse is not its own.
	tekuTeam := &user{actor: userEmail, teams: []string{"teku"}, services: "+upgradoor:team_image_override"}
	body = fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q],"part":"image","image":"sigp/lighthouse:fix"}`,
		hostMixed)

	rec = postClientOverride(h, tekuTeam, body)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, decodeJSON[ErrorBody](t, rec).Error,
		"forbidden: upgradoor:team_image_override does not cover container lighthouse on host "+hostMixed)

	// Both: the covered host is sent, the other one is listed.
	both := &user{actor: userEmail, teams: []string{"teku"}, services: "+upgradoor:team_image_override"}
	body = fmt.Sprintf(`{"client":"teku","kind":"cl","hosts":[%q,%q],"part":"image","image":"consensys/teku:fix"}`,
		hostTeku, hostMixed)

	rec = postClientOverride(h, both, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls = h.sched.recorded()
	require.Len(t, calls, 2)
	assert.Equal(t, []string{hostTeku, hostMixed}, overrideReq(t, calls[1]).Hosts,
		"a host that runs no such container is the scheduler's to refuse")

	// The plain feature decides once for every host.
	plain := &user{actor: userEmail, services: "+upgradoor:flag_override"}
	body = fmt.Sprintf(`{"client":"lighthouse","kind":"vc","hosts":[%q,%q],"part":"flags","addFlags":["--debug"]}`,
		hostMixed, hostTeku)

	rec = postClientOverride(h, plain, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, h.sched.recorded(), 3)
}

// TestClientOverrideRefusesUncoveredHosts: a host the twin does not cover is
// in the answer's rejected, not sent and not skipped silently.
func TestClientOverrideRefusesUncoveredHosts(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	// geth on both hosts; the team geth owns both, the team lighthouse none.
	u := &user{actor: userEmail, teams: []string{"lighthouse", "geth"}, services: "+upgradoor:team_flag_override"}
	body := fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q,%q],"part":"flags","addFlags":["--x"]}`,
		hostMixed, hostUnowned)

	rec := postClientOverride(h, u, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := decodeJSON[scheduler.ActionResult](t, rec)
	assert.Equal(t, []string{hostMixed, hostUnowned}, got.Hosts, "the fake sends what it is given")

	// A second teku host whose teku the team teku does not own.
	h.store.mu.Lock()
	h.store.hosts["teku-geth-2"] = &fleet.Host{Name: "teku-geth-2", LastSeen: testEpoch, Status: &protocol.SyncRequest{
		Containers: []protocol.Container{ofClient(owned("teku", "nimbus"), protocol.KindCL, "teku")},
	}}
	h.store.mu.Unlock()

	u = &user{actor: userEmail, teams: []string{"teku"}, services: "+upgradoor:team_flag_override"}
	body = fmt.Sprintf(`{"client":"teku","kind":"cl","hosts":[%q,"teku-geth-2"],"part":"flags","addFlags":["--x"]}`,
		hostTeku)

	rec = postClientOverride(h, u, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got = decodeJSON[scheduler.ActionResult](t, rec)
	assert.Equal(t, []string{hostTeku}, got.Hosts)
	require.Contains(t, got.Rejected, "teku-geth-2")
	assert.Contains(t, got.Rejected["teku-geth-2"], "upgradoor:team_flag_override does not cover container teku")
	assert.Equal(t, "sent to 1 of 2 hosts: each agent decides, and its container follows under the host's next lease",
		got.Message)
}

func overrideReq(t *testing.T, call schedulerCall) scheduler.ClientOverrideRequest {
	t.Helper()

	req, ok := call.Req.(scheduler.ClientOverrideRequest)
	require.True(t, ok, "%T", call.Req)

	return req
}

func TestClientOverrideValidates(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	admin := &user{actor: userEmail, services: "+upgradoor:image_override,+upgradoor:flag_override"}

	for name, body := range map[string]string{
		"no client":       `{"kind":"cl","hosts":["a"],"part":"image","image":"x:y"}`,
		"unknown kind":    `{"client":"teku","kind":"other","hosts":["a"],"part":"image","image":"x:y"}`,
		"no hosts":        `{"client":"teku","kind":"cl","hosts":[],"part":"image","image":"x:y"}`,
		"unknown part":    `{"client":"teku","kind":"cl","hosts":["a"],"part":"both"}`,
		"image and flags": `{"client":"teku","kind":"cl","hosts":["a"],"part":"image","image":"x:y","addFlags":["--x"]}`,
		"flags and image": `{"client":"teku","kind":"cl","hosts":["a"],"part":"flags","image":"x:y"}`,
		"bad image":       `{"client":"teku","kind":"cl","hosts":["a"],"part":"image","image":"x y"}`,
		"duplicate host":  `{"client":"teku","kind":"cl","hosts":["a","a"],"part":"image","image":"x:y"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postClientOverride(h, admin, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}

	assert.Empty(t, h.sched.recorded())
}
