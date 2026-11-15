package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// TestRetryOfAnAbortedRolloutNeedsSupervise: retrying an aborted rollout
// reopens its cause for every host. It needs the rollout-level feature,
// whatever hosts the request names; recover and its twin are about hosts.
func TestRetryOfAnAbortedRolloutNeedsSupervise(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	retry := func(rollout string, hosts ...string) string {
		quoted := make([]string, 0, len(hosts))
		for _, host := range hosts {
			quoted = append(quoted, fmt.Sprintf("%q", host))
		}

		return fmt.Sprintf(`{"rollout":%q,"hosts":[%s]}`, rollout, strings.Join(quoted, ","))
	}

	lacksSupervise := "forbidden: this login lacks the feature upgradoor:supervise"
	teamRecover, lighthouse := "+upgradoor:team_recover", []string{"lighthouse"}

	for _, tt := range []authzCase{
		{
			name: "the team twin, an owned host", body: retry("r-aborted", hostMixed),
			services: teamRecover, teams: lighthouse, refusal: lacksSupervise,
		},
		{
			name: "the team twin, a host that is no member", body: retry("r-aborted", hostTeku),
			services: "+upgradoor:team_*", teams: []string{"geth", "teku"}, refusal: lacksSupervise,
		},
		{
			name: "recover", body: retry("r-aborted", hostMixed),
			services: "+upgradoor:recover", refusal: lacksSupervise,
		},
		{
			name: "recover and every twin", body: retry("r-aborted", hostMixed),
			services: "+upgradoor:recover:roll:reboot:hold:team_*", teams: lighthouse, refusal: lacksSupervise,
		},
		{
			// Allowed here; the scheduler then refuses the hosts.
			name: "supervise", body: retry("r-aborted", hostMixed),
			services: "+upgradoor:supervise", by: "supervise",
		},
		{
			name: "supervise, no hosts", body: retry("r-aborted"),
			services: "+upgradoor:supervise", by: "supervise",
		},
		{
			name: "recover, no hosts", body: retry("r-aborted"),
			services: "+upgradoor:recover", refusal: lacksSupervise,
		},

		// A rollout that is not aborted: hosts are a recovery.
		{
			name: "halted, the team twin", body: retry("r-1", hostMixed),
			services: teamRecover, teams: lighthouse, by: "team_recover",
		},
		{
			name: "halted, recover", body: retry("r-1", hostMixed),
			services: "+upgradoor:recover", by: "recover",
		},
		{
			name: "halted, supervise names hosts", body: retry("r-1", hostMixed),
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:recover",
		},

		// A rollout nobody knows: nothing is owned in it. With the plain
		// feature the scheduler answers, as before.
		{
			name: "unknown, the team twin", body: retry("r-ghost", hostMixed),
			services: teamRecover, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_recover does not cover rollout r-ghost (owners: none)",
		},
		{
			name: "unknown, recover", body: retry("r-ghost", hostMixed),
			services: "+upgradoor:recover", by: "recover",
		},
		{
			name: "unknown, nothing", body: retry("r-ghost", hostMixed),
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:recover",
		},
	} {
		tt.action = "retry"

		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	// When the rollout cannot be read the request fails; it is not decided.
	h.store.setErr(assert.AnError)

	req := httptest.NewRequest(http.MethodPost, testBaseURL+"/api/v1/x",
		strings.NewReader(retry("r-aborted", hostMixed)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.srv.actions()["retry"](rec, req, &user{actor: userEmail, services: "+upgradoor:recover"})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestTeamTwinDoesNotTouchStaffHolds: team_hold lifts a hold that was set
// through team_hold, by any team that owns a container of the host. A hold
// that staff, the scheduler or the end of a lease set is not the twin's to
// lift, nor to set again as its own.
func TestTeamTwinDoesNotTouchStaffHolds(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	h.store.mu.Lock()
	// Staff's: HoldBy is empty, as for every hold of an earlier build.
	h.store.hosts[hostMixed].HoldWanted = true
	h.store.hosts[hostMixed].HoldReason = "lease ended by operator"
	// A team's.
	h.store.hosts[hostTeku].HoldWanted = true
	h.store.hosts[hostTeku].HoldBy = fleet.HoldByTeam
	h.store.mu.Unlock()

	hosts := func(names ...string) string { return `{"hosts":["` + strings.Join(names, `","`) + `"]}` }
	twin := "+upgradoor:team_hold"

	for _, tt := range []authzCase{
		{
			name: "unhold a staff hold", action: "unhold", body: hosts(hostMixed),
			services: twin, teams: []string{"lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "unhold a staff hold, every twin", action: "unhold", body: hosts(hostMixed),
			services: "+upgradoor:team_*", teams: []string{"geth", "lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "hold again what staff holds", action: "hold", body: hosts(hostMixed),
			services: twin, teams: []string{"lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not change the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "unhold a team's hold, the team", action: "unhold", body: hosts(hostTeku),
			services: twin, teams: []string{"teku"}, by: "team_hold",
		},
		{
			name: "unhold a team's hold, another owner of the host", action: "unhold", body: hosts(hostTeku),
			services: twin, teams: []string{"geth"}, by: "team_hold",
		},
		{
			name: "unhold a team's hold, a team without a container there", action: "unhold",
			body: hosts(hostTeku), services: twin, teams: []string{"lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not cover host teku-geth-1 (owners: geth, nimbus, teku)",
		},
		{
			name: "hold again what a team holds", action: "hold", body: hosts(hostTeku),
			services: twin, teams: []string{"geth"}, by: "team_hold",
		},
		{
			name: "one of two is staff's", action: "unhold", body: hosts(hostTeku, hostMixed),
			services: twin, teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "the plain feature lifts any hold", action: "unhold", body: hosts(hostTeku, hostMixed),
			services: "+upgradoor:hold", by: "hold",
		},
		{
			name: "the plain feature holds over any hold", action: "hold", body: hosts(hostMixed),
			services: "+upgradoor:hold", teams: []string{"lighthouse"}, by: "hold",
		},
		{
			name: "a host that is not held", action: "unhold", body: hosts(hostUnowned),
			services: twin, teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_hold does not cover host bootnode-1 (owners: none)",
		},
		{
			// Other host-level actions do not look at the hold.
			name: "reboot a host staff holds", action: "reboot", body: hosts(hostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"geth"}, by: "team_reboot",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	// The host views say whose hold it is.
	list := decodeJSON[Hosts](t, h.get("/api/v1/hosts"))
	by := make(map[string]string, len(list.Hosts))

	for _, host := range list.Hosts {
		by[host.Name] = host.HoldBy
	}

	assert.Equal(t, map[string]string{
		hostMixed: "staff", hostTeku: "team", hostUnowned: "", hostFresh: "",
	}, by)
	assert.Equal(t, "staff", decodeJSON[HostDetail](t, h.get("/api/v1/hosts/"+hostMixed)).HoldBy)
	assert.NotContains(t, h.get("/api/v1/public/hosts").Body.String(), "holdBy")
}

// TestTeamPathIsStaggered: a roll or reboot "now" skips the order and the
// wait for readiness, like force does. Through the team twin alone the
// request reaches the scheduler staggered; with the plain feature, or with
// admin next to the twin, it is what was asked for.
func TestTeamPathIsStaggered(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	roll := `{"hosts":["` + hostMixed + `"],"containers":["lighthouse"]}`
	reboot := `{"hosts":["` + hostMixed + `"]}`
	lighthouse := []string{"lighthouse"}

	for _, tt := range []struct {
		name      string
		action    string
		body      string
		services  string
		staggered bool
	}{
		{"roll now, team twin", "roll", roll, "+upgradoor:team_roll", true},
		{"roll now, every twin", "roll", roll, "+upgradoor:team_*", true},
		{"roll now, plain feature", "roll", roll, "+upgradoor:roll", false},
		{"roll now, plain and twin", "roll", roll, "+upgradoor:roll:team_roll", false},
		{"roll now, twin and admin", "roll", roll, "+upgradoor:team_roll:admin", false},
		{"roll now, everything", "roll", roll, "+upgradoor:all", false},
		{"reboot now, team twin", "reboot", reboot, "+upgradoor:team_reboot", true},
		{"reboot now, plain feature", "reboot", reboot, "+upgradoor:reboot", false},
		{"reboot now, twin and admin", "reboot", reboot, "+upgradoor:team_reboot:admin", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(h.sched.recorded())

			req := httptest.NewRequest(http.MethodPost, testBaseURL+"/api/v1/x", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")

			rec := httptest.NewRecorder()
			h.srv.actions()[tt.action](rec, req, &user{
				actor: userEmail, services: tt.services, teams: lighthouse,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			calls := h.sched.recorded()
			require.Len(t, calls, before+1)

			switch got := calls[before].Req.(type) {
			case scheduler.RollRequest:
				assert.Equal(t, tt.staggered, got.Staggered)
				assert.False(t, got.Force)
				assert.False(t, got.IgnoreBudget)
			case scheduler.RebootRequest:
				assert.Equal(t, tt.staggered, got.Staggered)
				assert.False(t, got.Force)
			default:
				require.Fail(t, "unexpected request", "%T", got)
			}
		})
	}

	// Open mode asks for nothing of the kind.
	rec := h.sendJSON(http.MethodPost, "/api/v1/actions/roll", roll)
	require.Equal(t, http.StatusOK, rec.Code)

	calls := h.sched.recorded()
	got, ok := calls[len(calls)-1].Req.(scheduler.RollRequest)
	require.True(t, ok)
	assert.False(t, got.Staggered)
}

// TestOwnershipIsExact: no teams own nothing, also where nobody owns
// anything; and a team is its name, byte for byte.
func TestOwnershipIsExact(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	hosts := func(name string) string { return `{"hosts":["` + name + `"]}` }

	for _, tt := range []authzCase{
		{
			name: "no teams, a host nobody owns", action: "reboot", body: hosts(hostUnowned),
			services: "+upgradoor:team_reboot",
			refusal:  "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "an empty team list, a host nobody owns", action: "hold", body: hosts(hostUnowned),
			services: "+upgradoor:team_hold", teams: []string{},
			refusal: "forbidden: upgradoor:team_hold does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, roll a host nobody owns", action: "roll", body: hosts(hostUnowned),
			services: "+upgradoor:team_roll",
			refusal: "forbidden: upgradoor:team_roll does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, roll a container nobody owns", action: "roll",
			body:     `{"hosts":["` + hostUnowned + `"],"containers":["bootnode"]}`,
			services: "+upgradoor:team_roll",
			refusal: "forbidden: upgradoor:team_roll does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, a host that never synced", action: "reboot", body: hosts(hostFresh),
			services: "+upgradoor:team_reboot",
			refusal:  "forbidden: upgradoor:team_reboot does not cover host fresh-1 (owners: none)",
		},
		{
			name: "an empty team name", action: "reboot", body: hosts(hostUnowned),
			services: "+upgradoor:team_reboot", teams: []string{""},
			refusal: "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "another case", action: "reboot", body: hosts(hostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"Lighthouse", "GETH"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "another case, a roll", action: "roll",
			body:     `{"hosts":["` + hostMixed + `"],"containers":["lighthouse"]}`,
			services: "+upgradoor:team_roll", teams: []string{"Lighthouse"},
			refusal: "forbidden: upgradoor:team_roll does not cover container lighthouse " +
				"on host lighthouse-geth-1 (owners: lighthouse)",
		},
		{
			name: "a prefix of the team", action: "reboot", body: hosts(hostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"light", "lighthouse2", " lighthouse"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "the name", action: "reboot", body: hosts(hostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"Lighthouse", "lighthouse"},
			by: "team_reboot",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}
}

// TestRefusalsEchoSafely: a name a refusal repeats is cut to 128 characters
// and carries printable ASCII only, whether it came with the request or from
// an agent.
func TestRefusalsEchoSafely(t *testing.T) {
	assert.Equal(t, "lighthouse-geth-1", echo("lighthouse-geth-1"))
	assert.Equal(t, "a?b??c?", echo("a\nb\x1b\x00c\x7f"))
	assert.Equal(t, "h??st", echo("hößt"[:1]+"\xc3\xb6st"), "bytes outside ASCII")
	assert.Equal(t, strings.Repeat("x", 128), echo(strings.Repeat("x", 500)))
	assert.Empty(t, echo(""))

	h := newHarness(t, nil)
	ownershipFixture(h)

	// What an agent reported, stored as it came (the sync cleans owners, not
	// container names).
	h.store.mu.Lock()
	h.store.hosts[hostUnowned].Status.Containers = append(h.store.hosts[hostUnowned].Status.Containers,
		protocol.Container{Name: "evil\r\n\x1b[31m" + strings.Repeat("n", 300), Managed: true})
	h.store.mu.Unlock()

	refusal := func(action, body string) string {
		req := httptest.NewRequest(http.MethodPost, testBaseURL+"/api/v1/x", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		h.srv.actions()[action](rec, req, &user{
			actor: userEmail, services: "+upgradoor:team_*", teams: []string{"geth"},
		})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		return decodeJSON[ErrorBody](t, rec).Error
	}

	long := strings.Repeat("h", 4000)

	for name, got := range map[string]string{
		"host":      refusal("hold", `{"hosts":["bad\u0007\u001b[2Jhosté`+long+`"]}`),
		"container": refusal("roll", `{"hosts":["`+hostMixed+`"],"containers":["c\n\u0000`+long+`"]}`),
		"lease":     refusal("end-lease", `{"lease":"l\t`+long+`"}`),
		"rollout":   refusal("retry", `{"rollout":"r\u001b`+long+`","hosts":["`+hostMixed+`"]}`),
		"agent":     refusal("roll", `{"hosts":["`+hostUnowned+`"]}`),
	} {
		assert.Less(t, len(got), 400, name)

		for _, c := range []byte(got) {
			require.True(t, c >= 0x20 && c <= 0x7e, "%s: byte %#x in %q", name, c, got)
		}
	}

	assert.Equal(t, "forbidden: upgradoor:team_hold does not cover host bad??[2Jhost??"+
		strings.Repeat("h", 128-len("bad??[2Jhost??"))+" (owners: none)",
		refusal("hold", `{"hosts":["bad\u0007\u001b[2Jhosté`+long+`"]}`))
}

// TestDuplicateContainerNames: a host that reports one name twice does not
// get to pick the owners afterwards. The first entry is the container; the
// later ones are dropped when the sync is stored.
func TestDuplicateContainerNames(t *testing.T) {
	h := newHarness(t, nil)

	req := syncRequest()
	req.Containers = []protocol.Container{
		owned("lighthouse", "lighthouse"),
		owned("geth", "geth"),
		// The same name again, for another team, and once more unmanaged.
		owned("lighthouse", "teku"),
		{Name: "lighthouse", State: "exited"},
		owned("geth"),
	}

	rec := h.agentPost(newAgent(t, identity.CertificateSpec{Hostname: hostMixed}), protocol.PathSync,
		req, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored := h.store.host(hostMixed).Status.Containers
	require.Len(t, stored, 2)
	assert.Equal(t, owned("lighthouse", "lighthouse"), stored[0])
	assert.Equal(t, owned("geth", "geth"), stored[1])

	roll := `{"hosts":["` + hostMixed + `"],"containers":["lighthouse"]}`

	for _, tt := range []authzCase{
		{
			name: "the team of the second entry", action: "roll", body: roll,
			services: "+upgradoor:team_roll", teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_roll does not cover container lighthouse " +
				"on host lighthouse-geth-1 (owners: lighthouse)",
		},
		{
			name: "the team of the second entry, host-level", action: "reboot",
			body:     `{"hosts":["` + hostMixed + `"]}`,
			services: "+upgradoor:team_reboot", teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "the team of the first entry", action: "roll", body: roll,
			services: "+upgradoor:team_roll", teams: []string{"lighthouse"}, by: "team_roll",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	// The view shows what was stored.
	detail := decodeJSON[HostDetail](t, h.get("/api/v1/hosts/"+hostMixed))
	require.Len(t, detail.Containers, 2)
}

// teamsVerifier accepts every token and gives it the teams.
type teamsVerifier struct {
	teams []string
}

func (v teamsVerifier) Verify(string, ...auth.VerifyOption) (*auth.Claims, error) {
	return &auth.Claims{Email: userEmail, Services: staffServices, Teams: v.teams}, nil
}

// TestTokenWithTooManyTeams: the teams of a token are decided with and are
// written into the audit entries of its actions. A token with more than 64
// is not a login; with 64 it is.
func TestTokenWithTooManyTeams(t *testing.T) {
	h, _ := authHarness(t, true)
	seedHost(h)

	teams := func(n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, fmt.Sprintf("team-%03d", i))
		}

		return out
	}

	with := func(n int) {
		h.srv.users.mu.Lock()
		h.srv.users.verifier = teamsVerifier{teams: teams(n)}
		h.srv.users.mu.Unlock()
	}

	read := userRoute{method: http.MethodGet, path: "/api/v1/hosts"}
	pause := userRoute{method: http.MethodPost, path: "/api/v1/actions/pause", body: `{}`}

	with(64)
	require.Equal(t, http.StatusOK, h.serve(read.request("Bearer x")).Code)
	require.Equal(t, http.StatusOK, h.serve(pause.request("Bearer x")).Code)

	calls := h.sched.recorded()
	require.Len(t, calls, 1)
	assert.Len(t, calls[0].Auth.Teams, 64)

	with(65)

	for _, route := range []userRoute{read, pause} {
		rec := h.serve(route.request("Bearer x"))
		require.Equal(t, http.StatusUnauthorized, rec.Code, route.path)
		assert.Equal(t, ErrorBody{Error: errUnauthenticated.Error()}, decodeJSON[ErrorBody](t, rec),
			"the answer does not say why")
	}

	assert.Len(t, h.sched.recorded(), 1)
}

// rolloutRow is a rollout as the scheduler stores one, with members in the
// given states.
func rolloutRow(id string, state fleet.RolloutState, members map[string]fleet.HostState) *fleet.Rollout {
	r := &fleet.Rollout{
		ID: id, Kind: fleet.RolloutPackages, Key: "packages/" + id, Title: "packages " + id,
		State: state, Approved: true, CreatedBy: "scheduler", CreatedAt: testEpoch, UpdatedAt: testEpoch,
	}

	if state == fleet.RolloutHalted {
		r.Reason = "a host failed"
	}

	if state.Terminal() {
		ended := testEpoch
		r.EndedAt = &ended
	}

	for _, host := range []string{hostMixed, hostTeku, hostUnowned} {
		if st, ok := members[host]; ok {
			r.Hosts = append(r.Hosts, fleet.RolloutHost{
				Host: host, State: st, Stage: fleet.StageRest, UpdatedAt: testEpoch,
			})
		}
	}

	return r
}

// TestRetryAndHoldEndToEnd runs the cases of the review through the real
// handlers, token verification, scheduler and store.
func TestRetryAndHoldEndToEnd(t *testing.T) {
	h, provider, st := realHarness(t)
	ctx := context.Background()

	syncOwned(t, h, hostMixed, owned("geth", "geth"), owned("lighthouse", "lighthouse"))
	syncOwned(t, h, hostTeku, owned("geth", "geth"), owned("teku", "teku"))
	syncOwned(t, h, hostUnowned, owned("bootnode"))

	require.NoError(t, st.Commit(ctx, &store.Changes{Rollouts: []*fleet.Rollout{
		rolloutRow("r-aborted", fleet.RolloutAborted, map[string]fleet.HostState{
			hostTeku: fleet.HostPending,
		}),
		// Halted: a host of lighthouse and geth failed, and one of teku and
		// geth; the bootnode waits.
		rolloutRow("r-halted", fleet.RolloutHalted, map[string]fleet.HostState{
			hostMixed: fleet.HostFailed, hostTeku: fleet.HostFailed, hostUnowned: fleet.HostPending,
		}),
	}}))

	send := func(action, body, authorization string) (int, string) {
		rec := h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))

		return rec.Code, decodeJSON[map[string]any](t, rec)["error"].(string) //nolint:errcheck // "" when there is none.
	}

	ok := func(action, body, authorization string) scheduler.ActionResult {
		t.Helper()

		rec := h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		return decodeJSON[scheduler.ActionResult](t, rec)
	}

	rollout := func(id string) *fleet.Rollout {
		r, err := st.GetRollout(ctx, id)
		require.NoError(t, err)

		return r
	}

	actions := func() int {
		entries, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
		require.NoError(t, err)

		return len(entries)
	}

	open := func() int {
		rollouts, err := st.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
		require.NoError(t, err)

		return len(rollouts)
	}

	lighthouse := provider.grant(t, "-all,+upgradoor,+upgradoor:team_recover:team_hold", "lighthouse")
	teku := provider.grant(t, "-all,+upgradoor,+upgradoor:team_*", "teku")
	supervisor := provider.grant(t, "+upgradoor:supervise")
	staff := provider.grant(t, staffServices, "panda")

	retry := func(id string, hosts ...string) string {
		if len(hosts) == 0 {
			return `{"rollout":"` + id + `"}`
		}

		return `{"rollout":"` + id + `","hosts":["` + strings.Join(hosts, `","`) + `"]}`
	}

	// H1: a team member cannot reopen an aborted cause by naming a host.
	code, msg := send("retry", retry("r-aborted", hostMixed), lighthouse)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "forbidden: this login lacks the feature upgradoor:supervise", msg)

	code, _ = send("retry", retry("r-aborted", hostTeku), teku)
	assert.Equal(t, http.StatusForbidden, code)

	// With the feature, hosts are still refused: the scheduler does not
	// reopen a cause half.
	code, msg = send("retry", retry("r-aborted", hostMixed), supervisor)
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, msg, "is aborted; retrying it reopens its cause for every host")

	assert.Equal(t, 1, open(), "nothing was reopened")
	assert.Zero(t, actions())

	// H2: a covered host that did not fail lifts no halt.
	code, msg = send("retry", retry("r-halted", hostUnowned), staff)
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, msg, "no host could be retried: bootnode-1 has neither failed nor been skipped")

	// A team retries its own failed host: the other team's keeps the halt.
	res := ok("retry", retry("r-halted", hostMixed), lighthouse)
	assert.Equal(t, []string{hostMixed}, res.Hosts)
	assert.Contains(t, res.Message, "the rollout stays halted: teku-geth-1 still failed")
	assert.Equal(t, fleet.RolloutHalted, rollout("r-halted").State)
	assert.Equal(t, fleet.HostPending, rollout("r-halted").Host(hostMixed).State)

	// Again: it is not failed any more, so nothing changes.
	code, _ = send("retry", retry("r-halted", hostMixed), lighthouse)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, fleet.RolloutHalted, rollout("r-halted").State)

	// The last failed host: the halt is lifted.
	res = ok("retry", retry("r-halted", hostTeku), teku)
	assert.Equal(t, []string{hostTeku}, res.Hosts)
	assert.Equal(t, fleet.RolloutActive, rollout("r-halted").State)
	assert.Equal(t, 2, actions())

	// Holds. A team's hold: any owner of the host lifts it.
	ok("hold", `{"hosts":["`+hostTeku+`"],"reason":"ours"}`, teku)

	host, err := st.GetHost(ctx, hostTeku)
	require.NoError(t, err)
	assert.Equal(t, fleet.HoldByTeam, host.HoldBy)

	ok("unhold", `{"hosts":["`+hostTeku+`"]}`, teku)

	// Staff's hold: the twin does not lift it and does not take it over.
	ok("hold", `{"hosts":["`+hostMixed+`"],"reason":"incident"}`, staff)

	for action, verb := range map[string]string{"unhold": "lift", "hold": "change"} {
		code, msg = send(action, `{"hosts":["`+hostMixed+`"]}`, lighthouse)
		assert.Equal(t, http.StatusForbidden, code)
		assert.Equal(t, "forbidden: upgradoor:team_hold does not "+verb+
			" the hold on host lighthouse-geth-1 (set by staff)", msg)
	}

	host, err = st.GetHost(ctx, hostMixed)
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "incident", host.HoldReason)
	assert.Empty(t, host.HoldBy)

	rec := h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts/" + hostMixed}.request(lighthouse))
	assert.Equal(t, "staff", decodeJSON[HostDetail](t, rec).HoldBy)

	ok("unhold", `{"hosts":["`+hostMixed+`"]}`, staff)

	// Through the twin a roll is staggered, whatever was asked.
	rollToken := provider.grant(t, "+upgradoor:team_roll", "lighthouse")
	res = ok("roll", `{"hosts":["`+hostMixed+`"],"containers":["lighthouse"],"staggered":false}`, rollToken)
	assert.Contains(t, res.Message, "staggered")
	assert.False(t, rollout(res.Rollout).Force)

	res = ok("roll", `{"hosts":["`+hostMixed+`"],"containers":["lighthouse"]}`, staff)
	assert.Contains(t, res.Message, "now")
	assert.True(t, rollout(res.Rollout).Force)
}

// TestAuditNamesWhatPermittedIt: admin is recorded whenever it is what
// permitted the request as it was executed: the flags, the selector all, and
// "now" for a login that acts through the team twin.
func TestAuditNamesWhatPermittedIt(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	roll := func(extra string) string {
		return `{"hosts":["` + hostMixed + `"],"containers":["lighthouse"]` + extra + `}`
	}
	reboot := func(extra string) string { return `{"hosts":["` + hostMixed + `"]` + extra + `}` }
	lighthouse := []string{"lighthouse"}

	for _, tt := range []authzCase{
		{
			name: "roll now, twin and admin", action: "roll", body: roll(""),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll now, every twin and admin", action: "roll", body: roll(`,"staggered":false`),
			services: "+upgradoor:team_*:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll staggered, twin and admin", action: "roll", body: roll(`,"staggered":true`),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll now, twin alone: run staggered", action: "roll", body: roll(""),
			services: "+upgradoor:team_roll", teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll staggered with force, twin and admin: named once", action: "roll",
			body:     roll(`,"staggered":true,"force":true`),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll now with ignoreBudget, twin and admin: named once", action: "roll",
			body:     roll(`,"ignoreBudget":true`),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll now, plain and admin: now is the plain feature's", action: "roll", body: roll(""),
			services: "+upgradoor:roll:admin", teams: lighthouse, by: "roll",
		},
		{
			name: "roll now, everything", action: "roll", body: roll(""),
			services: "+upgradoor:all", teams: lighthouse, by: "roll",
		},
		{
			name: "reboot now, twin and admin", action: "reboot", body: reboot(""),
			services: "+upgradoor:team_reboot:admin", teams: lighthouse, by: "team_reboot,admin",
		},
		{
			name: "reboot staggered, twin and admin", action: "reboot", body: reboot(`,"staggered":true`),
			services: "+upgradoor:team_reboot:admin", teams: lighthouse, by: "team_reboot",
		},
		{
			name: "hold, twin and admin: admin permitted nothing", action: "hold", body: reboot(""),
			services: "+upgradoor:team_hold:admin", teams: lighthouse, by: "team_hold",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}
}

// TestSchedulerRefusalIsForbidden: what the scheduler refuses under its lock
// for the authorization of the request is answered like a refusal of the
// API: 403, the service name, the same text.
func TestSchedulerRefusalIsForbidden(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.AuthService = "upgradoor-dev" })
	ownershipFixture(h)

	h.sched.set(func(f *fakeScheduler) {
		f.actionErr = fmt.Errorf("%w: team_hold does not lift the hold on host %s (set by staff)",
			scheduler.ErrForbidden, hostMixed)
	})

	req := httptest.NewRequest(http.MethodPost, testBaseURL+"/api/v1/x",
		strings.NewReader(`{"hosts":["`+hostMixed+`"]}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.srv.actions()["unhold"](rec, req, &user{
		actor: userEmail, services: "+upgradoor-dev:team_hold", teams: []string{"geth"},
	})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrorBody{
		Error: "forbidden: upgradoor-dev:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
	}, decodeJSON[ErrorBody](t, rec))
}

// TestTeamHoldDoesNotSurviveEndLease is the sequence of the review, through
// the real handlers, token verification, scheduler and store: a team member
// holds the team's host with the reason the end of a lease writes, ends the
// lease, and cannot lift the hold afterwards.
func TestTeamHoldDoesNotSurviveEndLease(t *testing.T) {
	h, provider, st := realHarness(t)
	ctx := context.Background()

	syncOwned(t, h, hostMixed, owned("geth", "geth"), owned("lighthouse", "lighthouse"))

	require.NoError(t, st.Commit(ctx, &store.Changes{Leases: []*fleet.Lease{{
		ID: "lease-1", Host: hostMixed, State: fleet.LeaseRunning, GrantedBy: "scheduler",
		Needs:     []protocol.Need{{ID: "reboot", Kind: protocol.NeedReboot}},
		Rollouts:  map[string]string{},
		GrantedAt: testEpoch, ExpiresAt: testEpoch.Add(time.Hour),
	}}}))

	send := func(action, body, authorization string) *httptest.ResponseRecorder {
		return h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))
	}

	host := func() *fleet.Host {
		got, err := st.GetHost(ctx, hostMixed)
		require.NoError(t, err)

		return got
	}

	team := provider.grant(t, "-all,+upgradoor,+upgradoor:team_*", "lighthouse")
	hosts := `{"hosts":["` + hostMixed + `"]}`

	rec := send("hold", `{"hosts":["`+hostMixed+`"],"reason":"lease ended by operator"}`, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, fleet.HoldByTeam, host().HoldBy)

	rec = send("end-lease", `{"lease":"lease-1"}`, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.True(t, host().HoldWanted)
	assert.Empty(t, host().HoldBy, "the hold of an ended lease is staff's, whatever its reason")

	entries, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action.end-lease"})
	require.NoError(t, err)
	require.Len(t, entries, 1)

	hold, ok := entries[0].Detail["hold"].(map[string]any)
	require.True(t, ok, "the change of the hold is audited: %v", entries[0].Detail)
	assert.Equal(t, map[string]any{
		"holdWanted": true, "holdReason": "lease ended by operator", "holdBy": "team",
	}, hold["before"])
	assert.Equal(t, map[string]any{
		"holdWanted": true, "holdReason": "lease ended by operator", "holdBy": "staff",
	}, hold["after"])

	// While the lease is open, and after it has ended: not the team's to lift.
	for range 2 {
		rec = send("unhold", hosts, team)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, ErrorBody{
			Error: "forbidden: upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
		}, decodeJSON[ErrorBody](t, rec))

		lease, getErr := st.GetLease(ctx, "lease-1")
		require.NoError(t, getErr)

		ended := testEpoch
		lease.State = fleet.LeaseFailed
		lease.EndedAt = &ended
		require.NoError(t, st.Commit(ctx, &store.Changes{Leases: []*fleet.Lease{lease}}))
	}

	assert.True(t, host().HoldWanted)

	rec = h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts/" + hostMixed}.request(team))
	assert.Equal(t, "staff", decodeJSON[HostDetail](t, rec).HoldBy)

	// Staff lifts it.
	rec = send("unhold", hosts, provider.grant(t, "+upgradoor:hold"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, host().HoldWanted)

	// Lifting it again changes nothing and is not audited.
	before, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
	require.NoError(t, err)

	rec = send("unhold", hosts, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, decodeJSON[scheduler.ActionResult](t, rec).Message, "1 host(s) were not held")

	after, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
	require.NoError(t, err)
	assert.Len(t, after, len(before))
}
