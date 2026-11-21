package userapi

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// walkKeys calls visit for every object key in v.
func walkKeys(v any, visit func(key string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			visit(k)
			walkKeys(child, visit)
		}
	case []any:
		for _, child := range x {
			walkKeys(child, visit)
		}
	}
}

func TestPublicEndpointsLeakNothing(t *testing.T) {
	networkKeys := []string{
		"ok", "reasons", "network",
		"updatedAt", "error", "genesisTime", "secondsPerSlot", "slotsPerEpoch", "headSlot",
		"currentEpoch", "finalizedEpoch", "finalityLagEpochs", "livenessKnown",
		"livenessEpoch", "livenessError", "tracked", "live",
		// The chain's slot phases: public chain data.
		"phases", "epoch", "start", "slotMs",
	}

	tests := []struct {
		path    string
		allowed []string
	}{
		{
			path: "/api/v1/public/hosts",
			allowed: []string{
				"hosts", "name", "el", "cl", "updatePending",
				"image", "digest", "revision",
			},
		},
		{
			path: "/api/v1/public/images",
			allowed: []string{
				"images", "image", "kind", "digest", "revision", "hostsOn", "hostsBehind",
			},
		},
		{path: "/api/v1/public/network", allowed: networkKeys},
		{
			// No "hosts" and no "name": which host attested is not public.
			path: "/api/v1/public/slots?window=600",
			allowed: []string{
				"firstSlot", "firstTime", "slotMs", "slotsPerEpoch", "bucketSlots",
				"minParticipationPercent", "epochSeconds",
				"recorded", "missed", "assigned", "included", "worstAssigned", "worstIncluded",
			},
		},
		{path: "/api/v1/config", allowed: []string{"network", "version", "authProviderUrl", "authService"}},
	}

	for _, withPlan := range []bool{true, false} {
		for _, tt := range tests {
			name := tt.path
			if !withPlan {
				name += " before the first plan"
			}

			t.Run(name, func(t *testing.T) {
				h := newHarness(t, nil)
				controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

				if !withPlan {
					h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentPlan = nil })
				}

				rec := h.get(tt.path)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Equal(t, httpjson.ContentTypeJSON, rec.Header().Get("Content-Type"))

				raw := rec.Body.String()

				var parsed any
				require.NoError(t, json.Unmarshal([]byte(raw), &parsed))

				walkKeys(parsed, func(key string) {
					assert.Contains(t, tt.allowed, key, "unexpected key in %s", tt.path)
				})

				for _, secret := range controllertest.LeakSecrets {
					assert.NotContains(t, raw, secret, "%s leaks fixture data", tt.path)
				}
			})
		}
	}
}

// TestLeakFixtureIsVisibleToUsers makes sure the leak test would notice: the
// same fixture, through the authenticated views, does carry the secrets.
func TestLeakFixtureIsVisibleToUsers(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	rec := h.get("/api/v1/hosts/lighthouse-geth-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	raw := rec.Body.String()
	for _, secret := range []string{
		controllertest.SecretKernelRunning, controllertest.SecretOSPretty, controllertest.SecretAddr, controllertest.SecretCertHash, controllertest.SecretPkgName,
		controllertest.SecretPkgVersion, controllertest.SecretBlockedPkg, controllertest.SecretAgentVersion, controllertest.SecretHoldReason,
		controllertest.SecretVCImage, controllertest.SecretOtherImage, controllertest.SecretLeaseError, controllertest.SecretAuditActor,
		controllertest.SecretOwnerEL, controllertest.SecretOwnerCL,
	} {
		assert.Contains(t, raw, secret)
	}

	// The pause reason of a rollout and the comparison of a lease with the
	// fleet are for users too, and for nobody else (the keys are in no public
	// view: TestPublicEndpointsLeakNothing).
	rec = h.get("/api/v1/rollouts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	raw = rec.Body.String()
	for _, want := range []string{
		`"pauseReason":"` + controllertest.SecretPauseReason + `"`, `"peerChecks":[{"layer":"` + controllertest.SecretPeerLayer + `"`, controllertest.SecretReadiness,
	} {
		assert.Contains(t, raw, want)
	}
}

func TestPublicHosts(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	rec := h.get("/api/v1/public/hosts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	geth := &views.PublicImageRef{
		Image: "ethereum/client-go:stable", Digest: "sha256:e1e1", Revision: "rev-geth-1",
	}

	assert.Equal(t, views.PublicHosts{Hosts: []views.PublicHost{
		{Name: "bootnode-1"},
		{
			Name: "lighthouse-geth-1", EL: geth, UpdatePending: true,
			CL: &views.PublicImageRef{
				Image: "sigp/lighthouse:unstable", Digest: "sha256:c1c1", Revision: "rev-lh-c1c1",
			},
		},
		{
			Name: "lighthouse-geth-2", EL: geth,
			CL: &views.PublicImageRef{
				Image: "sigp/lighthouse:unstable", Digest: "sha256:c2c2", Revision: "rev-lh-c2c2",
			},
		},
	}}, controllertest.DecodeJSON[views.PublicHosts](t, rec))
}

func TestPublicHostsEmpty(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get("/api/v1/public/hosts")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"hosts":[]}`, rec.Body.String())

	rec = h.get("/api/v1/public/images")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"images":[]}`, rec.Body.String())
}

