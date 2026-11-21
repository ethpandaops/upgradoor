package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestHosts(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	// One host has been silent for longer than hostStaleAfter (2m).
	h.store.hosts["lighthouse-geth-2"].LastSeen = testEpoch.Add(-3 * time.Minute)

	rec := h.get("/api/v1/hosts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	hosts := decodeJSON[Hosts](t, rec).Hosts
	require.Len(t, hosts, 3)

	boot, one, two := hosts[0], hosts[1], hosts[2]

	assert.Equal(t, "bootnode-1", boot.Name)
	assert.Equal(t, []string{"bootnode"}, boot.Roles)
	assert.Zero(t, boot.Validators)
	assert.Equal(t, scheduler.HostPlan{Slot: scheduler.SlotIdle}, boot.Plan,
		"a host the plan does not know is idle")

	assert.Equal(t, "lighthouse-geth-1", one.Name)
	assert.Equal(t, uint64(100), one.Validators)
	assert.False(t, one.Stale)
	assert.Equal(t, secretAgentVersion, one.AgentVersion)
	assert.Len(t, one.Containers, 4)
	assert.Equal(t, secretKernelRunning, one.OS.RunningKernel)
	assert.Equal(t, protocol.Pending{Upgradable: 9, Security: 4, Blocked: 1}, one.Pending)
	assert.Equal(t, int64(86400), one.UptimeSeconds)
	assert.True(t, one.Hold.Local)
	assert.True(t, one.HoldWanted)
	assert.Equal(t, secretHoldReason, one.HoldReason, "the reason of the controller hold, for those who are signed in")
	assert.Empty(t, boot.HoldReason)
	assert.Equal(t, []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages}, one.Verbs)
	assert.Len(t, one.Needs, 2)
	assert.Equal(t, scheduler.SlotHeld, one.Plan.Slot)
	assert.Equal(t, uint64(90), one.Plan.Live)

	assert.Equal(t, "lighthouse-geth-2", two.Name)
	assert.True(t, two.Stale)
	assert.Empty(t, two.Needs)

	// Lists are [] and never null, also for a host without a status.
	var raw struct {
		Hosts []map[string]json.RawMessage `json:"hosts"`
	}

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))

	for _, key := range []string{"containers", "verbs", "needs"} {
		assert.Equal(t, "[]", string(raw.Hosts[0][key]), key)
	}

	// A host whose certificate carried no roles.
	h.store.hosts["bootnode-1"].Roles = nil
	assert.Contains(t, h.get("/api/v1/hosts").Body.String(), `{"name":"bootnode-1","roles":[],`)
}

