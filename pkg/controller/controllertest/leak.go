package controllertest

import (
	"testing"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Values of the leak fixture that must never reach an unauthenticated
// visitor. Each is distinctive enough not to occur by accident.
const (
	SecretKernelRunning = "6.12.41+deb13-zzfix"
	SecretKernelNewest  = "6.12.48+deb13-zzfix"
	SecretOSPretty      = "Debian GNU/Linux 13 (zztrixie)"
	SecretOSID          = "zzdebian"
	SecretOSVersion     = "13.zz7"
	SecretArch          = "zzarm64"
	SecretAddr          = "203.0.113.77"
	SecretAddr6         = "2001:db8:77::1"
	SecretCertHash      = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	SecretPkgName       = "libzzfixture-crypto3"
	SecretPkgVersion    = "3.5.1-zz1+deb13u9"
	SecretPkgCandidate  = "3.5.4-zz1+deb13u1"
	SecretBlockedPkg    = "zzdocker-ce"
	SecretBlockedTo     = "5:28.3.3-zz1"
	SecretAgentVersion  = "agent-zz-0.4.2"
	SecretBootID        = "b007b007-zzzz-4b00-b007-b007b007b007"
	SecretHoldReason    = "zz debugging a consensus bug, ask sam"
	SecretVCImage       = "sigp/zz-validator:unstable"
	SecretOtherImage    = "tools/zz-debug-shell:latest"
	SecretContainerID   = "zz4f0e9d8c7b6a5"
	SecretImageID       = "sha256:zzimageid00"
	SecretProbeError    = "dial tcp 10.77.0.9:8545: connect: connection refused"
	SecretRegistryError = "zz registry said 401 for token ghp_zzzz"
	SecretBeaconError   = `Get "http://zz-beacon.cluster.local:5052/eth/v1/node/syncing": dial tcp 10.77.0.3:5052`
	SecretLeaseError    = "zz container exited 137"
	SecretReadiness     = "zz EL peers 4, the fleet's median is 31 (needs 16)"
	SecretPeerLayer     = "zzEL"
	SecretPauseReason   = "zz paused after the canary; resume to continue"
	SecretAuditActor    = "zz-admin@ethpandaops.io"
	SecretRangesError   = `Get "https://zz-inventory.cluster.local/ranges.json": EOF`
	SecretSettingsKey   = "zz-settings-only-action"
	SecretFlag          = "--zz-secret-flag=7"
	SecretActionName    = "zz-flush-caches"
	SecretActionOutput  = "zz flushed 3 caches on /dev/zzsda1"
	// Teams are team names; these two carry no "zz", so the catch-all below
	// is not what finds them.
	SecretOwnerEL = "qq-stealth-el-team"
	SecretOwnerCL = "qq-stealth-cl-team"
)

// LeakSecrets are the values of the leak fixture and the marks they share.
var LeakSecrets = []string{
	SecretKernelRunning, SecretKernelNewest, SecretOSPretty, SecretOSID, SecretOSVersion,
	SecretArch, SecretAddr, SecretAddr6, SecretCertHash, SecretPkgName, SecretPkgVersion,
	SecretPkgCandidate, SecretBlockedPkg, SecretBlockedTo, SecretAgentVersion, SecretBootID,
	SecretHoldReason, SecretVCImage, SecretOtherImage, SecretContainerID, SecretImageID,
	SecretProbeError, SecretRegistryError, SecretBeaconError, SecretLeaseError,
	SecretReadiness, SecretPeerLayer, SecretPauseReason,
	SecretAuditActor, SecretFlag, SecretActionName, SecretActionOutput, SecretOwnerEL, SecretOwnerCL,
	SecretRangesError, SecretSettingsKey, "zz", "10.77.", "cluster.local",
}

// AdminSecrets are the values of the leak fixture that only the admin pages
// show: the error texts of the beacon service and of the validator ranges
// inventory (the data sources), the settings a settings change did not
// change (the settings). No event stream carries them.
var AdminSecrets = []string{
	SecretBeaconError, SecretRangesError, SecretSettingsKey, "zz-beacon", "zz-inventory", "10.77.0.3",
}

// HostDetailSecrets are the values of the leak fixture that a login reads
// only with the full view of the host (views.Access): the system of the
// host, its unmanaged containers, the internals of managed containers, its
// identity, its resource use and its host actions. Of them, the command line of the
// container geth belongs to the team SecretOwnerEL.
var HostDetailSecrets = []string{
	SecretKernelRunning, SecretKernelNewest, SecretOSPretty, SecretOSID, SecretOSVersion,
	SecretArch, SecretAddr, SecretAddr6, SecretCertHash, SecretPkgName, SecretPkgVersion,
	SecretPkgCandidate, SecretBlockedPkg, SecretBlockedTo, SecretAgentVersion, SecretBootID,
	SecretOtherImage, SecretContainerID, SecretImageID, SecretProbeError, SecretFlag,
	SecretActionName, SecretActionOutput,
}

// LeakFixture fills the fakes with a host that carries every kind of data
// the controller knows, plus a second host one digest behind.
func LeakFixture(t *testing.T, st *Store, sched *Scheduler, beacon *Beacon) {
	t.Helper()

	checked := Epoch.Add(-time.Minute)
	busy := 42.5

	containers := func(clDigest string) []protocol.Container {
		return []protocol.Container{
			{
				Name: "geth", ID: SecretContainerID, Image: "ethereum/client-go:stable",
				ImageID: SecretImageID, Digest: "sha256:e1e1", Revision: "rev-geth-1",
				Version: "1.16.4", State: "running", Health: "healthy", RestartCount: 2,
				StartedAt: Epoch.Add(-time.Hour), Managed: true, Kind: protocol.KindEL,
				ConfiguredImage: "ethereum/client-go:stable", Team: SecretOwnerEL,
				Command: []string{"--http", SecretFlag},
				Stats:   &protocol.ContainerStats{Memory: 3 << 30, MemoryLimit: 8 << 30},
				Registry: &protocol.RegistryCheck{
					Digest: "sha256:e1e1", Revision: "rev-geth-1", CheckedAt: checked,
					Pulled: true,
				},
			},
			{
				Name: "lighthouse", ID: SecretContainerID + "b", Image: "sigp/lighthouse:unstable",
				ImageID: SecretImageID, Digest: clDigest, Revision: "rev-lh-" + clDigest[7:],
				State: "running", StartedAt: Epoch.Add(-time.Hour), Managed: true,
				Kind: protocol.KindCL, ConfiguredImage: "sigp/lighthouse:unstable",
				Team: SecretOwnerCL,
				Registry: &protocol.RegistryCheck{
					Digest: "sha256:c2c2", CheckedAt: checked, Error: SecretRegistryError,
				},
			},
			{
				Name: "lighthouse-validator", ID: SecretContainerID + "c", Image: SecretVCImage,
				ImageID: SecretImageID, Digest: "sha256:v1v1", State: "running", Managed: true,
				Kind: protocol.KindVC, ConfiguredImage: SecretVCImage,
			},
			{
				Name: "debug", ID: SecretContainerID + "d", Image: SecretOtherImage,
				ImageID: SecretImageID, State: "exited",
			},
		}
	}

	status := func(clDigest string, needs []protocol.Need) *protocol.SyncRequest {
		return &protocol.SyncRequest{
			ProtocolVersion: protocol.Version,
			AgentVersion:    SecretAgentVersion,
			SentAt:          Epoch,
			BootID:          SecretBootID,
			UptimeSeconds:   86400,
			Verbs:           []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages},
			Actions:         []protocol.ActionInfo{{Name: SecretActionName, Impact: protocol.ImpactNode}},
			Hold: protocol.Hold{Local: true, LocalReason: SecretHoldReason, Holds: []protocol.HostHold{
				{Holder: protocol.HolderHost, By: "pk", Reason: SecretHoldReason, At: Epoch},
				{Holder: protocol.HolderStaff, By: "alice@example.com", Reason: SecretHoldReason, At: Epoch},
			}},
			Containers: containers(clDigest),
			Node: protocol.NodeHealth{
				EL: &protocol.ELHealth{Error: SecretProbeError},
				CL: &protocol.CLHealth{Reachable: true, Peers: 12, HeadSlot: 99},
			},
			OS: protocol.OSSummary{
				ID: SecretOSID, VersionID: SecretOSVersion, PrettyName: SecretOSPretty,
				Arch: SecretArch, RunningKernel: SecretKernelRunning,
				NewestKernel: SecretKernelNewest, RebootRequired: true,
				RebootPackages: []string{SecretPkgName},
			},
			Pending: protocol.Pending{Upgradable: 9, Security: 4, Blocked: 1},
			Resources: protocol.Resources{
				CPU:    protocol.CPUStats{Cores: 16, Load1: 2.5, Load5: 2, Load15: 1.5, Busy: &busy},
				Memory: protocol.MemoryStats{Total: 64 << 30, Available: 20 << 30, SwapTotal: 8 << 30},
			},
			InventoryHash: "inv-hash",
			Needs:         needs,
		}
	}

	pkgs := []protocol.PackageChange{{Name: SecretBlockedPkg, From: "5:28.0.0-zz1", To: SecretBlockedTo}}

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

	st.Hosts["lighthouse-geth-1"] = &fleet.Host{
		Name: "lighthouse-geth-1", CertHash: SecretCertHash,
		Validators: []identity.ValidatorRange{{Start: 0, End: 100}},
		Roles:      []string{"ethereum_node"},
		FirstSeen:  Epoch.Add(-48 * time.Hour), LastSeen: Epoch,
		RemoteAddr: SecretAddr, Status: status("sha256:c1c1", needs),
		InventoryHash: "inv-hash",
	}
	st.Hosts["lighthouse-geth-2"] = &fleet.Host{
		Name: "lighthouse-geth-2", CertHash: SecretCertHash,
		Validators: []identity.ValidatorRange{{Start: 100, End: 200}},
		Roles:      []string{"ethereum_node"},
		FirstSeen:  Epoch.Add(-48 * time.Hour), LastSeen: Epoch,
		RemoteAddr: SecretAddr6, Status: status("sha256:c2c2", nil),
	}
	// A host that has never completed a sync body (defensive: no status).
	st.Hosts["bootnode-1"] = &fleet.Host{
		Name: "bootnode-1", CertHash: SecretCertHash, Roles: []string{"bootnode"},
		LastSeen: Epoch, RemoteAddr: SecretAddr,
	}

	st.Inventories["lighthouse-geth-1"] = &protocol.Inventory{
		Hash: "inv-hash", CollectedAt: Epoch,
		OS: protocol.OSInfo{
			ID: SecretOSID, VersionID: SecretOSVersion, PrettyName: SecretOSPretty,
			Arch: SecretArch,
		},
		Kernels: protocol.KernelInfo{
			Running: SecretKernelRunning, Newest: SecretKernelNewest,
			Installed: []string{SecretKernelRunning, SecretKernelNewest},
		},
		Packages: []protocol.Package{
			{Name: SecretPkgName, Version: SecretPkgVersion, Arch: SecretArch},
		},
		Upgradable: []protocol.Upgradable{{
			Name: SecretPkgName, Arch: SecretArch, Current: SecretPkgVersion,
			Candidate: SecretPkgCandidate, Origin: "zz-Security", Security: true,
		}},
	}

	st.Leases = []*fleet.Lease{{
		ID: "lease-zz", Host: "lighthouse-geth-1", State: fleet.LeaseFailed,
		Needs: needs, Error: SecretLeaseError,
		// What the last readiness check compared with the fleet.
		Readiness:  SecretReadiness,
		PeerChecks: []fleet.PeerCheck{{Layer: SecretPeerLayer, Peers: 4, Median: 31, Needs: 16}},
		// The host's state when the lease was granted.
		Baseline: &fleet.LeaseBaseline{
			At: Epoch, Unready: "EL is not reachable", Root: protocol.KindEL, Duty: 2, Live: 2, LiveKnown: true,
			Node: protocol.NodeHealth{EL: &protocol.ELHealth{Error: SecretProbeError}},
			Containers: []fleet.BaselineContainer{
				{Name: "geth", Kind: protocol.KindEL, State: "restarting", RestartCount: 7},
			},
		},
		Outcome: fleet.OutcomeWorse,
		Operation: &protocol.Operation{
			LeaseID: "lease-zz", Phase: protocol.PhaseFailed, StartedAt: Epoch,
			Steps: []protocol.Step{{
				NeedID: "action/1", Status: protocol.StepFailed,
				Action: &protocol.ActionResult{ExitCode: 1, Output: SecretActionOutput},
			}},
		},
	}}
	// A rollout that the scheduler paused, with its reason.
	st.Rollouts = []*fleet.Rollout{{
		ID: "rollout-zz", Kind: fleet.RolloutReboot, Key: "reboot", Title: "reboot",
		State: fleet.RolloutActive, Paused: true, PauseReason: SecretPauseReason,
		Hosts: []fleet.RolloutHost{{Host: "lighthouse-geth-1", State: fleet.HostPending}},
	}}
	// Per-host slot participation: public is the sum only, too.
	st.Slots = []fleet.SlotParticipation{{
		Slot: 7190, Proposed: true, Assigned: 4, Included: 3,
		Hosts: []fleet.HostSlotParticipation{
			{Host: "lighthouse-geth-1", Assigned: 2, Included: 2},
			{Host: "lighthouse-geth-2", Assigned: 2, Included: 1},
		},
	}}
	before := fleet.DefaultSettings()
	before.ActionPolicies = map[string]fleet.Policy{SecretSettingsKey: fleet.PolicyAuto}
	after := before
	after.MaxUnavailablePercent = 20

	st.Audit = []fleet.AuditEntry{{
		ID: 1, Time: Epoch, Actor: SecretAuditActor, Action: "action.hold",
		Target: "lighthouse-geth-1", Detail: map[string]any{"reason": SecretHoldReason},
	}, {
		ID: 2, Time: Epoch, Actor: SecretAuditActor, Action: "settings.update",
		Target: "settings", Detail: map[string]any{"before": before, "after": after},
	}, {
		ID: 3, Time: Epoch, Actor: SecretAuditActor, Action: "sources.validator-ranges.refresh",
		Target: "validator-ranges", Detail: map[string]any{"changed": false, "error": SecretRangesError},
	}}

	health := fleet.NetworkHealth{
		UpdatedAt: Epoch, Error: SecretBeaconError,
		GenesisTime:    Epoch.Add(-24 * time.Hour),
		SecondsPerSlot: 12, SlotsPerEpoch: 32, HeadSlot: 7200, CurrentEpoch: 225,
		Phases:         fleet.SinglePhase(Epoch.Add(-24*time.Hour), 12*time.Second),
		FinalizedEpoch: 223, FinalityLagEpochs: 2, LivenessKnown: true, LivenessEpoch: 224,
		Tracked: 200, Live: 190, ValidatorRangesError: SecretRangesError,
	}

	beacon.Network = health
	sched.Set(func(f *Scheduler) {
		f.CurrentPlan = &scheduler.Plan{
			ComputedAt: Epoch,
			Gate: scheduler.Gate{
				OK: false, Reasons: []string{"participation 88% < 90%"}, Network: health,
			},
			Budget: scheduler.Budget{TotalWeight: 200, AllowedWeight: 20, UnavailableWeight: 10},
			Queue: []scheduler.QueueEntry{{
				Host: "lighthouse-geth-1", Position: 1, Reason: SecretHoldReason,
			}, {
				Host: "lighthouse-geth-2", Position: 2,
				Reason: scheduler.ReasonGatePrefix + "beacon unavailable: " + SecretBeaconError,
			}},
			Hosts: map[string]scheduler.HostPlan{
				"lighthouse-geth-1": {
					Slot: scheduler.SlotHeld, Weight: 100, Live: 90, LiveKnown: true,
					LeaseID: "lease-zz", Reason: SecretHoldReason,
				},
				"lighthouse-geth-2": {
					Slot: scheduler.SlotQueued, Weight: 100, Live: 100, LiveKnown: true,
					Reason: scheduler.ReasonGatePrefix + "beacon unavailable: " + SecretBeaconError,
				},
			},
		}
	})
}
