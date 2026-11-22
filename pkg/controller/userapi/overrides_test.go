package userapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
)

func postClientOverride(h *harness, u *authz.User, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/actions/client-override", strings.NewReader(body))
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
	controllertest.OwnershipFixture(h.store)

	team := &authz.User{Actor: controllertest.UserEmail, Teams: []string{"lighthouse"}, Services: "+upgradoor:team_image_override"}

	// teku-geth-1 runs no consensus container of lighthouse: the scheduler
	// names that. ghost-1 is unknown: so does the scheduler.
	body := fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q,%q,%q],"part":"image","image":"sigp/lighthouse:fix"}`,
		controllertest.HostMixed, controllertest.HostTeku, controllertest.HostGhost)

	rec := postClientOverride(h, team, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls := h.sched.Recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, scheduler.Authorization{By: "team_image_override", Teams: team.Teams}, calls[0].Auth)
	assert.Equal(t, []string{controllertest.HostMixed, controllertest.HostTeku, controllertest.HostGhost}, overrideReq(t, calls[0]).Hosts)

	// The teku team: lighthouse-geth-1's lighthouse is not its own.
	tekuTeam := &authz.User{Actor: controllertest.UserEmail, Teams: []string{"teku"}, Services: "+upgradoor:team_image_override"}
	body = fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q],"part":"image","image":"sigp/lighthouse:fix"}`,
		controllertest.HostMixed)

	rec = postClientOverride(h, tekuTeam, body)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error,
		"forbidden: upgradoor:team_image_override does not cover container lighthouse on host "+controllertest.HostMixed)

	// Both: the covered host is sent, the other one is listed.
	both := &authz.User{Actor: controllertest.UserEmail, Teams: []string{"teku"}, Services: "+upgradoor:team_image_override"}
	body = fmt.Sprintf(`{"client":"teku","kind":"cl","hosts":[%q,%q],"part":"image","image":"consensys/teku:fix"}`,
		controllertest.HostTeku, controllertest.HostMixed)

	rec = postClientOverride(h, both, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls = h.sched.Recorded()
	require.Len(t, calls, 2)
	assert.Equal(t, []string{controllertest.HostTeku, controllertest.HostMixed}, overrideReq(t, calls[1]).Hosts,
		"a host that runs no such container is the scheduler's to refuse")

	// The plain feature decides once for every host.
	plain := &authz.User{Actor: controllertest.UserEmail, Services: "+upgradoor:flag_override"}
	body = fmt.Sprintf(`{"client":"lighthouse","kind":"vc","hosts":[%q,%q],"part":"flags","addFlags":["--debug"]}`,
		controllertest.HostMixed, controllertest.HostTeku)

	rec = postClientOverride(h, plain, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, h.sched.Recorded(), 3)
}

// TestClientOverrideRefusesUncoveredHosts: a host the twin does not cover is
// in the answer's rejected, not sent and not skipped silently.
func TestClientOverrideRefusesUncoveredHosts(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	// geth on both hosts; the team geth owns both, the team lighthouse none.
	u := &authz.User{Actor: controllertest.UserEmail, Teams: []string{"lighthouse", "geth"}, Services: "+upgradoor:team_flag_override"}
	body := fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q,%q],"part":"flags","addFlags":["--x"]}`,
		controllertest.HostMixed, controllertest.HostUnowned)

	rec := postClientOverride(h, u, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := controllertest.DecodeJSON[scheduler.ActionResult](t, rec)
	assert.Equal(t, []string{controllertest.HostMixed, controllertest.HostUnowned}, got.Hosts, "the fake sends what it is given")
}

func overrideReq(t *testing.T, call controllertest.Call) scheduler.ClientOverrideRequest {
	t.Helper()

	req, ok := call.Req.(scheduler.ClientOverrideRequest)
	require.True(t, ok, "%T", call.Req)

	return req
}

func TestClientOverrideValidates(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	admin := &authz.User{Actor: controllertest.UserEmail, Services: "+upgradoor:image_override,+upgradoor:flag_override"}

	for name, body := range map[string]string{
		"no client":       `{"kind":"cl","hosts":["a"],"part":"image","image":"x:y"}`,
		"an invalid kind": `{"client":"teku","kind":"CL","hosts":["a"],"part":"image","image":"x:y"}`,
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

	assert.Empty(t, h.sched.Recorded())
}