func TestHostDetail(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	h.store.leases = append(h.store.leases, &fleet.Lease{
		ID: "lease-other", Host: "lighthouse-geth-2", State: fleet.LeaseReleased,
	})
	h.store.audit = append(h.store.audit, fleet.AuditEntry{
		ID: 2, Time: testEpoch, Actor: "scheduler", Action: "lease.granted",
		Target: "lighthouse-geth-2",
	})

	rec := h.get("/api/v1/hosts/lighthouse-geth-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	detail := decodeJSON[HostDetail](t, rec)
	assert.Equal(t, "lighthouse-geth-1", detail.Name)
	assert.Equal(t, secretCertHash, detail.CertHash)
	assert.Equal(t, uint64(3), detail.Generation)
	assert.Equal(t, secretAddr, detail.RemoteAddr)
	assert.True(t, detail.FirstSeen.Equal(testEpoch.Add(-48*time.Hour)))
	require.NotNil(t, detail.ValidatorStart)
	require.NotNil(t, detail.ValidatorEnd)
	assert.Equal(t, uint64(0), *detail.ValidatorStart)
	assert.Equal(t, uint64(100), *detail.ValidatorEnd)
	require.NotNil(t, detail.Inventory)
	assert.Equal(t, secretPkgName, detail.Inventory.Packages[0].Name)
	require.Len(t, detail.Leases, 1)
	assert.Equal(t, "lease-zz", detail.Leases[0].ID)
	require.Len(t, detail.History, 1)
	assert.Equal(t, "action.hold", detail.History[0].Action)

	// A host without validators, inventory, leases or history.
	rec = h.get("/api/v1/hosts/bootnode-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))

	for _, key := range []string{"leases", "history"} {
		assert.Equal(t, "[]", string(raw[key]), key)
	}

	for _, key := range []string{"inventory", "validatorStart", "validatorEnd"} {
		assert.NotContains(t, raw, key)
	}
}

func TestHostDetailErrors(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get("/api/v1/hosts/nope")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, ErrorBody{Error: "unknown host"}, decodeJSON[ErrorBody](t, rec))

	h.store.setErr(errors.New("secret: database is locked"))

	for _, path := range []string{"/api/v1/hosts", "/api/v1/hosts/nope", "/api/v1/rollouts",
		"/api/v1/packages?q=abc", "/api/v1/audit"} {
		rec = h.get(path)
		require.Equal(t, http.StatusInternalServerError, rec.Code, path)
		assert.Equal(t, ErrorBody{Error: "internal error"}, decodeJSON[ErrorBody](t, rec), path)
	}
}

func TestPackages(t *testing.T) {
	h := newHarness(t, nil)
	h.store.packages = []store.PackageMatch{
		{Name: "openssl", Versions: []store.PackageVersion{
			{Version: "3.5.1-1", Hosts: []string{"a", "b"}},
			{Version: "3.5.4-1", Hosts: []string{"c"}},
		}},
		{Name: "libssl3t64", Versions: []store.PackageVersion{
			{Version: "3.5.1-1", Hosts: []string{"a"}},
		}},
	}

	tests := []struct {
		name      string
		query     string
		status    int
		wantNames []string
		wantQuery string
		wantLimit int
	}{
		{
			name: "match", query: "?q=openssl", status: http.StatusOK,
			wantNames: []string{"openssl"}, wantQuery: "openssl", wantLimit: store.DefaultPackageSearchLimit,
		},
		{
			name: "substring, trimmed", query: "?q=%20ssl%20&limit=7", status: http.StatusOK,
			wantNames: []string{"openssl", "libssl3t64"}, wantQuery: "ssl", wantLimit: 7,
		},
		{
			name: "limit is capped", query: "?q=ssl&limit=100000", status: http.StatusOK,
			wantNames: []string{"openssl", "libssl3t64"}, wantQuery: "ssl",
			wantLimit: store.MaxPackageSearchLimit,
		},
		{
			name: "no match is an empty list", query: "?q=nothing", status: http.StatusOK,
			wantNames: []string{}, wantQuery: "nothing", wantLimit: store.DefaultPackageSearchLimit,
		},
		{
			name: "no query lists everything", query: "", status: http.StatusOK,
			wantNames: []string{"openssl", "libssl3t64"}, wantLimit: store.DefaultPackageSearchLimit,
		},
		{
			name: "one character filters", query: "?q=o", status: http.StatusOK,
			wantNames: []string{"openssl"}, wantQuery: "o", wantLimit: store.DefaultPackageSearchLimit,
		},
		{name: "not a cursor", query: "?after=x", status: http.StatusBadRequest},
		{
			name: "query too long", query: "?q=" + strings.Repeat("a", maxPackageQuery+1),
			status: http.StatusBadRequest,
		},
		{name: "bad limit", query: "?q=ssl&limit=many", status: http.StatusBadRequest},
		{name: "zero limit", query: "?q=ssl&limit=0", status: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := h.get("/api/v1/packages" + tt.query)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			if tt.status != http.StatusOK {
				assert.NotEmpty(t, decodeJSON[ErrorBody](t, rec).Error)

				return
			}

			out := decodeJSON[Packages](t, rec)
			assert.Equal(t, tt.wantQuery, out.Query)

			names := make([]string, 0, len(out.Matches))
			for _, m := range out.Matches {
				names = append(names, m.Name)
			}

			assert.Equal(t, tt.wantNames, names)
			assert.Equal(t, tt.wantQuery, h.store.lastSearchQuery)
			assert.Equal(t, tt.wantLimit, h.store.lastSearchLimit)
			assert.NotContains(t, rec.Body.String(), "null")
		})
	}
}