func TestPublicImages(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	// A third host runs a hand-swapped CL image no registry check covers.
	h.store.Hosts["teku-geth-1"] = &fleet.Host{
		Name: "teku-geth-1", LastSeen: controllertest.Epoch,
		Status: &protocol.SyncRequest{Containers: []protocol.Container{{
			Name: "teku", Image: "someone/teku:debug", Digest: "sha256:d0d0",
			Revision: "rev-debug", Managed: true, Kind: protocol.KindCL,
			ConfiguredImage: "consensys/teku:develop",
		}}},
	}

	rec := h.get("/api/v1/public/images")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, views.PublicImages{Images: []views.PublicImage{
		{
			Image: "ethereum/client-go:stable", Kind: protocol.KindEL, Digest: "sha256:e1e1",
			Revision:    "rev-geth-1",
			HostsOn:     []string{"lighthouse-geth-1", "lighthouse-geth-2"},
			HostsBehind: []string{},
		},
		{
			// Both registry checks failed; their last good digest is still
			// the best knowledge of where the tag points.
			Image: "sigp/lighthouse:unstable", Kind: protocol.KindCL, Digest: "sha256:c2c2",
			Revision:    "rev-lh-c2c2",
			HostsOn:     []string{"lighthouse-geth-2"},
			HostsBehind: []string{"lighthouse-geth-1"},
		},
		{
			Image: "someone/teku:debug", Kind: protocol.KindCL, Digest: "sha256:d0d0",
			Revision: "rev-debug", HostsOn: []string{"teku-geth-1"}, HostsBehind: []string{},
		},
	}}, controllertest.DecodeJSON[views.PublicImages](t, rec))
}

func TestPublicNetwork(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	rec := h.get("/api/v1/public/network")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	gate := controllertest.DecodeJSON[scheduler.Gate](t, rec)
	assert.False(t, gate.OK)
	assert.Equal(t, []string{"participation 88% < 90%"}, gate.Reasons)
	assert.Equal(t, uint64(2), gate.Network.FinalityLagEpochs)
	assert.Equal(t, uint64(190), gate.Network.Live)
	assert.Equal(t, "beacon node unavailable", gate.Network.Error, "the real error text stays inside")

	// The authenticated plan carries the real error.
	plan := controllertest.DecodeJSON[scheduler.Plan](t, h.get("/api/v1/plan"))
	assert.Equal(t, controllertest.SecretBeaconError, plan.Gate.Network.Error)

	// Before the first plan: a closed gate around the beacon view.
	h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentPlan = nil })

	gate = controllertest.DecodeJSON[scheduler.Gate](t, h.get("/api/v1/public/network"))
	assert.False(t, gate.OK)
	assert.Equal(t, []string{"the scheduler has not planned yet"}, gate.Reasons)
	assert.Equal(t, uint64(225), gate.Network.CurrentEpoch)
	assert.Equal(t, "beacon node unavailable", gate.Network.Error)
}

// A scheduler may quote the beacon error in a gate reason. The public view
// must not carry it there either.
func TestPublicNetworkScrubsReasons(t *testing.T) {
	const inner = "dial tcp 10.77.0.3:5052"

	require.Contains(t, controllertest.SecretBeaconError, ": "+inner, "the fixture error wraps the inner one")

	tests := []struct {
		name    string
		err     string
		reasons []string
		want    []string
	}{
		{
			name:    "reason quotes the error",
			err:     controllertest.SecretBeaconError,
			reasons: []string{"beacon unavailable: " + controllertest.SecretBeaconError, "participation 88% < 90%"},
			want:    []string{"beacon node unavailable", "participation 88% < 90%"},
		},
		{
			name:    "reason is the error",
			err:     controllertest.SecretBeaconError,
			reasons: []string{controllertest.SecretBeaconError},
			want:    []string{"beacon node unavailable"},
		},
		{
			name:    "reason quotes only the wrapped error",
			err:     controllertest.SecretBeaconError,
			reasons: []string{"no beacon data (" + inner + ")"},
			want:    []string{"beacon node unavailable"},
		},
		{
			name: "several quoting reasons collapse into one",
			err:  controllertest.SecretBeaconError,
			reasons: []string{
				"beacon unavailable: " + controllertest.SecretBeaconError,
				"finality unknown: " + controllertest.SecretBeaconError,
				"finality lag 5 epochs > 3",
			},
			want: []string{"beacon node unavailable", "finality lag 5 epochs > 3"},
		},
		{
			name:    "no beacon error: reasons pass",
			reasons: []string{"participation 88% < 90%", "finality lag 5 epochs > 3"},
			want:    []string{"participation 88% < 90%", "finality lag 5 epochs > 3"},
		},
		{
			name:    "no reasons",
			err:     controllertest.SecretBeaconError,
			reasons: []string{},
			want:    []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.sched.Set(func(f *controllertest.Scheduler) {
				f.CurrentPlan = &scheduler.Plan{Gate: scheduler.Gate{
					Reasons: tt.reasons,
					Network: fleet.NetworkHealth{Error: tt.err, CurrentEpoch: 225},
				}}
			})

			rec := h.get("/api/v1/public/network")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			for _, secret := range []string{
				controllertest.SecretBeaconError, inner, "zz-beacon", "10.77.0.3", "5052",
			} {
				assert.NotContains(t, rec.Body.String(), secret)
			}

			gate := controllertest.DecodeJSON[scheduler.Gate](t, rec)
			assert.Equal(t, tt.want, gate.Reasons)

			// The plan the scheduler handed out is not modified, and users
			// still see the real text.
			plan := controllertest.DecodeJSON[scheduler.Plan](t, h.get("/api/v1/plan"))
			assert.Equal(t, tt.reasons, plan.Gate.Reasons)
		})
	}
}

