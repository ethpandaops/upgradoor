package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Values of the leak fixture that must never reach an unauthenticated
// visitor. Each is distinctive enough not to occur by accident.
const (
	secretKernelRunning = "6.12.41+deb13-zzfix"
	secretKernelNewest  = "6.12.48+deb13-zzfix"
	secretOSPretty      = "Debian GNU/Linux 13 (zztrixie)"
	secretOSID          = "zzdebian"
	secretOSVersion     = "13.zz7"
	secretArch          = "zzarm64"
	secretAddr          = "203.0.113.77"
	secretAddr6         = "2001:db8:77::1"
	secretCertHash      = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	secretPkgName       = "libzzfixture-crypto3"
	secretPkgVersion    = "3.5.1-zz1+deb13u9"
	secretPkgCandidate  = "3.5.4-zz1+deb13u1"
	secretBlockedPkg    = "zzdocker-ce"
	secretBlockedTo     = "5:28.3.3-zz1"
	secretAgentVersion  = "agent-zz-0.4.2"
	secretBootID        = "b007b007-zzzz-4b00-b007-b007b007b007"
	secretHoldReason    = "zz debugging a consensus bug, ask sam"
	secretVCImage       = "sigp/zz-validator:unstable"
	secretOtherImage    = "tools/zz-debug-shell:latest"
	secretContainerID   = "zz4f0e9d8c7b6a5"
	secretImageID       = "sha256:zzimageid00"
	secretProbeError    = "dial tcp 10.77.0.9:8545: connect: connection refused"
	secretRegistryError = "zz registry said 401 for token ghp_zzzz"
	secretBeaconError   = `Get "http://zz-beacon.cluster.local:5052/eth/v1/node/syncing": dial tcp 10.77.0.3:5052`
	secretLeaseError    = "zz container exited 137"
	secretAuditActor    = "zz-admin@ethpandaops.io"
)

var leakSecrets = []string{
	secretKernelRunning, secretKernelNewest, secretOSPretty, secretOSID, secretOSVersion,
	secretArch, secretAddr, secretAddr6, secretCertHash, secretPkgName, secretPkgVersion,
	secretPkgCandidate, secretBlockedPkg, secretBlockedTo, secretAgentVersion, secretBootID,
	secretHoldReason, secretVCImage, secretOtherImage, secretContainerID, secretImageID,
	secretProbeError, secretRegistryError, secretBeaconError, secretLeaseError,
	secretAuditActor, "zz", "10.77.", "cluster.local",
}