// The list is paged: the first page says how many there are and where the
// next one starts; the last page has no next.
func TestPackagesPaging(t *testing.T) {
	h := newHarness(t, nil)

	for _, name := range []string{"acl", "adduser", "apt", "base-files", "bash"} {
		h.store.packages = append(h.store.packages, store.PackageMatch{
			Name: name, Versions: []store.PackageVersion{{Version: "1", Hosts: []string{"a"}}},
		})
	}

	names := func(p Packages) []string {
		out := make([]string, 0, len(p.Matches))
		for _, m := range p.Matches {
			out = append(out, m.Name)
		}

		return out
	}

	first := decodeJSON[Packages](t, h.get("/api/v1/packages?limit=2"))
	assert.Equal(t, []string{"acl", "adduser"}, names(first))
	require.NotNil(t, first.Total)
	assert.Equal(t, 5, *first.Total)
	require.NotEmpty(t, first.Next)

	second := decodeJSON[Packages](t, h.get("/api/v1/packages?limit=2&after="+url.QueryEscape(first.Next)))
	assert.Equal(t, []string{"apt", "base-files"}, names(second))
	assert.Nil(t, second.Total, "counted once, for the first page")

	last := decodeJSON[Packages](t, h.get("/api/v1/packages?limit=2&after="+url.QueryEscape(second.Next)))
	assert.Equal(t, []string{"bash"}, names(last))
	assert.Empty(t, last.Next)

	filtered := decodeJSON[Packages](t, h.get("/api/v1/packages?q=a&limit=2"))
	assert.Equal(t, 5, *filtered.Total)
	assert.Equal(t, "a", filtered.Query)
}