func TestConfig(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get("/api/v1/config")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, views.Config{Network: controllertest.Network, Version: "v1.2.3-test", AuthService: "upgradoor"},
		controllertest.DecodeJSON[views.Config](t, rec))

	h = newHarness(t, func(o *Options) { o.AuthProviderURL = "https://auth.testnet.example/" })

	// Public routes work without Start and without a token.
	assert.Equal(t, views.Config{
		Network: controllertest.Network, Version: "v1.2.3-test",
		AuthProviderURL: "https://auth.testnet.example", AuthService: "upgradoor",
	}, controllertest.DecodeJSON[views.Config](t, h.get("/api/v1/config")))
}

func TestPublicStoreError(t *testing.T) {
	h := newHarness(t, nil)
	h.store.SetErr(assert.AnError)

	for _, path := range []string{"/api/v1/public/hosts", "/api/v1/public/images"} {
		rec := h.get(path)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, httpjson.ErrorBody{Error: "internal error"}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
	}
}

// TestWebFeaturesAreTheServers: the features the page asks the auth client
// for are the features this package checks, name for name, and the page
// gives a team twin to the same ones. A feature added, renamed or removed on
// one side only fails here.
func TestWebFeaturesAreTheServers(t *testing.T) {
	raw, err := os.ReadFile(webPermissions)
	require.NoError(t, err)

	src := string(raw)

	// export const FEATURE = { roll: 'roll', ... } as const;
	entry := regexp.MustCompile(`(?m)^\s*(\w+):\s*'([^']+)',\s*$`)
	web := make([]string, 0, 8)

	for _, m := range entry.FindAllStringSubmatch(tsBlock(t, src, "export const FEATURE = {", "} as const;"), -1) {
		assert.Equal(t, m[1], m[2], "the page names a feature by its own name")
		web = append(web, m[2])
	}

	server := []string{
		authz.FeatureRoll, authz.FeatureReboot, authz.FeatureHold, authz.FeatureRecover, authz.FeatureSupervise, authz.FeatureAdmin,
		authz.FeatureClientPolicy, authz.FeatureImageOverride, authz.FeatureFlagOverride,
	}

	slices.Sort(web)
	slices.Sort(server)
	assert.Equal(t, server, web, "features of %s and of authz.go", webPermissions)

	// export const TWINNED: readonly Feature[] = [FEATURE.roll, ...];
	twinned := make([]string, 0, 4)
	for _, m := range regexp.MustCompile(`FEATURE\.(\w+)`).FindAllStringSubmatch(
		tsBlock(t, src, "export const TWINNED: readonly Feature[] = [", "];"), -1) {
		twinned = append(twinned, m[1])
	}

	slices.Sort(twinned)
	assert.Equal(t, []string{
		authz.FeatureClientPolicy, authz.FeatureFlagOverride, authz.FeatureHold, authz.FeatureImageOverride,
		authz.FeatureReboot, authz.FeatureRecover, authz.FeatureRoll,
	}, twinned,
		"the features with a team twin")

	assert.Contains(t, src, "export const TEAM_PREFIX = '"+authz.TeamFeaturePrefix+"';")

	// The twins the server gives are the ones whose requirement names
	// targets: one request per action with a twin.
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	withTwin := make([]string, 0, 4)

	for _, s := range actionSamples(t) {
		if s.twin && !slices.Contains(withTwin, s.feature) {
			withTwin = append(withTwin, s.feature)
		}
	}

	slices.Sort(withTwin)
	assert.Equal(t, twinned, withTwin)
}
