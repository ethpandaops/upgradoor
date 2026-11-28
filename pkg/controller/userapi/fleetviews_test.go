package userapi

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

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestHosts(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	// One host has been silent for longer than hostStaleAfter (2m).
	h.store.Hosts["lighthouse-geth-2"].LastSeen = controllertest.Epoch.Add(-3 * time.Minute)

	rec := h.get("/api/v1/hosts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	hosts := controllertest.DecodeJSON[views.Hosts](t, rec).Hosts
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
	assert.Equal(t, controllertest.SecretAgentVersion, one.AgentVersion)
	assert.Len(t, one.Containers, 4)
	assert.Equal(t, controllertest.SecretKernelRunning, one.OS.RunningKernel)
	assert.Equal(t, protocol.Pending{Upgradable: 9, Security: 4, Blocked: 1}, one.Pending)
	assert.Equal(t, int64(86400), one.UptimeSeconds)
	assert.True(t, one.Hold.Local)
	assert.Equal(t, []string{protocol.HolderHost, fleet.HolderStaff}, one.Holders)
	assert.Empty(t, boot.Holders)
	assert.Equal(t, []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages}, one.Verbs)
	assert.Len(t, one.Needs, 3)
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
	h.store.Hosts["bootnode-1"].Roles = nil
	assert.Contains(t, h.get("/api/v1/hosts").Body.String(), `{"name":"bootnode-1","roles":[],`)
}

func TestHostDetail(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	h.store.Leases = append(h.store.Leases, &fleet.Lease{
		ID: "lease-other", Host: "lighthouse-geth-2", State: fleet.LeaseReleased,
	})
	h.store.Audit = append(h.store.Audit, fleet.AuditEntry{
		ID: 2, Time: controllertest.Epoch, Actor: "scheduler", Action: "lease.granted",
		Target: "lighthouse-geth-2",
	})

	rec := h.get("/api/v1/hosts/lighthouse-geth-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	detail := controllertest.DecodeJSON[views.HostDetail](t, rec)
	assert.Equal(t, "lighthouse-geth-1", detail.Name)
	assert.Equal(t, controllertest.SecretCertHash, detail.CertHash)
	assert.Equal(t, controllertest.SecretAddr, detail.RemoteAddr)
	assert.True(t, detail.FirstSeen.Equal(controllertest.Epoch.Add(-48*time.Hour)))
	assert.Equal(t, uint64(100), detail.Assigned)
	require.NotNil(t, detail.Inventory)
	assert.Equal(t, controllertest.SecretPkgName, detail.Inventory.Packages[0].Name)
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
	assert.Equal(t, httpjson.ErrorBody{Error: "unknown host"}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))

	h.store.SetErr(errors.New("secret: database is locked"))

	for _, path := range []string{"/api/v1/hosts", "/api/v1/hosts/nope", "/api/v1/rollouts",
		"/api/v1/packages?q=abc", "/api/v1/audit"} {
		rec = h.get(path)
		require.Equal(t, http.StatusInternalServerError, rec.Code, path)
		assert.Equal(t, httpjson.ErrorBody{Error: "internal error"}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec), path)
	}
}

func TestPackages(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Packages = []store.PackageMatch{
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
		{name: "unknown show", query: "?show=newest", status: http.StatusBadRequest},
		{name: "unknown sort", query: "?sort=size", status: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := h.get("/api/v1/packages" + tt.query)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			if tt.status != http.StatusOK {
				assert.NotEmpty(t, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error)

				return
			}

			out := controllertest.DecodeJSON[views.Packages](t, rec)
			assert.Equal(t, tt.wantQuery, out.Query)

			names := make([]string, 0, len(out.Matches))
			for _, m := range out.Matches {
				names = append(names, m.Name)
			}

			assert.Equal(t, tt.wantNames, names)
			assert.Equal(t, tt.wantQuery, h.store.LastSearchQuery)
			assert.Equal(t, tt.wantLimit, h.store.LastSearchLimit)
			assert.NotContains(t, rec.Body.String(), "null")
		})
	}
}