func TestRollouts(t *testing.T) {
	h := newHarness(t, nil)

	// Newest first, open and closed interleaved.
	states := []fleet.RolloutState{
		fleet.RolloutActive, fleet.RolloutCompleted, fleet.RolloutHalted,
		fleet.RolloutSuperseded, fleet.RolloutAborted, fleet.RolloutCompleted,
	}
	for i, state := range states {
		h.store.rollouts = append(h.store.rollouts, &fleet.Rollout{
			ID: fmt.Sprintf("r%d", i), Kind: fleet.RolloutImage, State: state,
			Hosts: []fleet.RolloutHost{},
		})
	}

	leaseStates := []fleet.LeaseState{
		fleet.LeaseRunning, fleet.LeaseReleased, fleet.LeaseFailed, fleet.LeaseVerifying,
		fleet.LeaseExpired,
	}
	for i, state := range leaseStates {
		h.store.leases = append(h.store.leases, &fleet.Lease{
			ID: fmt.Sprintf("l%d", i), Host: "a", State: state,
		})
	}

	ids := func(out Rollouts) (open, recent, leases, recentLeases []string) {
		for _, r := range out.Open {
			open = append(open, r.ID)
		}

		for _, r := range out.Recent {
			recent = append(recent, r.ID)
		}

		for _, l := range out.Leases {
			leases = append(leases, l.ID)
		}

		for _, l := range out.RecentLeases {
			recentLeases = append(recentLeases, l.ID)
		}

		return open, recent, leases, recentLeases
	}

	rec := h.get("/api/v1/rollouts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	open, recent, leases, recentLeases := ids(decodeJSON[Rollouts](t, rec))
	assert.Equal(t, []string{"r0", "r2"}, open)
	assert.Equal(t, []string{"r1", "r3", "r4", "r5"}, recent)
	assert.Equal(t, []string{"l0", "l3"}, leases)
	assert.Equal(t, []string{"l1", "l2", "l4"}, recentLeases)

	// The limit bounds the closed ones only; open ones are always complete.
	rec = h.get("/api/v1/rollouts?limit=2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	open, recent, leases, recentLeases = ids(decodeJSON[Rollouts](t, rec))
	assert.Equal(t, []string{"r0", "r2"}, open)
	assert.Equal(t, []string{"r1", "r3"}, recent)
	assert.Equal(t, []string{"l0", "l3"}, leases)
	assert.Equal(t, []string{"l1", "l2"}, recentLeases)

	assert.Equal(t, http.StatusBadRequest, h.get("/api/v1/rollouts?limit=-1").Code)

	// Empty store: four empty lists.
	h = newHarness(t, nil)
	assert.JSONEq(t, `{"open":[],"recent":[],"leases":[],"recentLeases":[]}`,
		h.get("/api/v1/rollouts").Body.String())
}

func TestPlan(t *testing.T) {
	h := newHarness(t, nil)
	h.beacon.health = fleet.NetworkHealth{CurrentEpoch: 7, FinalizedEpoch: 5, FinalityLagEpochs: 2}

	// Before the first reconcile: an empty plan with a closed gate.
	rec := h.get("/api/v1/plan")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	plan := decodeJSON[scheduler.Plan](t, rec)
	assert.False(t, plan.Gate.OK)
	assert.Equal(t, uint64(7), plan.Gate.Network.CurrentEpoch)
	assert.Contains(t, rec.Body.String(), `"queue":[]`)
	assert.Contains(t, rec.Body.String(), `"hosts":{}`)

	leakFixture(t, h)

	rec = h.get("/api/v1/plan")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	plan = decodeJSON[scheduler.Plan](t, rec)
	assert.True(t, plan.ComputedAt.Equal(testEpoch))
	assert.Equal(t, uint64(10), plan.Budget.UnavailableWeight)
	require.Len(t, plan.Queue, 1)
	assert.Equal(t, "lighthouse-geth-1", plan.Queue[0].Host)
	assert.Equal(t, scheduler.SlotHeld, plan.Hosts["lighthouse-geth-1"].Slot)

	// A plan with nil lists still encodes them as empty.
	h.sched.set(func(f *fakeScheduler) { f.plan = &scheduler.Plan{ComputedAt: testEpoch} })

	body := h.get("/api/v1/plan").Body.String()
	assert.Contains(t, body, `"queue":[]`)
	assert.Contains(t, body, `"hosts":{}`)
	assert.Contains(t, body, `"reasons":[]`)
}

func TestSettings(t *testing.T) {
	defaults := fleet.DefaultSettings()
	defaults.ReadyEpochs = 2

	h := newHarness(t, func(o *Options) { o.DefaultSettings = defaults })

	rec := h.get("/api/v1/settings")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, SettingsBody{Settings: fleet.DefaultSettings(), Defaults: defaults},
		decodeJSON[SettingsBody](t, rec))

	changed := fleet.DefaultSettings()
	changed.MaxUnavailablePercent = 25
	changed.LeaseTTL = fleet.Duration(45 * time.Minute)
	changed.PolicyReboot = fleet.PolicyAuto
	changed.PauseAfterCanaryImage = true
	changed.ReadyPeerShare = 0.25
	changed.Paused = true

	body, err := json.Marshal(changed)
	require.NoError(t, err)
	require.Contains(t, string(body), `"leaseTtl":"45m0s"`)
	require.Contains(t, string(body), `"pauseAfterCanaryImage":true`)
	require.Contains(t, string(body), `"pauseAfterCanaryPackages":false`)
	require.Contains(t, string(body), `"readyPeerShare":0.25`)

	rec = h.sendJSON(http.MethodPut, "/api/v1/settings", string(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, SettingsBody{Settings: changed, Defaults: defaults},
		decodeJSON[SettingsBody](t, rec))

	calls := h.sched.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, schedulerCall{
		Name: "settings", Actor: actorAnonymous, Req: changed, Auth: openAuth,
	}, calls[0])

	assert.Equal(t, changed, decodeJSON[SettingsBody](t, h.get("/api/v1/settings")).Settings)
}

// A PUT changes the keys it names and no others: a page of an earlier build,
// which does not know the newer settings and leaves them out, resets none of
// them (nor did it reset the others before, but it did reset these).
func TestSettingsPutKeepsWhatItDoesNotName(t *testing.T) {
	stored := fleet.DefaultSettings()
	stored.ReadyPeerShare = 0.25
	stored.PauseAfterCanaryImage = true
	stored.PauseAfterCanaryReboot = true
	stored.Paused = true
	stored.MinPeers = 5

	put := func(t *testing.T, body string) (fleet.Settings, *harness) {
		t.Helper()

		h := newHarness(t, nil)
		h.sched.set(func(f *fakeScheduler) { f.settings = stored })

		rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got := decodeJSON[SettingsBody](t, rec).Settings

		calls := h.sched.recorded()
		require.Len(t, calls, 1)
		assert.Equal(t, schedulerCall{Name: "settings", Actor: actorAnonymous, Req: got, Auth: openAuth},
			calls[0], "the scheduler gets, and audits, the whole set")

		return got, h
	}

	t.Run("the body of a page that knows neither the peer share nor the pause switches", func(t *testing.T) {
		old := `{"maxUnavailablePercent":20,"minParticipationPercent":90,"maxFinalityLagEpochs":3,"readyEpochs":1,` +
			`"minPeers":3,"leaseTtl":"30m","hostStaleAfter":"2m","readinessTimeout":"30m","policyImage":"auto",` +
			`"policyPackages":"auto","policyReboot":"manual","paused":false}`

		got, _ := put(t, old)

		want := stored
		want.MaxUnavailablePercent = 20
		want.MinPeers = 3
		want.Paused = false
		assert.Equal(t, want, got, "what it names is changed; the peer share and the switches are as they were")
	})

	t.Run("one key changes that key", func(t *testing.T) {
		got, _ := put(t, `{"readyPeerShare":0.4}`)

		want := stored
		want.ReadyPeerShare = 0.4
		assert.Equal(t, want, got)
	})

	t.Run("explicit values change them, also to off", func(t *testing.T) {
		got, _ := put(t, `{"readyPeerShare":0,"pauseAfterCanaryImage":false,"pauseAfterCanaryPackages":true}`)

		want := stored
		want.ReadyPeerShare = 0
		want.PauseAfterCanaryImage = false
		want.PauseAfterCanaryPackages = true
		assert.Equal(t, want, got)
	})

	t.Run("no key, or only what is set already, changes nothing and reaches no audit entry", func(t *testing.T) {
		same, err := json.Marshal(stored)
		require.NoError(t, err)

		for _, body := range []string{`{}`, string(same), `{"minPeers":5,"paused":true}`} {
			h := newHarness(t, nil)
			h.sched.set(func(f *fakeScheduler) { f.settings = stored })

			rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, stored, decodeJSON[SettingsBody](t, rec).Settings)
			assert.Empty(t, h.sched.recorded(), body)
		}
	})

	t.Run("a refused body changes nothing", func(t *testing.T) {
		h := newHarness(t, nil)
		h.sched.set(func(f *fakeScheduler) { f.settings = stored })

		for _, body := range []string{
			`{"readyPeerShare":0.4,"paused":null}`, `{"readyPeerShare":0.4,"minPeers":0}`,
			`{"readyPeerShare":0.4,"leaseTtl":"soon"}`, `{"readyPeerShare":0.4,"nosuch":1}`,
		} {
			rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, body)
		}

		assert.Empty(t, h.sched.recorded())
		assert.Equal(t, stored, decodeJSON[SettingsBody](t, h.get("/api/v1/settings")).Settings)
	})
}

