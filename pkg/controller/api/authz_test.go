package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The fleet of the authorization tests.
const (
	// hostMixed runs geth (owner geth), lighthouse and lighthouse-validator
	// (owner lighthouse) and an unmanaged container "debug".
	hostMixed = "lighthouse-geth-1"
	// hostTeku runs geth (owner geth) and teku (owners teku and nimbus).
	hostTeku = "teku-geth-1"
	// hostUnowned runs one managed container, "bootnode", nobody owns.
	hostUnowned = "bootnode-1"
	// hostFresh has registered and never synced.
	hostFresh = "fresh-1"
	// hostGhost is not known to the controller.
	hostGhost = "ghost-1"

	// leaseMixed is open on hostMixed, leaseUnowned on hostUnowned, leaseDone
	// has ended on hostMixed, leaseGhost does not exist.
	leaseMixed   = "lease-mixed"
	leaseUnowned = "lease-unowned"
	leaseDone    = "lease-done"
	leaseGhost   = "lease-ghost"

	// actionSettings stands for PUT /api/v1/settings in the tables.
	actionSettings = "settings"
)

func owned(name string, owners ...string) protocol.Container {
	return protocol.Container{Name: name, State: "running", Managed: true, Owners: owners}
}

// ownershipFixture fills the fake store with the fleet above.
func ownershipFixture(h *harness) {
	status := func(containers ...protocol.Container) *protocol.SyncRequest {
		req := syncRequest()
		req.Containers = containers

		return req
	}

	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	h.store.hosts[hostMixed] = &fleet.Host{Name: hostMixed, LastSeen: testEpoch, Status: status(
		owned("geth", "geth"),
		owned("lighthouse", "lighthouse"),
		owned("lighthouse-validator", "lighthouse"),
		protocol.Container{Name: "debug", State: "exited"},
	)}
	h.store.hosts[hostTeku] = &fleet.Host{Name: hostTeku, LastSeen: testEpoch, Status: status(
		owned("geth", "geth"),
		owned("teku", "nimbus", "teku"),
	)}
	h.store.hosts[hostUnowned] = &fleet.Host{Name: hostUnowned, LastSeen: testEpoch, Status: status(
		owned("bootnode"),
	)}
	h.store.hosts[hostFresh] = &fleet.Host{Name: hostFresh, LastSeen: testEpoch}

	// r-1 is halted, r-aborted is aborted; r-ghost does not exist.
	h.store.rollouts = []*fleet.Rollout{
		{ID: "r-1", State: fleet.RolloutHalted},
		{ID: "r-aborted", State: fleet.RolloutAborted},
	}

	h.store.leases = []*fleet.Lease{
		{ID: leaseMixed, Host: hostMixed, State: fleet.LeaseRunning},
		{ID: leaseUnowned, Host: hostUnowned, State: fleet.LeaseGranted},
		{ID: leaseDone, Host: hostMixed, State: fleet.LeaseReleased},
	}
}

// authzCase is one request by one user.
type authzCase struct {
	name   string
	action string
	body   string
	// services is the token's "services" claim; teams its teams; open stands
	// for a controller without an auth provider.
	services string
	teams    []string
	open     bool
	// by is what the audit log records for an allowed request. Empty means
	// the request is refused, with exactly refusal or, when that is empty,
	// with something that starts with refusalPrefix.
	by            string
	refusal       string
	refusalPrefix string
}

// settingsBody is a body of PUT /api/v1/settings that changes a setting of
// the defaults.
func settingsBody(t *testing.T) string {
	t.Helper()

	return `{"minPeers":7}`
}