// The list is paged: the first page says how many there are and where the
// next one starts; the last page has no next.
func TestImages(t *testing.T) {
	h := newHarness(t, nil)

	for name, digest := range map[string]string{"a": "sha256:1", "b": "sha256:1", "c": "sha256:2"} {
		h.store.Hosts[name] = &fleet.Host{Name: name, LastSeen: controllertest.Epoch, Status: &protocol.SyncRequest{
			Containers: []protocol.Container{
				{Name: "geth", Image: "ethpandaops/geth:master", Digest: digest},
				{Name: "dora", Image: "ethpandaops/dora:latest", Digest: "sha256:d"},
			},
		}}
	}

	var page views.Images

	rec := h.get("/api/v1/images?q=%20geth%20")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	assert.Equal(t, "geth", page.Query)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, []views.ImageVersion{
		{Digest: "sha256:1", Hosts: []string{"a", "b"}}, {Digest: "sha256:2", Hosts: []string{"c"}},
	}, page.Matches[0].Versions)

	rec = h.get("/api/v1/images?limit=1")
	require.Equal(t, http.StatusOK, rec.Code)

	page = views.Images{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Matches, 1)
	require.NotNil(t, page.Total)
	assert.Equal(t, 2, *page.Total)
	require.NotEmpty(t, page.Next)

	rec = h.get("/api/v1/images?limit=1&after=" + url.QueryEscape(page.Next))
	require.Equal(t, http.StatusOK, rec.Code)

	page = views.Images{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "ethpandaops/geth:master", page.Matches[0].Name)
	assert.Empty(t, page.Next)

	for _, bad := range []string{"?after=x", "?limit=0", "?q=" + strings.Repeat("g", 300)} {
		assert.Equal(t, http.StatusBadRequest, h.get("/api/v1/images"+bad).Code, bad)
	}
}

func TestRefreshImages(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.sendJSON(http.MethodPost, "/api/v1/images/refresh", `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"tags":3,"message":"3 tag(s) are looked up now"}`, rec.Body.String())
	assert.Equal(t, 1, h.lookups.Refreshed)

	h.lookups.Refreshable = discovery.Refreshed{Recent: 2, Next: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}

	rec = h.sendJSON(http.MethodPost, "/api/v1/images/refresh", `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"tags":0,"recent":2,"next":"2026-01-02T03:04:05Z",`+
		`"message":"every tag was looked up within the last 30s; `+
		`the next lookup on request is possible at 03:04:05 UTC"}`, rec.Body.String())
}

func TestPackagesPaging(t *testing.T) {
	h := newHarness(t, nil)

	for _, name := range []string{"acl", "adduser", "apt", "base-files", "bash"} {
		h.store.Packages = append(h.store.Packages, store.PackageMatch{
			Name: name, Versions: []store.PackageVersion{{Version: "1", Hosts: []string{"a"}}},
		})
	}

	names := func(p views.Packages) []string {
		out := make([]string, 0, len(p.Matches))
		for _, m := range p.Matches {
			out = append(out, m.Name)
		}

		return out
	}

	first := controllertest.DecodeJSON[views.Packages](t, h.get("/api/v1/packages?limit=2"))
	assert.Equal(t, []string{"acl", "adduser"}, names(first))
	require.NotNil(t, first.Total)
	assert.Equal(t, 5, *first.Total)
	require.NotEmpty(t, first.Next)

	second := controllertest.DecodeJSON[views.Packages](t, h.get("/api/v1/packages?limit=2&after="+url.QueryEscape(first.Next)))
	assert.Equal(t, []string{"apt", "base-files"}, names(second))
	assert.Nil(t, second.Total, "counted once, for the first page")

	last := controllertest.DecodeJSON[views.Packages](t, h.get("/api/v1/packages?limit=2&after="+url.QueryEscape(second.Next)))
	assert.Equal(t, []string{"bash"}, names(last))
	assert.Empty(t, last.Next)

	filtered := controllertest.DecodeJSON[views.Packages](t, h.get("/api/v1/packages?q=a&limit=2"))
	assert.Equal(t, 5, *filtered.Total)
	assert.Equal(t, "a", filtered.Query)
	require.NotNil(t, filtered.Counts)
	assert.Equal(t, 5, filtered.Counts.All)

	controllertest.DecodeJSON[views.Packages](t, h.get("/api/v1/packages?show=outdated&sort=hosts&compact=1"))
	assert.Equal(t, store.PackageFilter{
		Limit: store.DefaultPackageSearchLimit, Show: store.PackageShowOutdated, Sort: store.PackageSortHosts, Compact: true,
	}, h.store.LastPackageFilter)
}