func TestSettingsErrors(t *testing.T) {
	valid, err := json.Marshal(fleet.DefaultSettings())
	require.NoError(t, err)

	withField := func(key, value string) string {
		var m map[string]json.RawMessage

		require.NoError(t, json.Unmarshal(valid, &m))

		m[key] = json.RawMessage(value)

		out, merr := json.Marshal(m)
		require.NoError(t, merr)

		return string(out)
	}

	tests := []struct {
		name        string
		body        string
		contentType string
		schedErr    error
		status      int
		contains    string
	}{
		{
			name: "out of range", body: withField("maxUnavailablePercent", "101"),
			status: http.StatusBadRequest, contains: "maxUnavailablePercent",
		},
		{
			name: "finality lag below the healthy value",
			body: withField("maxFinalityLagEpochs", "1"), status: http.StatusBadRequest,
			contains: "maxFinalityLagEpochs",
		},
		{
			name: "peer share above 1", body: withField("readyPeerShare", "1.5"),
			status: http.StatusBadRequest, contains: "readyPeerShare must be between 0 and 1",
		},
		{
			name: "peer share below 0", body: withField("readyPeerShare", "-0.5"),
			status: http.StatusBadRequest, contains: "readyPeerShare must be between 0 and 1",
		},
		{
			name: "a pause switch that is no boolean", body: withField("pauseAfterCanaryReboot", `"yes"`),
			status: http.StatusBadRequest, contains: "pauseAfterCanaryReboot",
		},
		{
			name: "unknown policy", body: withField("policyImage", `"yolo"`),
			status: http.StatusBadRequest, contains: "invalid policy",
		},
		{
			name: "bad duration", body: withField("leaseTtl", `"soon"`),
			status: http.StatusBadRequest, contains: "invalid duration",
		},
		{
			name: "null for a key", body: withField("readyPeerShare", "null"),
			status: http.StatusBadRequest, contains: "readyPeerShare must not be null",
		},
		{
			name: "null for a key that an old page knows", body: `{"paused":null}`,
			status: http.StatusBadRequest, contains: "paused must not be null",
		},
		{name: "null", body: "null", status: http.StatusBadRequest, contains: "must be an object"},
		{name: "not an object", body: "[]", status: http.StatusBadRequest, contains: "invalid JSON"},
		{
			name: "one key that makes the whole invalid", body: `{"minPeers":0}`,
			status: http.StatusBadRequest, contains: "minPeers",
		},
		{
			name: "unknown field", body: withField("maxUnavailable", "10"),
			status: http.StatusBadRequest, contains: `unknown setting "maxUnavailable"`,
		},
		{
			name: "not json", body: "paused=true", status: http.StatusBadRequest,
			contains: "invalid JSON",
		},
		{
			name: "trailing data", body: string(valid) + "{}",
			status: http.StatusBadRequest, contains: "trailing data",
		},
		{name: "empty body", body: "", status: http.StatusBadRequest, contains: "invalid JSON"},
		{
			name: "wrong content type", body: string(valid), contentType: "text/plain",
			status: http.StatusUnsupportedMediaType, contains: "application/json",
		},
		{
			name: "form post", body: string(valid),
			contentType: "application/x-www-form-urlencoded",
			status:      http.StatusUnsupportedMediaType, contains: "application/json",
		},
		{
			name: "too large", body: withField("policyImage",
				`"`+strings.Repeat("a", maxUserBody)+`"`),
			status: http.StatusRequestEntityTooLarge, contains: "too large",
		},
		{
			name: "scheduler refuses", body: withField("minPeers", "7"),
			schedErr: fmt.Errorf("%w: a rollout is running", scheduler.ErrInvalidAction),
			status:   http.StatusConflict, contains: "a rollout is running",
		},
		{
			name: "scheduler fails", body: withField("minPeers", "7"),
			schedErr: errors.New("secret: database is locked"),
			status:   http.StatusInternalServerError, contains: "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.sched.set(func(f *fakeScheduler) { f.actionErr = tt.schedErr })

			req := httptest.NewRequest(http.MethodPut, testBaseURL+"/api/v1/settings",
				strings.NewReader(tt.body))

			contentType := tt.contentType
			if contentType == "" {
				contentType = "application/json; charset=utf-8"
			}

			req.Header.Set("Content-Type", contentType)

			rec := h.serve(req)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			msg := decodeJSON[ErrorBody](t, rec).Error
			assert.Contains(t, msg, tt.contains)
			assert.NotContains(t, msg, "secret")
			assert.NotContains(t, msg, "bad request:")

			if tt.schedErr == nil {
				assert.Empty(t, h.sched.recorded(), "invalid settings never reach the scheduler")
			}

			assert.Equal(t, fleet.DefaultSettings(),
				decodeJSON[SettingsBody](t, h.get("/api/v1/settings")).Settings)
		})
	}
}