// runAuthz sends the request of one case to the action's handler as the
// case's user: the handler the route serves, without the token verification
// in front of it.
func runAuthz(t *testing.T, h *harness, tt *authzCase) {
	t.Helper()

	handler, method := h.srv.handlePutSettings, http.MethodPut

	// Every settings case starts from the defaults, so its body is a change.
	h.sched.set(func(f *fakeScheduler) { f.settings = fleet.DefaultSettings() })

	if tt.action != actionSettings {
		var ok bool

		handler, ok = h.srv.actions()[tt.action]
		require.True(t, ok, "no action %q", tt.action)

		method = http.MethodPost
	}

	req := httptest.NewRequest(method, testBaseURL+"/api/v1/x", strings.NewReader(tt.body))
	req.Header.Set("Content-Type", "application/json")

	u := &user{actor: userEmail, open: tt.open, teams: tt.teams, services: tt.services}
	if tt.open {
		u.actor = actorAnonymous
	}

	before := h.sched.recorded()
	audit := h.store.auditEntries()

	rec := httptest.NewRecorder()
	handler(rec, req, u)

	calls := h.sched.recorded()

	if tt.by != "" {
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Len(t, calls, len(before)+1, "the scheduler was called once")

		call := calls[len(before)]
		assert.Equal(t, u.actor, call.Actor)
		assert.Equal(t, scheduler.Authorization{By: tt.by, Teams: tt.teams}, call.Auth)

		return
	}

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	got := decodeJSON[ErrorBody](t, rec).Error

	if tt.refusal != "" {
		assert.Equal(t, tt.refusal, got)
	} else {
		require.NotEmpty(t, tt.refusalPrefix, "the case names no expectation")
		assert.True(t, strings.HasPrefix(got, tt.refusalPrefix), "%q does not start with %q",
			got, tt.refusalPrefix)
	}

	assert.Len(t, calls, len(before), "a refused request does not reach the scheduler")
	assert.Equal(t, audit, h.store.auditEntries(), "a refused request is not audited")
}

// actionSample is one request per kind of action, on targets the team
// lighthouse covers, with the feature the action declares.
type actionSample struct {
	action  string
	body    string
	feature string
	// twin: the feature has a team twin.
	twin bool
}

func actionSamples(t *testing.T) []actionSample {
	t.Helper()

	hosts := fmt.Sprintf(`{"hosts":[%q]}`, hostMixed)
	rolloutHosts := fmt.Sprintf(`{"rollout":"r-1","hosts":[%q]}`, hostMixed)

	return []actionSample{
		{
			action:  "roll",
			body:    fmt.Sprintf(`{"hosts":[%q],"containers":["lighthouse"]}`, hostMixed),
			feature: FeatureRoll, twin: true,
		},
		{action: "reboot", body: hosts, feature: FeatureReboot, twin: true},
		{action: "hold", body: hosts, feature: FeatureHold, twin: true},
		{action: "unhold", body: hosts, feature: FeatureHold, twin: true},
		{action: "skip", body: rolloutHosts, feature: FeatureRecover, twin: true},
		{action: "retry", body: rolloutHosts, feature: FeatureRecover, twin: true},
		{
			action: "end-lease", body: fmt.Sprintf(`{"lease":%q}`, leaseMixed),
			feature: FeatureRecover, twin: true,
		},
		{action: "pause", body: `{"rollout":"r-1"}`, feature: FeatureSupervise},
		{action: "resume", body: `{"rollout":"r-1"}`, feature: FeatureSupervise},
		{action: "abort", body: `{"rollout":"r-1"}`, feature: FeatureSupervise},
		{action: "retry", body: `{"rollout":"r-1"}`, feature: FeatureSupervise},
		{action: "pause", body: `{}`, feature: FeatureAdmin},
		{action: "resume", body: `{"reason":"over"}`, feature: FeatureAdmin},
		{action: "forget", body: hosts, feature: FeatureAdmin},
		{action: actionSettings, body: settingsBody(t), feature: FeatureAdmin},
	}
}

