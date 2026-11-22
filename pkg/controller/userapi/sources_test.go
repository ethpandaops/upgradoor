package userapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/beacon"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/validators"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

const (
	pathSources = "/api/v1/settings/sources"
	pathRefresh = "/api/v1/settings/sources/validator-ranges/refresh"
)

type fakeEndpoints []beacon.EndpointStatus

func (f fakeEndpoints) Endpoints() []beacon.EndpointStatus { return f }

// sourcesHarness is an auth harness with a running validator ranges service
// whose URL carries credentials, and a known host node-a.
func sourcesHarness(t *testing.T) (*harness, *controllertest.AuthProvider) {
	t.Helper()

	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ranges": {"0-99": "node-a", "100-149": "node-x", "150-159": "node-a"}}`))
	}))
	t.Cleanup(inventory.Close)

	h, provider := authHarness(t, true)
	h.store.Hosts["node-a"] = &fleet.Host{Name: "node-a"}

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	svc := validators.New(log, validators.Options{
		URL:   strings.Replace(inventory.URL, "http://", "http://inv-user:inv-secret@", 1) + "/ranges.json",
		Store: h.store, Interval: time.Hour, Now: h.clock.Now,
	})
	require.NoError(t, svc.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, svc.Stop()) })
	require.Eventually(t, func() bool { return !svc.Status().SuccessAt.IsZero() }, 5*time.Second, 5*time.Millisecond)

	head := uint64(82)
	h.srv.opts.Validators = svc
	h.srv.opts.Endpoints = fakeEndpoints{
		{
			Label: "http://beacon-1:5052", State: beacon.EndpointOK, SuccessAt: controllertest.Epoch,
			HeadSlot: &head, HeadRoot: "0x01",
		},
		{Label: "http://beacon-2:5052", State: beacon.EndpointDown, Error: "GET /eth/v1/node/health: HTTP 503", ErrorAt: controllertest.Epoch},
	}

	return h, provider
}

func TestSourcesNeedAdmin(t *testing.T) {
	h, provider := sourcesHarness(t)

	for _, route := range []userRoute{
		{method: http.MethodGet, path: pathSources},
		{method: http.MethodPost, path: pathRefresh, body: `{}`},
	} {
		rec := h.serve(route.request(provider.Grant(t, "+upgradoor:manage")))
		require.Equal(t, http.StatusForbidden, rec.Code, route.path)
		assert.Contains(t, rec.Body.String(), "upgradoor:admin")
	}

	for _, e := range h.store.AuditEntries() {
		assert.Equal(t, "auth.refused", e.Action, "a refused request fetches nothing")
	}
}

func TestSources(t *testing.T) {
	h, provider := sourcesHarness(t)

	rec := h.serve(userRoute{method: http.MethodGet, path: pathSources}.request(provider.Grant(t, "+upgradoor:admin")))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "inv-secret")
	assert.NotContains(t, rec.Body.String(), "inv-user")

	got := controllertest.DecodeJSON[views.DataSources](t, rec)

	require.Len(t, got.Beacon, 2)
	assert.Equal(t, beacon.EndpointOK, got.Beacon[0].State)
	assert.Equal(t, "0x01", got.Beacon[0].HeadRoot)
	assert.Equal(t, beacon.EndpointDown, got.Beacon[1].State)
	assert.Equal(t, uint64(82), *got.Beacon[0].HeadSlot)
	assert.Equal(t, "GET /eth/v1/node/health: HTTP 503", got.Beacon[1].Error)

	src := got.ValidatorRanges
	assert.True(t, strings.HasSuffix(src.URL, "/ranges.json"), src.URL)
	assert.Equal(t, controllertest.Epoch, src.SuccessAt)
	assert.Equal(t, controllertest.Epoch, src.ChangedAt)
	assert.Empty(t, src.Error)
	assert.Equal(t, uint64(160), src.Validators)
	assert.Equal(t, 1, src.UnknownHosts)
	assert.Equal(t, []views.ValidatorRangeRow{
		{Start: 0, End: 100, Host: "node-a", Count: 100},
		{Start: 100, End: 150, Host: "node-x", Count: 50, Unknown: true},
		{Start: 150, End: 160, Host: "node-a", Count: 10},
	}, src.Ranges)
	assert.Equal(t, []views.ValidatorHostTotal{
		{Host: "node-a", Ranges: 2, Validators: 110},
		{Host: "node-x", Ranges: 1, Validators: 50, Unknown: true},
	}, src.Hosts)
}

func TestRefreshValidatorRanges(t *testing.T) {
	h, provider := sourcesHarness(t)
	admin := provider.Grant(t, "+upgradoor:admin", "panda")
	refresh := userRoute{method: http.MethodPost, path: pathRefresh, body: `{}`}

	// The start-up fetch was just now: within the floor.
	rec := h.serve(refresh.request(admin))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := controllertest.DecodeJSON[views.ValidatorRangesRefresh](t, rec)
	assert.False(t, got.Fetched)
	assert.Equal(t, controllertest.Epoch.Add(validators.RefreshFloor), got.Next)
	assert.Equal(t, "fetched within the last 10s", got.Message)
	assert.Empty(t, h.store.AuditEntries())

	h.clock.Advance(validators.RefreshFloor)

	rec = h.serve(refresh.request(admin))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "inv-secret")

	got = controllertest.DecodeJSON[views.ValidatorRangesRefresh](t, rec)
	assert.True(t, got.Fetched)
	assert.False(t, got.Changed)
	assert.Equal(t, "fetched: unchanged", got.Message)
	assert.Equal(t, h.clock.Now(), got.Sources.ValidatorRanges.AttemptAt)
	assert.Len(t, got.Sources.ValidatorRanges.Ranges, 3)

	audit := h.store.AuditEntries()
	require.Len(t, audit, 1)
	assert.Equal(t, controllertest.UserEmail, audit[0].Actor)
	assert.Equal(t, "sources.validator-ranges.refresh", audit[0].Action)
	assert.Equal(t, map[string]any{
		"changed": false, "authorizedBy": "admin", "teams": []string{"panda"},
	}, audit[0].Detail)
}