func TestAuditPaging(t *testing.T) {
	h := newHarness(t, nil)

	for i := 1; i <= 25; i++ {
		entry := fleet.AuditEntry{
			ID: int64(i), Time: testEpoch.Add(time.Duration(i) * time.Second),
			Actor: "scheduler", Action: "lease.granted", Target: "host-a",
		}

		if i%5 == 0 {
			entry.Actor, entry.Action, entry.Target = userEmail, "action.hold", "host-b"
			entry.Detail = map[string]any{"reason": "maintenance"}
		}

		h.store.audit = append(h.store.audit, entry)
	}

	page := func(query string) Audit {
		rec := h.get("/api/v1/audit" + query)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		return decodeJSON[Audit](t, rec)
	}

	ids := func(a Audit) []int64 {
		out := make([]int64, 0, len(a.Entries))
		for _, e := range a.Entries {
			out = append(out, e.ID)
		}

		return out
	}

	// Everything fits in the default page.
	all := page("")
	assert.Len(t, all.Entries, 25)
	assert.Equal(t, int64(25), all.Entries[0].ID, "newest first")
	assert.Zero(t, all.NextBefore)

	// Walk the trail in pages of 10.
	first := page("?limit=10")
	assert.Equal(t, []int64{25, 24, 23, 22, 21, 20, 19, 18, 17, 16}, ids(first))
	assert.Equal(t, int64(16), first.NextBefore)

	second := page(fmt.Sprintf("?limit=10&before=%d", first.NextBefore))
	assert.Equal(t, []int64{15, 14, 13, 12, 11, 10, 9, 8, 7, 6}, ids(second))
	assert.Equal(t, int64(6), second.NextBefore)

	third := page(fmt.Sprintf("?limit=10&before=%d", second.NextBefore))
	assert.Equal(t, []int64{5, 4, 3, 2, 1}, ids(third))
	assert.Zero(t, third.NextBefore, "the last page ends the walk")

	// A page that ends exactly at the end has no next page.
	exact := page("?limit=5&before=6")
	assert.Equal(t, []int64{5, 4, 3, 2, 1}, ids(exact))
	assert.Zero(t, exact.NextBefore)

	// Filters.
	byTarget := page("?target=host-b")
	assert.Equal(t, []int64{25, 20, 15, 10, 5}, ids(byTarget))
	assert.Equal(t, "maintenance", byTarget.Entries[0].Detail["reason"])

	byAction := page("?action=action.&limit=2")
	assert.Equal(t, []int64{25, 20}, ids(byAction))
	assert.Equal(t, int64(20), byAction.NextBefore)

	assert.Empty(t, page("?target=nobody").Entries)
	assert.JSONEq(t, `{"entries":[],"nextBefore":0}`,
		h.get("/api/v1/audit?target=nobody").Body.String())

	// The limit is capped.
	for i := 26; i <= store.MaxAuditLimit+50; i++ {
		h.store.audit = append(h.store.audit, fleet.AuditEntry{ID: int64(i), Action: "x"})
	}

	capped := page("?limit=100000")
	assert.Len(t, capped.Entries, store.MaxAuditLimit)
	assert.Equal(t, capped.Entries[store.MaxAuditLimit-1].ID, capped.NextBefore)

	for _, bad := range []string{
		"?limit=0", "?limit=x", "?before=x", "?before=-4",
		"?target=" + strings.Repeat("a", maxAuditFilter+1),
	} {
		rec := h.get("/api/v1/audit" + bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
}