// TestAuthorizeEveryAction runs every action against every kind of login.
func TestAuthorizeEveryAction(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)

	samples := actionSamples(t)

	// Every registered action is in the samples, so a new action cannot be
	// added without a row here.
	sampled := make(map[string]struct{}, len(samples))
	for _, s := range samples {
		sampled[s.action] = struct{}{}
	}

	delete(sampled, actionSettings)
	assert.ElementsMatch(t, slices.Collect(maps.Keys(h.srv.actions())),
		slices.Collect(maps.Keys(sampled)))

	owners := []string{"lighthouse"}
	strangers := []string{"besu", "reth"}

	for _, s := range samples {
		lacks := "forbidden: this login lacks the feature upgradoor:" + s.feature
		twin := teamFeaturePrefix + s.feature

		// What the team twin of the action's feature gets: the action, or
		// nothing when the feature has no twin.
		viaTeam := authzCase{refusal: lacks}
		if s.twin {
			viaTeam = authzCase{by: twin}
		}

		admin := authzCase{refusal: lacks}
		if s.feature == FeatureAdmin {
			admin = authzCase{by: FeatureAdmin}
		}

		notCovered := authzCase{refusal: lacks}
		if s.twin {
			notCovered = authzCase{refusalPrefix: "forbidden: upgradoor:" + twin + " does not cover "}
		}

		cases := []authzCase{
			{name: "open mode", open: true, by: authorizedByOpen},
			{name: "open mode ignores a claim", open: true, services: "-all", by: authorizedByOpen},
			{name: "no services claim", refusal: lacks},
			{name: "no services claim, in the owning team", teams: owners, refusal: lacks},
			{name: "basic only", services: "+upgradoor", teams: owners, refusal: lacks},
			{name: "basic spelled out", services: "+upgradoor:basic", teams: owners, refusal: lacks},
			{name: "only upgradoor, basic", services: "-all,+upgradoor", teams: owners, refusal: lacks},
			{name: "plain feature", services: "+upgradoor:" + s.feature, by: s.feature},
			{
				name: "plain feature, in no owning team", services: "+upgradoor:" + s.feature,
				teams: strangers, by: s.feature,
			},
			{
				name: "team twin, owning team", services: "+upgradoor:" + twin, teams: owners,
				by: viaTeam.by, refusal: viaTeam.refusal,
			},
			{
				name: "team twin, other teams", services: "+upgradoor:" + twin, teams: strangers,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name: "team twin, no teams", services: "+upgradoor:" + twin,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name: "plain and twin, other teams", teams: strangers,
				services: "+upgradoor:" + s.feature + ":" + twin, by: s.feature,
			},
			{name: "upgradoor:all", services: "+upgradoor:all", by: s.feature},
			{name: "upgradoor:all, twin spelled too", services: "+upgradoor:all:" + twin, by: s.feature},
			{name: "all:all", services: "+all:all", teams: strangers, by: s.feature},
			{name: "only upgradoor:all", services: "-all,+upgradoor:all", by: s.feature},
			{
				name: "upgradoor:team_*, owning team", services: "+upgradoor:team_*", teams: owners,
				by: viaTeam.by, refusal: viaTeam.refusal,
			},
			{
				name: "upgradoor:team_*, other teams", services: "+upgradoor:team_*", teams: strangers,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{name: "another service only", services: "+spamoor:all", teams: owners, refusal: lacks},
			{name: "no access to this service", services: "-all,+spamoor:all", teams: owners, refusal: lacks},
			{name: "service denied after the grant", services: "+upgradoor:all,-upgradoor", refusal: lacks},
			// "all" without the plain feature still holds the twin, which
			// covers nothing for a login without teams.
			{
				name: "all but this feature", services: "+upgradoor:all:-" + s.feature,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name: "all, this feature removed later", services: "+all:all,-upgradoor:" + s.feature,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name:     "all but this feature and its twin",
				services: "+upgradoor:all:-" + s.feature + ":-" + twin, teams: owners, refusal: lacks,
			},
			{
				name: "admin alone implies nothing", services: "+upgradoor:admin", teams: owners,
				by: admin.by, refusal: admin.refusal,
			},
			{name: "a malformed grant", services: "+upgradoor:" + s.feature + ":", refusal: lacks},
		}

		for _, tt := range cases {
			tt.action, tt.body = s.action, s.body

			t.Run(s.action+" "+s.body+"/"+tt.name, func(t *testing.T) {
				runAuthz(t, h, &tt)
			})
		}
	}
}