func TestRollouts(t *testing.T) {
	h := newHarness(t, nil)

	// Newest first, open and closed interleaved.
	states := []fleet.RolloutState{
		fleet.RolloutActive, fleet.RolloutCompleted, fleet.RolloutHalted,
		fleet.RolloutSuperseded, fleet.RolloutAborted, fleet.RolloutCompleted,
	}
	for i, state := range states {
		h.store.Rollouts = append(h.store.Rollouts, &fleet.Rollout{
			ID: fmt.Sprintf("r%d", i), Kind: fleet.RolloutGroup, State: state,
			Hosts: []fleet.RolloutHost{},
		})
	}

	leaseStates := []fleet.LeaseState{
		fleet.LeaseRunning, fleet.LeaseReleased, fleet.LeaseFailed, fleet.LeaseVerifying,
		fleet.LeaseExpired,
	}
	for i, state := range leaseStates {
		h.store.Leases = append(h.store.Leases, &fleet.Lease{
			ID: fmt.Sprintf("l%d", i), Host: "a", State: state,
		})
	}

	ids := func(out views.Rollouts) (open, recent, leases, recentLeases []string) {
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

	open, recent, leases, recentLeases := ids(controllertest.DecodeJSON[views.Rollouts](t, rec))
	assert.Equal(t, []string{"r0", "r2"}, open)
	assert.Equal(t, []string{"r1", "r3", "r4", "r5"}, recent)
	assert.Equal(t, []string{"l0", "l3"}, leases)
	assert.Equal(t, []string{"l1", "l2", "l4"}, recentLeases)

	// The limit bounds the closed ones only; open ones are always complete.
	rec = h.get("/api/v1/rollouts?limit=2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	open, recent, leases, recentLeases = ids(controllertest.DecodeJSON[views.Rollouts](t, rec))
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
	h.beacon.Network = fleet.NetworkHealth{CurrentEpoch: 7, FinalizedEpoch: 5, FinalityLagEpochs: 2}

	// Before the first reconcile: an empty plan with a closed gate.
	rec := h.get("/api/v1/plan")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	plan := controllertest.DecodeJSON[scheduler.Plan](t, rec)
	assert.False(t, plan.Gate.OK)
	assert.Equal(t, uint64(7), plan.Gate.Network.CurrentEpoch)
	assert.Contains(t, rec.Body.String(), `"queue":[]`)
	assert.Contains(t, rec.Body.String(), `"hosts":{}`)

	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	rec = h.get("/api/v1/plan")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	plan = controllertest.DecodeJSON[scheduler.Plan](t, rec)
	assert.True(t, plan.ComputedAt.Equal(controllertest.Epoch))
	assert.Equal(t, uint64(10), plan.Budget.UnavailableWeight)
	require.Len(t, plan.Queue, 2)
	assert.Equal(t, "lighthouse-geth-1", plan.Queue[0].Host)
	assert.Equal(t, scheduler.SlotHeld, plan.Hosts["lighthouse-geth-1"].Slot)

	// A waiting reason that quotes the beacon error keeps the fixed text.
	assert.Equal(t, "network gate: beacon node unavailable", plan.Queue[1].Reason)
	assert.Equal(t, "network gate: beacon node unavailable", plan.Hosts["lighthouse-geth-2"].Reason)
	assert.Equal(t, "beacon node unavailable", plan.Gate.Network.Error)
	assert.Equal(t, "the last fetch failed", plan.Gate.Network.ValidatorRangesError)

	// A plan with nil lists still encodes them as empty.
	h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentPlan = &scheduler.Plan{ComputedAt: controllertest.Epoch} })

	body := h.get("/api/v1/plan").Body.String()
	assert.Contains(t, body, `"queue":[]`)
	assert.Contains(t, body, `"hosts":{}`)
	assert.Contains(t, body, `"reasons":[]`)
}