// leakFixture fills the fakes with a host that carries every kind of data
// the controller knows, plus a second host one digest behind.
func leakFixture(t *testing.T, h *harness) {
	t.Helper()

	checked := testEpoch.Add(-time.Minute)

	containers := func(clDigest string) []protocol.Container {
		return []protocol.Container{
			{
				Name: "geth", ID: secretContainerID, Image: "ethereum/client-go:stable",
				ImageID: secretImageID, Digest: "sha256:e1e1", Revision: "rev-geth-1",
				Version: "1.16.4", State: "running", Health: "healthy", RestartCount: 2,
				StartedAt: testEpoch.Add(-time.Hour), Managed: true, Kind: protocol.KindEL,
				ConfiguredImage: "ethereum/client-go:stable",
				Registry: &protocol.RegistryCheck{
					Digest: "sha256:e1e1", Revision: "rev-geth-1", CheckedAt: checked,
					Pulled: true,
				},
			},
			{
				Name: "lighthouse", ID: secretContainerID + "b", Image: "sigp/lighthouse:unstable",
				ImageID: secretImageID, Digest: clDigest, Revision: "rev-lh-" + clDigest[7:],
				State: "running", StartedAt: testEpoch.Add(-time.Hour), Managed: true,
				Kind: protocol.KindCL, ConfiguredImage: "sigp/lighthouse:unstable",
				Registry: &protocol.RegistryCheck{
					Digest: "sha256:c2c2", CheckedAt: checked, Error: secretRegistryError,
				},
			},
			{
				Name: "lighthouse-validator", ID: secretContainerID + "c", Image: secretVCImage,
				ImageID: secretImageID, Digest: "sha256:v1v1", State: "running", Managed: true,
				Kind: protocol.KindVC, ConfiguredImage: secretVCImage,
			},
			{
				Name: "debug", ID: secretContainerID + "d", Image: secretOtherImage,
				ImageID: secretImageID, State: "exited",
			},
		}
	}

	status := func(clDigest string, needs []protocol.Need) *protocol.SyncRequest {
		return &protocol.SyncRequest{
			ProtocolVersion: protocol.Version,
			AgentVersion:    secretAgentVersion,
			SentAt:          testEpoch,
			BootID:          secretBootID,
			UptimeSeconds:   86400,
			Verbs:           []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages},
			Hold:            protocol.Hold{Local: true, LocalReason: secretHoldReason},
			Containers:      containers(clDigest),
			Node: protocol.NodeHealth{
				EL: &protocol.ELHealth{Error: secretProbeError},
				CL: &protocol.CLHealth{Reachable: true, Peers: 12, HeadSlot: 99},
			},
			OS: protocol.OSSummary{
				ID: secretOSID, VersionID: secretOSVersion, PrettyName: secretOSPretty,
				Arch: secretArch, RunningKernel: secretKernelRunning,
				NewestKernel: secretKernelNewest, RebootRequired: true,
				RebootPackages: []string{secretPkgName},
			},
			Pending:       protocol.Pending{Upgradable: 9, Security: 4, Blocked: 1},
			InventoryHash: "inv-hash",
			Needs:         needs,
		}
	}

	pkgs := []protocol.PackageChange{{Name: secretBlockedPkg, From: "5:28.0.0-zz1", To: secretBlockedTo}}

	needs := []protocol.Need{
		{
			ID: protocol.PackagesNeedID(pkgs), Kind: protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		},
		{
			ID: protocol.RebootNeedID, Kind: protocol.NeedReboot,
			Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonKernel}},
		},
	}

	h.store.hosts["lighthouse-geth-1"] = &fleet.Host{
		Name: "lighthouse-geth-1", CertHash: secretCertHash, Generation: 3,
		Validators: &identity.ValidatorRange{Start: 0, End: 100},
		Roles:      []string{"ethereum_node"},
		FirstSeen:  testEpoch.Add(-48 * time.Hour), LastSeen: testEpoch,
		RemoteAddr: secretAddr, Status: status("sha256:c1c1", needs),
		InventoryHash: "inv-hash", HoldWanted: true, HoldReason: secretHoldReason,
	}
	h.store.hosts["lighthouse-geth-2"] = &fleet.Host{
		Name: "lighthouse-geth-2", CertHash: secretCertHash,
		Validators: &identity.ValidatorRange{Start: 100, End: 200},
		Roles:      []string{"ethereum_node"},
		FirstSeen:  testEpoch.Add(-48 * time.Hour), LastSeen: testEpoch,
		RemoteAddr: secretAddr6, Status: status("sha256:c2c2", nil),
	}
	// A host that has never completed a sync body (defensive: no status).
	h.store.hosts["bootnode-1"] = &fleet.Host{
		Name: "bootnode-1", CertHash: secretCertHash, Roles: []string{fleet.RoleBootnode},
		LastSeen: testEpoch, RemoteAddr: secretAddr,
	}

	h.store.inventories["lighthouse-geth-1"] = &protocol.Inventory{
		Hash: "inv-hash", CollectedAt: testEpoch,
		OS: protocol.OSInfo{
			ID: secretOSID, VersionID: secretOSVersion, PrettyName: secretOSPretty,
			Arch: secretArch,
		},
		Kernels: protocol.KernelInfo{
			Running: secretKernelRunning, Newest: secretKernelNewest,
			Installed: []string{secretKernelRunning, secretKernelNewest},
		},
		Packages: []protocol.Package{
			{Name: secretPkgName, Version: secretPkgVersion, Arch: secretArch},
		},
		Upgradable: []protocol.Upgradable{{
			Name: secretPkgName, Arch: secretArch, Current: secretPkgVersion,
			Candidate: secretPkgCandidate, Origin: "zz-Security", Security: true,
		}},
	}

	h.store.leases = []*fleet.Lease{{
		ID: "lease-zz", Host: "lighthouse-geth-1", State: fleet.LeaseFailed,
		Needs: needs, Error: secretLeaseError,
	}}
	// Per-host liveness: the public history may only show its sum.
	h.store.liveness = []fleet.Liveness{
		{Host: "lighthouse-geth-1", Epoch: 224, Live: 61, Total: 64},
		{Host: "lighthouse-geth-2", Epoch: 224, Live: 13, Total: 64},
		{Host: "lighthouse-geth-1", Epoch: 223, Live: 64, Total: 64},
	}
	h.store.audit = []fleet.AuditEntry{{
		ID: 1, Time: testEpoch, Actor: secretAuditActor, Action: "action.hold",
		Target: "lighthouse-geth-1", Detail: map[string]any{"reason": secretHoldReason},
	}}

	health := fleet.NetworkHealth{
		UpdatedAt: testEpoch, Error: secretBeaconError,
		GenesisTime:    testEpoch.Add(-24 * time.Hour),
		SecondsPerSlot: 12, SlotsPerEpoch: 32, HeadSlot: 7200, CurrentEpoch: 225,
		FinalizedEpoch: 223, FinalityLagEpochs: 2, LivenessKnown: true, LivenessEpoch: 224,
		Tracked: 200, Live: 190,
	}

	h.beacon.health = health
	h.sched.set(func(f *fakeScheduler) {
		f.plan = &scheduler.Plan{
			ComputedAt: testEpoch,
			Gate: scheduler.Gate{
				OK: false, Reasons: []string{"participation 88% < 90%"}, Network: health,
			},
			Budget: scheduler.Budget{TotalWeight: 200, AllowedWeight: 20, UnavailableWeight: 10},
			Queue: []scheduler.QueueEntry{{
				Host: "lighthouse-geth-1", Position: 1, Reason: secretHoldReason,
			}},
			Hosts: map[string]scheduler.HostPlan{
				"lighthouse-geth-1": {
					Slot: scheduler.SlotHeld, Weight: 100, Live: 90, LiveKnown: true,
					LeaseID: "lease-zz", Reason: secretHoldReason,
				},
			},
		}
	})
}

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
		"livenessEpoch", "livenessUnsupported", "tracked", "live",
	}

	tests := []struct {
		path    string
		allowed []string
	}{
		{
			path: "/api/v1/public/hosts",
			allowed: []string{
				"hosts", "name", "el", "cl", "updatePending",
				"image", "digest", "revision", "custom",
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
			path: "/api/v1/public/history",
			allowed: []string{
				"epochs", "epoch", "time", "live", "total",
				"minParticipationPercent", "epochSeconds",
			},
		},
		{path: "/api/v1/config", allowed: []string{"network", "version", "authProviderUrl"}},
	}

	for _, withPlan := range []bool{true, false} {
		for _, tt := range tests {
			name := tt.path
			if !withPlan {
				name += " before the first plan"
			}

			t.Run(name, func(t *testing.T) {
				h := newHarness(t, nil)
				leakFixture(t, h)

				if !withPlan {
					h.sched.set(func(f *fakeScheduler) { f.plan = nil })
				}

				rec := h.get(tt.path)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))

				raw := rec.Body.String()

				var parsed any
				require.NoError(t, json.Unmarshal([]byte(raw), &parsed))

				walkKeys(parsed, func(key string) {
					assert.Contains(t, tt.allowed, key, "unexpected key in %s", tt.path)
				})

				for _, secret := range leakSecrets {
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
	leakFixture(t, h)

	rec := h.get("/api/v1/hosts/lighthouse-geth-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	raw := rec.Body.String()
	for _, secret := range []string{
		secretKernelRunning, secretOSPretty, secretAddr, secretCertHash, secretPkgName,
		secretPkgVersion, secretBlockedPkg, secretAgentVersion, secretHoldReason,
		secretVCImage, secretOtherImage, secretLeaseError, secretAuditActor,
	} {
		assert.Contains(t, raw, secret)
	}
}

func TestPublicHosts(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	rec := h.get("/api/v1/public/hosts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	geth := &PublicImageRef{
		Image: "ethereum/client-go:stable", Digest: "sha256:e1e1", Revision: "rev-geth-1",
	}

	assert.Equal(t, PublicHosts{Hosts: []PublicHost{
		{Name: "bootnode-1"},
		{
			Name: "lighthouse-geth-1", EL: geth, UpdatePending: true,
			CL: &PublicImageRef{
				Image: "sigp/lighthouse:unstable", Digest: "sha256:c1c1", Revision: "rev-lh-c1c1",
			},
		},
		{
			Name: "lighthouse-geth-2", EL: geth,
			CL: &PublicImageRef{
				Image: "sigp/lighthouse:unstable", Digest: "sha256:c2c2", Revision: "rev-lh-c2c2",
			},
		},
	}}, decodeJSON[PublicHosts](t, rec))
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
	leakFixture(t, h)

	// A third host runs a hand-swapped CL image no registry check covers.
	h.store.hosts["teku-geth-1"] = &fleet.Host{
		Name: "teku-geth-1", LastSeen: testEpoch,
		Status: &protocol.SyncRequest{Containers: []protocol.Container{{
			Name: "teku", Image: "someone/teku:debug", Digest: "sha256:d0d0",
			Revision: "rev-debug", Managed: true, Kind: protocol.KindCL,
			ConfiguredImage: "consensys/teku:develop", Custom: true,
		}}},
	}

	rec := h.get("/api/v1/public/images")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, PublicImages{Images: []PublicImage{
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
	}}, decodeJSON[PublicImages](t, rec))
}

func TestTargetDigest(t *testing.T) {
	at := func(minute int) time.Time {
		return testEpoch.Add(time.Duration(minute) * time.Minute)
	}
	reg := func(digest, rev string, checked time.Time, errText string) *protocol.RegistryCheck {
		return &protocol.RegistryCheck{
			Digest: digest, Revision: rev, CheckedAt: checked, Error: errText,
		}
	}

	tests := []struct {
		name         string
		uses         []imageUse
		wantDigest   string
		wantRevision string
	}{
		{
			name: "newest successful registry check wins",
			uses: []imageUse{
				{host: "a", digest: "sha256:old", registry: reg("sha256:old", "r-old", at(1), "")},
				{host: "b", digest: "sha256:old", registry: reg("sha256:new", "r-new", at(5), "")},
				{host: "c", digest: "sha256:old", registry: reg("sha256:err", "", at(9), "boom")},
			},
			wantDigest: "sha256:new", wantRevision: "r-new",
		},
		{
			name: "a failed check still knows the last good digest",
			uses: []imageUse{
				{host: "a", digest: "sha256:old", registry: reg("sha256:new", "r-new", at(5), "boom")},
				{host: "b", digest: "sha256:old"},
				{host: "c", digest: "sha256:old"},
			},
			wantDigest: "sha256:new", wantRevision: "r-new",
		},
		{
			name: "revision from a host that already runs the target",
			uses: []imageUse{
				{host: "a", digest: "sha256:new", revision: "r-running",
					registry: reg("sha256:new", "", at(1), "")},
				{host: "b", digest: "sha256:old", revision: "r-old"},
			},
			wantDigest: "sha256:new", wantRevision: "r-running",
		},
		{
			name: "without registry data the most common digest",
			uses: []imageUse{
				{host: "a", digest: "sha256:x", revision: "r-x"},
				{host: "b", digest: "sha256:y", revision: "r-y"},
				{host: "c", digest: "sha256:y", revision: "r-y"},
			},
			wantDigest: "sha256:y", wantRevision: "r-y",
		},
		{
			name:       "nothing known",
			uses:       []imageUse{{host: "a"}},
			wantDigest: "", wantRevision: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			digest, revision := targetDigest(tt.uses)
			assert.Equal(t, tt.wantDigest, digest)
			assert.Equal(t, tt.wantRevision, revision)
		})
	}
}

func TestPublicNetwork(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	rec := h.get("/api/v1/public/network")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	gate := decodeJSON[scheduler.Gate](t, rec)
	assert.False(t, gate.OK)
	assert.Equal(t, []string{"participation 88% < 90%"}, gate.Reasons)
	assert.Equal(t, uint64(2), gate.Network.FinalityLagEpochs)
	assert.Equal(t, uint64(190), gate.Network.Live)
	assert.Equal(t, beaconErrorPublic, gate.Network.Error, "the real error text stays inside")

	// The authenticated plan carries the real error.
	plan := decodeJSON[scheduler.Plan](t, h.get("/api/v1/plan"))
	assert.Equal(t, secretBeaconError, plan.Gate.Network.Error)

	// Before the first plan: a closed gate around the beacon view.
	h.sched.set(func(f *fakeScheduler) { f.plan = nil })

	gate = decodeJSON[scheduler.Gate](t, h.get("/api/v1/public/network"))
	assert.False(t, gate.OK)
	assert.Equal(t, []string{"the scheduler has not planned yet"}, gate.Reasons)
	assert.Equal(t, uint64(225), gate.Network.CurrentEpoch)
	assert.Equal(t, beaconErrorPublic, gate.Network.Error)
}

func TestPublicHistory(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	h.store.liveness = []fleet.Liveness{
		{Host: "b", Epoch: 224, Live: 60, Total: 64},
		{Host: "a", Epoch: 224, Live: 100, Total: 128},
		{Host: "a", Epoch: 223, Live: 128, Total: 128},
	}

	rec := h.get("/api/v1/public/history")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := decodeJSON[PublicHistory](t, rec)
	genesis := testEpoch.Add(-24 * time.Hour)

	assert.Equal(t, []ParticipationPoint{
		{Epoch: 223, Time: genesis.Add(224 * 384 * time.Second), Live: 128, Total: 128},
		{Epoch: 224, Time: genesis.Add(225 * 384 * time.Second), Live: 160, Total: 192},
	}, got.Epochs, "summed per epoch, oldest first, stamped with the end of the epoch")
	assert.InDelta(t, fleet.DefaultSettings().MinParticipationPercent, got.MinParticipationPercent, 0)
	assert.InDelta(t, 384, got.EpochSeconds, 0)
	assert.NotContains(t, rec.Body.String(), `"host"`, "which host attested is not public")

	// A point is the sum and nothing else.
	var raw struct {
		Epochs []map[string]json.RawMessage `json:"epochs"`
	}

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Epochs, 2)

	for _, point := range raw.Epochs {
		keys := make([]string, 0, len(point))
		for key := range point {
			keys = append(keys, key)
		}

		assert.ElementsMatch(t, []string{"epoch", "time", "live", "total"}, keys)
	}

	assert.Equal(t, http.StatusBadRequest, h.get("/api/v1/public/history?limit=0").Code)

	// No liveness yet: an empty list, not null.
	h.store.liveness = nil
	assert.JSONEq(t, `[]`, string(decodeJSON[map[string]json.RawMessage](t,
		h.get("/api/v1/public/history"))["epochs"]))
}

// A scheduler may quote the beacon error in a gate reason. The public view
// must not carry it there either.
func TestPublicNetworkScrubsReasons(t *testing.T) {
	const inner = "dial tcp 10.77.0.3:5052"

	require.Contains(t, secretBeaconError, ": "+inner, "the fixture error wraps the inner one")

	tests := []struct {
		name    string
		err     string
		reasons []string
		want    []string
	}{
		{
			name:    "reason quotes the error",
			err:     secretBeaconError,
			reasons: []string{"beacon unavailable: " + secretBeaconError, "participation 88% < 90%"},
			want:    []string{beaconErrorPublic, "participation 88% < 90%"},
		},
		{
			name:    "reason is the error",
			err:     secretBeaconError,
			reasons: []string{secretBeaconError},
			want:    []string{beaconErrorPublic},
		},
		{
			name:    "reason quotes only the wrapped error",
			err:     secretBeaconError,
			reasons: []string{"no beacon data (" + inner + ")"},
			want:    []string{beaconErrorPublic},
		},
		{
			name: "several quoting reasons collapse into one",
			err:  secretBeaconError,
			reasons: []string{
				"beacon unavailable: " + secretBeaconError,
				"finality unknown: " + secretBeaconError,
				"finality lag 5 epochs > 3",
			},
			want: []string{beaconErrorPublic, "finality lag 5 epochs > 3"},
		},
		{
			name:    "no beacon error: reasons pass",
			reasons: []string{"participation 88% < 90%", "finality lag 5 epochs > 3"},
			want:    []string{"participation 88% < 90%", "finality lag 5 epochs > 3"},
		},
		{
			name:    "no reasons",
			err:     secretBeaconError,
			reasons: []string{},
			want:    []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.sched.set(func(f *fakeScheduler) {
				f.plan = &scheduler.Plan{Gate: scheduler.Gate{
					Reasons: tt.reasons,
					Network: fleet.NetworkHealth{Error: tt.err, CurrentEpoch: 225},
				}}
			})

			rec := h.get("/api/v1/public/network")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			for _, secret := range []string{
				secretBeaconError, inner, "zz-beacon", "10.77.0.3", "5052",
			} {
				assert.NotContains(t, rec.Body.String(), secret)
			}

			gate := decodeJSON[scheduler.Gate](t, rec)
			assert.Equal(t, tt.want, gate.Reasons)

			// The plan the scheduler handed out is not modified, and users
			// still see the real text.
			plan := decodeJSON[scheduler.Plan](t, h.get("/api/v1/plan"))
			assert.Equal(t, tt.reasons, plan.Gate.Reasons)
		})
	}
}

func TestConfig(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get("/api/v1/config")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, Config{Network: testNetwork, Version: "v1.2.3-test"},
		decodeJSON[Config](t, rec))

	h = newHarness(t, func(o *Options) { o.AuthProviderURL = "https://auth.testnet.example/" })

	// Public routes work without Start and without a token.
	assert.Equal(t, Config{
		Network: testNetwork, Version: "v1.2.3-test",
		AuthProviderURL: "https://auth.testnet.example",
	}, decodeJSON[Config](t, h.get("/api/v1/config")))
}

func TestPublicStoreError(t *testing.T) {
	h := newHarness(t, nil)
	h.store.setErr(assert.AnError)

	for _, path := range []string{"/api/v1/public/hosts", "/api/v1/public/images"} {
		rec := h.get(path)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, ErrorBody{Error: "internal error"}, decodeJSON[ErrorBody](t, rec))
	}
}