// TestAuthorizeTargets: what the team twins cover, and what needs admin.
func TestAuthorizeTargets(t *testing.T) {
	hosts := func(names ...string) string {
		raw, err := json.Marshal(names)
		require.NoError(t, err)

		return string(raw)
	}

	teamRoll, teamReboot := "+upgradoor:team_roll", "+upgradoor:team_reboot"
	teamRecover := "+upgradoor:team_recover"
	lighthouse, geth := []string{"lighthouse"}, []string{"geth"}
	both := []string{"geth", "lighthouse"}

	rollMixed := func(containers string) string {
		return `{"hosts":` + hosts(hostMixed) + `,"containers":` + containers + `}`
	}

	tests := []authzCase{
		// A roll with named containers: every one of them.
		{
			name: "roll, own container", action: "roll", body: rollMixed(`["lighthouse"]`),
			services: teamRoll, teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll, own containers", action: "roll",
			body:     rollMixed(`["lighthouse","lighthouse-validator"]`),
			services: teamRoll, teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll, mixed ownership", action: "roll", body: rollMixed(`["lighthouse","geth"]`),
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll, mixed ownership, in both teams", action: "roll",
			body:     rollMixed(`["lighthouse","geth"]`),
			services: teamRoll, teams: both, by: "team_roll",
		},
		{
			name: "roll, the other team's container", action: "roll", body: rollMixed(`["geth"]`),
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll, a container of two teams", action: "roll",
			body:     `{"hosts":` + hosts(hostTeku) + `,"containers":["teku"]}`,
			services: teamRoll, teams: []string{"nimbus"}, by: "team_roll",
		},
		{
			name: "roll, a container of two teams, a third team", action: "roll",
			body:     `{"hosts":` + hosts(hostTeku) + `,"containers":["teku"]}`,
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container teku " +
				"on host teku-geth-1 (owners: nimbus, teku)",
		},
		{
			name: "roll, one container on two hosts", action: "roll",
			body:     `{"hosts":` + hosts(hostMixed, hostTeku) + `,"containers":["geth"]}`,
			services: teamRoll, teams: geth, by: "team_roll",
		},
		{
			name: "roll, a container one host does not have", action: "roll",
			body:     `{"hosts":` + hosts(hostMixed, hostUnowned) + `,"containers":["geth"]}`,
			services: teamRoll, teams: geth,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "roll, unknown container", action: "roll", body: rollMixed(`["nethermind"]`),
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container nethermind " +
				"on host lighthouse-geth-1 (owners: none)",
		},
		{
			name: "roll, unmanaged container", action: "roll", body: rollMixed(`["debug"]`),
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container debug " +
				"on host lighthouse-geth-1 (owners: none)",
		},
		{
			name: "roll, unknown container, plain feature", action: "roll",
			body: rollMixed(`["nethermind"]`), services: "+upgradoor:roll", by: "roll",
		},

		// A roll without containers: every managed container of the host.
		{
			name: "roll a host, one team of two", action: "roll", body: `{"hosts":` + hosts(hostMixed) + `}`,
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll a host, both teams", action: "roll", body: `{"hosts":` + hosts(hostMixed) + `}`,
			services: teamRoll, teams: both, by: "team_roll",
		},
		{
			name: "roll hosts, second not covered", action: "roll",
			body:     `{"hosts":` + hosts(hostMixed, hostTeku) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container teku " +
				"on host teku-geth-1 (owners: nimbus, teku)",
		},
		{
			name: "roll a host nobody owns", action: "roll", body: `{"hosts":` + hosts(hostUnowned) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "roll a host that never synced", action: "roll", body: `{"hosts":` + hosts(hostFresh) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover host fresh-1 (owners: none)",
		},
		{
			name: "roll an unknown host", action: "roll", body: `{"hosts":` + hosts(hostGhost) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover host ghost-1 (owners: none)",
		},
		{
			name: "roll an unknown host, named container", action: "roll",
			body:     `{"hosts":` + hosts(hostGhost) + `,"containers":["geth"]}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host ghost-1 (owners: none)",
		},
		{
			// The scheduler answers the unknown host, as before.
			name: "roll an unknown host, plain feature", action: "roll",
			body: `{"hosts":` + hosts(hostGhost) + `}`, services: "+upgradoor:roll", by: "roll",
		},
		{
			name: "roll, no target", action: "roll", body: `{}`, services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll needs a target that one of your teams owns, " +
				"and the request names none",
		},

		// Host-level actions: one owned container is enough.
		{
			name: "reboot, the CL team", action: "reboot", body: `{"hosts":` + hosts(hostMixed) + `}`,
			services: teamReboot, teams: lighthouse, by: "team_reboot",
		},
		{
			name: "reboot, the EL team", action: "reboot", body: `{"hosts":` + hosts(hostMixed) + `}`,
			services: teamReboot, teams: geth, by: "team_reboot",
		},
		{
			name: "reboot, a team without a container there", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed) + `}`,
			services: teamReboot, teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "reboot two hosts, the EL team", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed, hostTeku) + `}`,
			services: teamReboot, teams: geth, by: "team_reboot",
		},
		{
			name: "reboot two hosts, second not covered", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed, hostTeku) + `}`,
			services: teamReboot, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_reboot does not cover host teku-geth-1 " +
				"(owners: geth, nimbus, teku)",
		},
		{
			name: "reboot a host nobody owns", action: "reboot", body: `{"hosts":` + hosts(hostUnowned) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "reboot a host that never synced", action: "reboot",
			body:     `{"hosts":` + hosts(hostFresh) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: upgradoor:team_reboot does not cover host fresh-1 (owners: none)",
		},
		{
			name: "reboot an unknown host", action: "reboot", body: `{"hosts":` + hosts(hostGhost) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: upgradoor:team_reboot does not cover host ghost-1 (owners: none)",
		},
		{
			name: "reboot an unknown host, plain feature", action: "reboot",
			body: `{"hosts":` + hosts(hostGhost) + `}`, services: "+upgradoor:reboot", by: "reboot",
		},
		{
			name: "hold, no target", action: "hold", body: `{"hosts":[]}`,
			services: "+upgradoor:team_hold", teams: both,
			refusal: "forbidden: upgradoor:team_hold needs a target that one of your teams owns, " +
				"and the request names none",
		},
		{
			name: "unhold, a host nobody owns", action: "unhold", body: `{"hosts":` + hosts(hostUnowned) + `}`,
			services: "+upgradoor:team_hold", teams: both,
			refusal: "forbidden: upgradoor:team_hold does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "the twin of another feature", action: "hold", body: `{"hosts":` + hosts(hostMixed) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: this login lacks the feature upgradoor:hold",
		},

		// The flags and the selector all need admin on top.
		{
			name: "reboot with force, no admin", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed) + `,"force":true}`,
			services: "+upgradoor:reboot",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "reboot with force, admin", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed) + `,"force":true}`,
			services: "+upgradoor:reboot:admin", by: "reboot,admin",
		},
		{
			name: "reboot with force, admin without reboot", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed) + `,"force":true}`,
			services: "+upgradoor:admin",
			refusal:  "forbidden: this login lacks the feature upgradoor:reboot",
		},
		{
			name: "reboot with force, team twin and admin", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed) + `,"force":true}`,
			services: "+upgradoor:team_reboot:admin", teams: geth, by: "team_reboot,admin",
		},
		{
			name: "reboot with force, team twin without admin", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed) + `,"force":true}`,
			services: teamReboot, teams: geth,
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "reboot with force, team twin and admin, not covered", action: "reboot",
			body:     `{"hosts":` + hosts(hostUnowned) + `,"force":true}`,
			services: "+upgradoor:team_reboot:admin", teams: geth,
			refusal: "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "reboot with force, everything", action: "reboot",
			body:     `{"hosts":` + hosts(hostMixed) + `,"force":true}`,
			services: "+upgradoor:all", by: "reboot,admin",
		},
		{
			name: "roll with ignoreBudget, no admin", action: "roll",
			body:     rollMixed(`["lighthouse"]`)[:len(rollMixed(`["lighthouse"]`))-1] + `,"ignoreBudget":true}`,
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for ignoreBudget)",
		},
		{
			name: "roll with ignoreBudget, admin", action: "roll",
			body:     rollMixed(`["lighthouse"]`)[:len(rollMixed(`["lighthouse"]`))-1] + `,"ignoreBudget":true}`,
			services: "+upgradoor:team_*:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll with force, plain", action: "roll", body: `{"hosts":` + hosts(hostMixed) + `,"force":true}`,
			services: "+upgradoor:roll",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "roll, staggered is no flag", action: "roll",
			body:     `{"hosts":` + hosts(hostMixed) + `,"staggered":true}`,
			services: "+upgradoor:roll", by: "roll",
		},
		{
			name: "roll all, plain", action: "roll", body: `{"all":true}`, services: "+upgradoor:roll",
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for all)",
		},
		{
			name: "roll all, admin", action: "roll", body: `{"all":true}`,
			services: "+upgradoor:roll:admin", by: "roll,admin",
		},
		{
			name: "roll all, team twin and admin: not every host is covered", action: "roll",
			body: `{"all":true}`, services: "+upgradoor:team_roll:admin", teams: both,
			refusalPrefix: "forbidden: upgradoor:team_roll does not cover ",
		},
		{
			name: "reboot all, plain", action: "reboot", body: `{"all":true}`, services: "+upgradoor:reboot",
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for all)",
		},
		{
			name: "reboot all, admin", action: "reboot", body: `{"all":true,"staggered":true}`,
			services: "+upgradoor:reboot:admin", by: "reboot,admin",
		},

		// One rollout, or everything.
		{
			name: "pause one rollout with admin only", action: "pause", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:admin",
			refusal:  "forbidden: this login lacks the feature upgradoor:supervise",
		},
		{
			name: "pause everything with supervise only", action: "pause", body: `{}`,
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin",
		},
		{
			name: "resume everything with supervise only", action: "resume", body: `{}`,
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin",
		},
		{
			name: "abort with a team twin", action: "abort", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:team_*", teams: both,
			refusal: "forbidden: this login lacks the feature upgradoor:supervise",
		},

		// Retry: hosts are a recovery, none is the whole rollout.
		{
			name: "retry hosts, team", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(hostMixed) + `}`,
			services: teamRecover, teams: geth, by: "team_recover",
		},
		{
			name: "retry hosts, team, not covered", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(hostMixed, hostUnowned) + `}`,
			services: teamRecover, teams: geth,
			refusal: "forbidden: upgradoor:team_recover does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "retry the rollout, team", action: "retry", body: `{"rollout":"r-1"}`,
			services: teamRecover, teams: both,
			refusal: "forbidden: this login lacks the feature upgradoor:supervise",
		},
		{
			name: "retry the rollout, recover", action: "retry", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:recover",
			refusal:  "forbidden: this login lacks the feature upgradoor:supervise",
		},
		{
			name: "retry hosts, rollouts", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(hostMixed) + `}`,
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:recover",
		},
		{
			name: "skip, team, not covered", action: "skip",
			body:     `{"rollout":"r-1","hosts":` + hosts(hostTeku) + `}`,
			services: teamRecover, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_recover does not cover host teku-geth-1 " +
				"(owners: geth, nimbus, teku)",
		},

		// A lease stands for its host.
		{
			name: "end a lease of an owned host", action: "end-lease", body: `{"lease":"` + leaseMixed + `"}`,
			services: teamRecover, teams: geth, by: "team_recover",
		},
		{
			name: "end a closed lease of an owned host", action: "end-lease",
			body: `{"lease":"` + leaseDone + `"}`, services: teamRecover, teams: geth, by: "team_recover",
		},
		{
			name: "end a lease of another team's host", action: "end-lease",
			body:     `{"lease":"` + leaseMixed + `"}`,
			services: teamRecover, teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_recover does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "end a lease of a host nobody owns", action: "end-lease",
			body:     `{"lease":"` + leaseUnowned + `"}`,
			services: teamRecover, teams: both,
			refusal: "forbidden: upgradoor:team_recover does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "end an unknown lease", action: "end-lease", body: `{"lease":"` + leaseGhost + `"}`,
			services: teamRecover, teams: both,
			refusal: "forbidden: upgradoor:team_recover does not cover lease lease-ghost (owners: none)",
		},
		{
			name: "end an unknown lease, plain feature", action: "end-lease",
			body: `{"lease":"` + leaseGhost + `"}`, services: "+upgradoor:recover", by: "recover",
		},
		{
			name: "end no lease", action: "end-lease", body: `{}`, services: teamRecover, teams: both,
			refusal: "forbidden: upgradoor:team_recover needs a target that one of your teams owns, " +
				"and the request names none",
		},
	}

	h := newHarness(t, nil)
	ownershipFixture(h)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}
}

// TestAuthorizeConfiguredService: the features are features of the service
// the controller was told it is.
func TestAuthorizeConfiguredService(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.AuthService = "upgradoor-dev" })
	ownershipFixture(h)

	body := `{"hosts":["` + hostMixed + `"]}`

	for _, tt := range []authzCase{
		{
			name: "the default name grants nothing", services: "+upgradoor:all",
			refusal: "forbidden: this login lacks the feature upgradoor-dev:hold",
		},
		{name: "the configured name", services: "+upgradoor-dev:hold", by: "hold"},
		{name: "all services", services: "+all:all", by: "hold"},
		{
			name: "the team twin", services: "+upgradoor-dev:team_hold", teams: []string{"geth"},
			by: "team_hold",
		},
		{
			name: "the team twin, not covered", services: "+upgradoor-dev:team_hold",
			teams: []string{"teku"},
			refusal: "forbidden: upgradoor-dev:team_hold does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
	} {
		tt.action, tt.body = "hold", body

		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	assert.Equal(t, "upgradoor-dev", decodeJSON[Config](t, h.get("/api/v1/config")).AuthService)

	for _, bad := range []string{"Upgradoor", "upgradoor:all", "up*", "+upgradoor", "a,b", " ", "a.b"} {
		opts := h.srv.opts
		opts.AuthService = bad
		opts.Registry = nil

		_, err := New(h.srv.log, opts)
		require.ErrorContains(t, err, "is not a service name", bad)
	}
}

// TestAuthorizeStoreError: when ownership cannot be read, the request fails;
// it is not allowed and it is not blamed on the login.
func TestAuthorizeStoreError(t *testing.T) {
	h := newHarness(t, nil)
	ownershipFixture(h)
	h.store.setErr(assert.AnError)

	for _, tt := range []struct{ action, body string }{
		{"hold", `{"hosts":["` + hostMixed + `"]}`},
		{"end-lease", `{"lease":"` + leaseMixed + `"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, testBaseURL+"/api/v1/x", strings.NewReader(tt.body))
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		h.srv.actions()[tt.action](rec, req, &user{
			actor: userEmail, services: "+upgradoor:team_*", teams: []string{"geth"},
		})

		require.Equal(t, http.StatusInternalServerError, rec.Code, tt.action)
		assert.Equal(t, ErrorBody{Error: "internal error"}, decodeJSON[ErrorBody](t, rec))
	}

	assert.Empty(t, h.sched.recorded())
}