func TestSettings(t *testing.T) {
	defaults := fleet.DefaultSettings()
	defaults.ReadyAttestations = 3

	h := newHarness(t, func(o *Options) { o.DefaultSettings = defaults })

	rec := h.get("/api/v1/settings")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, views.SettingsBody{Settings: fleet.DefaultSettings(), Defaults: defaults, Kinds: []protocol.ContainerKind{}},
		controllertest.DecodeJSON[views.SettingsBody](t, rec))

	changed := fleet.DefaultSettings()
	changed.MaxUnavailablePercent = 25
	changed.LeaseTTL = fleet.Duration(45 * time.Minute)
	changed.PolicyReboot = fleet.PolicyAuto
	changed.PauseAfterCanaryImage = true
	changed.ReadyPeerShare = 0.25
	changed.ReadyAttestations = 1
	changed.ReadyStableFor = 0
	changed.Paused = true

	body, err := json.Marshal(changed)
	require.NoError(t, err)
	require.Contains(t, string(body), `"leaseTtl":"45m0s"`)
	require.Contains(t, string(body), `"pauseAfterCanaryImage":true`)
	require.Contains(t, string(body), `"pauseAfterCanaryPackages":false`)
	require.Contains(t, string(body), `"readyPeerShare":0.25`)
	require.Contains(t, string(body), `"readyStableFor":"0s"`)

	rec = h.sendJSON(http.MethodPut, "/api/v1/settings", string(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, views.SettingsBody{Settings: changed, Defaults: defaults, Kinds: []protocol.ContainerKind{}},
		controllertest.DecodeJSON[views.SettingsBody](t, rec))

	calls := h.sched.Recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, controllertest.Call{
		Name: "settings", Actor: actorAnonymous, Req: changed, Auth: controllertest.OpenAuth,
	}, calls[0])

	assert.Equal(t, changed, controllertest.DecodeJSON[views.SettingsBody](t, h.get("/api/v1/settings")).Settings)
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
		h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentSettings = stored })

		rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got := controllertest.DecodeJSON[views.SettingsBody](t, rec).Settings

		calls := h.sched.Recorded()
		require.Len(t, calls, 1)
		assert.Equal(t, controllertest.Call{Name: "settings", Actor: actorAnonymous, Req: got, Auth: controllertest.OpenAuth},
			calls[0], "the scheduler gets, and audits, the whole set")

		return got, h
	}

	t.Run("the body of a page that knows neither the peer share nor the pause switches", func(t *testing.T) {
		old := `{"maxUnavailablePercent":20,"minParticipationPercent":90,"maxFinalityLagEpochs":3,"readyAttestations":2,` +
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
			h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentSettings = stored })

			rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, stored, controllertest.DecodeJSON[views.SettingsBody](t, rec).Settings)
			assert.Empty(t, h.sched.Recorded(), body)
		}
	})

	t.Run("a refused body changes nothing", func(t *testing.T) {
		h := newHarness(t, nil)
		h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentSettings = stored })

		for _, body := range []string{
			`{"readyPeerShare":0.4,"paused":null}`, `{"readyPeerShare":0.4,"minPeers":0}`,
			`{"readyPeerShare":0.4,"leaseTtl":"soon"}`, `{"readyPeerShare":0.4,"nosuch":1}`,
		} {
			rec := h.sendJSON(http.MethodPut, "/api/v1/settings", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, body)
		}

		assert.Empty(t, h.sched.Recorded())
		assert.Equal(t, stored, controllertest.DecodeJSON[views.SettingsBody](t, h.get("/api/v1/settings")).Settings)
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
			h.sched.Set(func(f *controllertest.Scheduler) { f.ActionErr = tt.schedErr })

			req := httptest.NewRequest(http.MethodPut, controllertest.BaseURL+"/api/v1/settings",
				strings.NewReader(tt.body))

			contentType := tt.contentType
			if contentType == "" {
				contentType = "application/json; charset=utf-8"
			}

			req.Header.Set("Content-Type", contentType)

			rec := h.serve(req)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			msg := controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error
			assert.Contains(t, msg, tt.contains)
			assert.NotContains(t, msg, "secret")
			assert.NotContains(t, msg, "bad request:")

			if tt.schedErr == nil {
				assert.Empty(t, h.sched.Recorded(), "invalid settings never reach the scheduler")
			}

			assert.Equal(t, fleet.DefaultSettings(),
				controllertest.DecodeJSON[views.SettingsBody](t, h.get("/api/v1/settings")).Settings)
		})
	}
}

func TestAuditPaging(t *testing.T) {
	h := newHarness(t, nil)

	for i := 1; i <= 25; i++ {
		entry := fleet.AuditEntry{
			ID: int64(i), Time: controllertest.Epoch.Add(time.Duration(i) * time.Second),
			Actor: "scheduler", Action: "lease.granted", Target: "host-a",
		}

		if i%5 == 0 {
			entry.Actor, entry.Action, entry.Target = controllertest.UserEmail, "action.hold", "host-b"
			entry.Detail = map[string]any{"reason": "maintenance"}
		}

		h.store.Audit = append(h.store.Audit, entry)
	}

	page := func(query string) views.Audit {
		rec := h.get("/api/v1/audit" + query)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		return controllertest.DecodeJSON[views.Audit](t, rec)
	}

	ids := func(a views.Audit) []int64 {
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
		h.store.Audit = append(h.store.Audit, fleet.AuditEntry{ID: int64(i), Action: "x"})
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

// A login without details reads no system data of a host on any read
// route; the containers of its own teams it reads in full, and with
// team_details the hosts those teams own a container on.
func TestHostDetailsFeature(t *testing.T) {
	h, provider := authHarness(t, true)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)
	h.store.Audit = append(h.store.Audit, fleet.AuditEntry{
		ID: 2, Time: controllertest.Epoch, Actor: "agent:lighthouse-geth-1", Action: "host.registered",
		Target: "lighthouse-geth-1",
		Detail: map[string]any{"certHash": controllertest.SecretCertHash, "remoteAddr": controllertest.SecretAddr},
	})

	paths := []string{
		"/api/v1/hosts", "/api/v1/hosts/lighthouse-geth-1", "/api/v1/hosts/lighthouse-geth-2",
		"/api/v1/hosts/bootnode-1", "/api/v1/rollouts", "/api/v1/audit", "/api/v1/images",
		"/api/v1/clients", "/api/v1/clients/" + controllertest.SecretOwnerEL + "/hosts",
		"/api/v1/clients/" + controllertest.SecretOwnerCL + "/hosts", "/api/v1/plan",
		"/api/v1/slots?window=600",
	}

	read := func(path, authorization string) string {
		t.Helper()

		rec := h.serve(userRoute{method: http.MethodGet, path: path}.request(authorization))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		return rec.Body.String()
	}

	packages := func(authorization string) int {
		return h.serve(userRoute{method: http.MethodGet, path: "/api/v1/packages"}.request(authorization)).Code
	}

	t.Run("reduced", func(t *testing.T) {
		basic := provider.Grant(t, "+upgradoor")

		for _, path := range paths {
			raw := read(path, basic)
			assert.NotContains(t, raw, `"resources":`, "%s leaks the resource use", path)
			assert.NotContains(t, raw, `"usage":`, "%s leaks the resource use", path)

			for _, secret := range controllertest.HostDetailSecrets {
				assert.NotContains(t, raw, secret, "%s leaks host details", path)
			}
		}

		detail := controllertest.DecodeJSON[views.HostDetail](t,
			h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts/lighthouse-geth-1"}.request(basic)))
		assert.True(t, detail.Reduced)
		assert.Nil(t, detail.Resources)
		assert.Len(t, detail.Containers, 3, "the managed containers")
		assert.Nil(t, detail.Containers[0].Stats)
		assert.Equal(t, controllertest.SecretVCImage, detail.Containers[2].Image)
		assert.Equal(t, []protocol.NeedKind{protocol.NeedPackages, protocol.NeedReboot},
			[]protocol.NeedKind{detail.Needs[0].Kind, detail.Needs[1].Kind})
		require.NotEmpty(t, detail.Leases)
		assert.Equal(t, controllertest.SecretLeaseError, detail.Leases[0].Error)

		assert.Equal(t, http.StatusForbidden, packages(basic))
		assert.Equal(t, http.StatusForbidden, packages(provider.Grant(t, "+upgradoor:team_details")))
	})

	t.Run("own containers", func(t *testing.T) {
		member := provider.Grant(t, "+upgradoor", controllertest.SecretOwnerEL)

		raw := read("/api/v1/hosts/lighthouse-geth-1", member)
		assert.Contains(t, raw, controllertest.SecretFlag, "the command line of the team's container")
		assert.Contains(t, raw, `"stats":`, "the resource use of the team's container")
		assert.NotContains(t, raw, controllertest.SecretCertHash)
		assert.Contains(t, read("/api/v1/clients/"+controllertest.SecretOwnerEL+"/hosts", member),
			controllertest.SecretFlag)
		assert.Contains(t, read("/api/v1/rollouts", member), controllertest.SecretFlag,
			"the command line change of the team's deploy")
	})

	t.Run("owned hosts", func(t *testing.T) {
		twin := provider.Grant(t, "+upgradoor:team_details", controllertest.SecretOwnerEL)

		raw := read("/api/v1/hosts/lighthouse-geth-1", twin)
		for _, secret := range []string{
			controllertest.SecretCertHash, controllertest.SecretOtherImage, controllertest.SecretPkgName,
			controllertest.SecretActionOutput,
		} {
			assert.Contains(t, raw, secret)
		}

		assert.Contains(t, raw, `"resources":`, "the host's resource use")

		assert.NotContains(t, read("/api/v1/hosts/bootnode-1", twin), controllertest.SecretCertHash,
			"a host without a container of the team")
	})

	t.Run("full", func(t *testing.T) {
		full := provider.Grant(t, "+upgradoor:details")

		raw := read("/api/v1/hosts/lighthouse-geth-1", full)
		for _, secret := range controllertest.HostDetailSecrets {
			if secret != controllertest.SecretBootID && secret != controllertest.SecretAddr6 {
				assert.Contains(t, raw, secret)
			}
		}

		assert.Contains(t, read("/api/v1/audit", full), controllertest.SecretCertHash)
		assert.Equal(t, http.StatusOK, packages(full))
	})
}

// What only the admin pages show (the settings, the data sources, the error
// texts of the beacon service and the inventory, the settings a change left
// alone) is on no read route of a login without admin, details or not.
func TestAdminFeature(t *testing.T) {
	h, provider := authHarness(t, true)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	paths := []string{
		"/api/v1/hosts", "/api/v1/hosts/lighthouse-geth-1", "/api/v1/hosts/lighthouse-geth-2",
		"/api/v1/rollouts", "/api/v1/audit", "/api/v1/images", "/api/v1/clients",
		"/api/v1/clients/" + controllertest.SecretOwnerEL + "/hosts", "/api/v1/plan", "/api/v1/slots?window=600",
	}

	get := func(path, authorization string) *httptest.ResponseRecorder {
		return h.serve(userRoute{method: http.MethodGet, path: path}.request(authorization))
	}

	for _, services := range []string{"+upgradoor", "+upgradoor:details", "+upgradoor:details,manage*"} {
		login := provider.Grant(t, services, controllertest.SecretOwnerEL)

		for _, path := range paths {
			rec := get(path, login)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			for _, secret := range controllertest.AdminSecrets {
				assert.NotContains(t, rec.Body.String(), secret, "%s: %s", services, path)
			}
		}

		for _, path := range []string{"/api/v1/settings", "/api/v1/settings/sources"} {
			assert.Equal(t, http.StatusForbidden, get(path, login).Code, "%s: %s", services, path)
		}

		details := map[string]map[string]any{}
		for _, e := range controllertest.DecodeJSON[views.Audit](t, get("/api/v1/audit", login)).Entries {
			details[e.Action] = e.Detail
		}

		assert.Equal(t, map[string]any{
			"before": map[string]any{"maxUnavailablePercent": 10.0},
			"after":  map[string]any{"maxUnavailablePercent": 20.0},
		}, details["settings.update"], "%s: what the change changed", services)
		assert.Equal(t, map[string]any{"changed": false}, details["sources.validator-ranges.refresh"])
	}

	admin := provider.Grant(t, "+upgradoor:admin")

	rec := get("/api/v1/settings", admin)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	sources := controllertest.DecodeJSON[views.DataSources](t, get("/api/v1/settings/sources", admin))
	assert.Equal(t, controllertest.SecretBeaconError, sources.Chain.Error)

	raw := get("/api/v1/audit", admin).Body.String()
	assert.Contains(t, raw, controllertest.SecretSettingsKey)
	assert.Contains(t, raw, "zz-inventory", "the error of the fetch")

	// The policies that the views need come with the rows.
	rollouts := controllertest.DecodeJSON[views.Rollouts](t, get("/api/v1/rollouts", provider.Grant(t, "+upgradoor")))
	require.Len(t, rollouts.Open, 2)

	for _, r := range rollouts.Open {
		if r.Kind == fleet.RolloutReboot {
			assert.Equal(t, fleet.DefaultSettings().PolicyReboot, r.Policy)
		}
	}

	clients := controllertest.DecodeJSON[views.Clients](t, get("/api/v1/clients", provider.Grant(t, "+upgradoor")))
	require.NotEmpty(t, clients.Clients)
	require.NotEmpty(t, clients.Clients[0].DefaultPolicies)

	for kind, p := range clients.Clients[0].DefaultPolicies {
		assert.Equal(t, fleet.DefaultSettings().PolicyImage, p, kind)
	}
}