// TestFeatureNames: the names are what a grant spells, a twin is the name
// with the prefix, and none collides with what the grammar reserves.
func TestFeatureNames(t *testing.T) {
	features := []string{
		FeatureRoll, FeatureReboot, FeatureHold, FeatureRecover, FeatureSupervise,
		FeatureAdmin,
	}

	for _, f := range features {
		assert.NotEqual(t, auth.FeatureBasic, f)
		assert.NotEqual(t, "all", f)
		assert.False(t, strings.HasPrefix(f, teamFeaturePrefix), f)

		for _, name := range []string{f, teamFeaturePrefix + f} {
			assert.True(t, auth.FeatureAllowed("+upgradoor:"+name, "upgradoor", name,
				auth.FeatureDefaultDeny), name)
			assert.False(t, auth.FeatureAllowed("+upgradoor", "upgradoor", name,
				auth.FeatureDefaultDeny), name)
		}
	}

	// A prefix grant covers every feature that starts with it. No name starts
	// with another, so a grant that spells a feature and adds "*" gives that
	// feature only; and no twin is reached by a prefix of a plain feature.
	all := slices.Clone(features)
	for _, f := range features {
		all = append(all, teamFeaturePrefix+f)
	}

	for _, f := range features {
		granted := make([]string, 0, 1)

		for _, other := range all {
			if auth.FeatureAllowed("+upgradoor:"+f+"*", "upgradoor", other, auth.FeatureDefaultDeny) {
				granted = append(granted, other)
			}
		}

		assert.Equal(t, []string{f}, granted, "upgradoor:%s*", f)
	}

	// What a short prefix does reach, so that nobody writes it by accident.
	assert.True(t, auth.FeatureAllowed("+upgradoor:r*", "upgradoor", FeatureRecover, auth.FeatureDefaultDeny))
	assert.True(t, auth.FeatureAllowed("+upgradoor:r*", "upgradoor", FeatureReboot, auth.FeatureDefaultDeny))
	assert.False(t, auth.FeatureAllowed("+upgradoor:team_*", "upgradoor", FeatureRoll, auth.FeatureDefaultDeny))

	// The team names of a token and the owners of a container are the same
	// kind of name.
	for _, name := range []string{"geth", "a.b_c-d", "0x", "Geth", "", "-a", "a b", "a:b", "a*"} {
		assert.Equal(t, auth.ValidTeam(name), protocol.ValidOwner(name), "%q", name)
	}
}
