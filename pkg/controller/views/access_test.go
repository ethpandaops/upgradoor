package views

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func accessHost() *HostSummary {
	return &HostSummary{
		Name: "a", AgentVersion: "v1", UptimeSeconds: 9,
		OS:        protocol.OSSummary{RunningKernel: "6.12"},
		Pending:   protocol.Pending{Upgradable: 3},
		Actions:   []protocol.ActionInfo{{Name: "flush"}},
		Resources: &protocol.Resources{Memory: protocol.MemoryStats{Total: 8}},
		Containers: []protocol.Container{
			{
				Name: "geth", ID: "c1", Image: "geth:1", Managed: true, Team: "geth", Command: []string{"--x"},
				Stats: &protocol.ContainerStats{Memory: 4},
			},
			{Name: "teku", ID: "c2", Image: "teku:1", Managed: true, Team: "teku", Command: []string{"--y"}},
			{Name: "shell", ID: "c3", Image: "debug:1"},
		},
		Needs: []protocol.Need{
			{ID: "p", Kind: protocol.NeedPackages, Packages: &protocol.PackagesNeed{}},
			{ID: "i", Kind: protocol.NeedImage, PullError: "e", Image: &protocol.ImageNeed{Team: "geth"}},
			{ID: "s", Kind: protocol.NeedSpec, Spec: &protocol.SpecNeed{
				Team: "teku", Flags: true, Command: &protocol.CommandDelta{Added: []string{"--z"}},
			}},
		},
	}
}

func TestAccessSummary(t *testing.T) {
	sum := accessHost()

	full := Access{Full: true}
	assert.Equal(t, *sum, full.Summary(sum))

	twin := Access{Teams: []string{"teku"}, OwnedHosts: true}
	assert.Equal(t, *sum, twin.Summary(sum), "a team of the login owns a container")

	member := Access{Teams: []string{"geth"}}
	got := member.Summary(sum)

	assert.Empty(t, got.AgentVersion)
	assert.Zero(t, got.UptimeSeconds)
	assert.Zero(t, got.OS)
	assert.Zero(t, got.Pending)
	assert.Nil(t, got.Resources)
	assert.Empty(t, got.Actions)
	require.Len(t, got.Containers, 2, "no unmanaged container")
	assert.Equal(t, sum.Containers[0], got.Containers[0], "the team's own container in full, stats too")
	assert.Equal(t, protocol.Container{Name: "teku", Image: "teku:1", Managed: true, Team: "teku"},
		got.Containers[1])
	assert.Nil(t, got.Needs[0].Packages)
	assert.Equal(t, "e", got.Needs[1].PullError, "the need of the team's container")
	assert.Nil(t, got.Needs[2].Spec.Command, "another team's command line")
	assert.True(t, got.Needs[2].Spec.Flags)
	assert.NotNil(t, sum.Needs[2].Spec.Command)

	assert.Equal(t, "c1", sum.Containers[0].ID, "the summary is not changed")
	assert.NotNil(t, sum.Resources)
	assert.NotNil(t, sum.Needs[0].Packages)
}

func TestAccessDetail(t *testing.T) {
	d := &HostDetail{
		HostSummary: *accessHost(), CertHash: "h", RemoteAddr: "1.2.3.4",
		Inventory: &protocol.Inventory{}, Assigned: 4,
		Leases: []*fleet.Lease{{ID: "l", Operation: &protocol.Operation{
			Steps: []protocol.Step{{Action: &protocol.ActionResult{ExitCode: 1, Output: "out"}}},
		}}},
		History: []fleet.AuditEntry{{Action: "host.registered", Detail: map[string]any{
			"certHash": "h", "remoteAddr": "1.2.3.4", "roles": []string{"x"},
		}}},
	}

	got := (&Access{}).Detail(d)

	assert.True(t, got.Reduced)
	assert.Empty(t, got.CertHash)
	assert.Empty(t, got.RemoteAddr)
	assert.Nil(t, got.Inventory)
	assert.Equal(t, uint64(4), got.Assigned)
	assert.Equal(t, protocol.ActionResult{ExitCode: 1}, *got.Leases[0].Operation.Steps[0].Action)
	assert.Equal(t, map[string]any{"roles": []string{"x"}}, got.History[0].Detail)
	assert.Equal(t, "out", d.Leases[0].Operation.Steps[0].Action.Output, "the detail is not changed")

	assert.Same(t, d, (&Access{Full: true}).Detail(d))
	assert.Nil(t, (&Access{}).Detail(nil))
}

func TestAccessClientHost(t *testing.T) {
	sum := accessHost()
	row := newClientHost(sum, "teku")
	require.NotNil(t, row.Usage)

	got := (&Access{Teams: []string{"geth"}, OwnedHosts: true}).ClientHost(&row)
	assert.Nil(t, got.Usage, "the login owns no container of the row")
	assert.Equal(t, []protocol.Container{ReducedContainer(&sum.Containers[1])}, got.Containers)

	twin := (&Access{Teams: []string{"teku"}, OwnedHosts: true}).ClientHost(&row)
	assert.Equal(t, row, twin)
}

func TestAccessRollout(t *testing.T) {
	r := &RolloutView{Rollout: fleet.Rollout{
		Kind: fleet.RolloutPackages, Title: "packages: openssl 3",
		Packages: []protocol.PackageChange{{Name: "openssl"}},
	}, Policy: fleet.PolicyManual}

	got := (&Access{}).Rollout(r)
	assert.Equal(t, "packages", got.Title)
	assert.Nil(t, got.Packages)
	assert.Equal(t, fleet.PolicyManual, got.Policy)
	assert.Same(t, r, (&Access{Full: true}).Rollout(r))

	group := &RolloutView{Rollout: fleet.Rollout{
		Kind: fleet.RolloutGroup, Team: "geth",
		Targets: []fleet.RolloutTarget{{Spec: "k", Flags: true, Command: &protocol.CommandDelta{Added: []string{"--x"}}}},
	}}
	assert.Same(t, group, (&Access{Teams: []string{"geth"}}).Rollout(group), "the team's own")

	other := (&Access{Teams: []string{"teku"}}).Rollout(group)
	assert.Nil(t, other.Targets[0].Command, "another team's command line")
	assert.True(t, other.Targets[0].Flags)
	assert.NotNil(t, group.Targets[0].Command)

	entries := (&Access{}).Audit([]fleet.AuditEntry{{
		Action: "rollout.created", Detail: map[string]any{"title": "packages: openssl 3"},
	}})
	assert.Equal(t, "packages", entries[0].Detail["title"])
}

// A step error is the internals of the need's container: shown to its team.
// The pull in flight names an image like the needs do: shown to all.
func TestAccessLease(t *testing.T) {
	l := &fleet.Lease{
		Needs: []protocol.Need{
			{ID: "i", Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Team: "geth"}},
			{ID: "p", Kind: protocol.NeedPackages, Packages: &protocol.PackagesNeed{}},
		},
		Error: "step p failed",
		Operation: &protocol.Operation{Error: "failed", Steps: []protocol.Step{
			{NeedID: "i", Status: protocol.StepDone, Error: "pull e"},
			{NeedID: "p", Status: protocol.StepFailed, Error: "dpkg e"},
		}, Pull: &protocol.PullProgress{Image: "ethpandaops/geth:master", Attempt: 2, BytesDone: 5}},
		Baseline: &fleet.LeaseBaseline{Unready: "EL is not reachable", Node: protocol.NodeHealth{
			EL:     &protocol.ELHealth{Error: "dial 10.0.0.1"},
			Probes: []protocol.KindHealth{{Kind: "mev", Error: "dial 10.0.0.2"}},
		}},
	}

	got := (&Access{Teams: []string{"geth"}}).Lease(l)
	assert.Equal(t, "pull e", got.Operation.Steps[0].Error, "the team's container")
	assert.Empty(t, got.Operation.Steps[1].Error)
	assert.Equal(t, "step p failed", got.Error)
	assert.Equal(t, "failed", got.Operation.Error)
	assert.Equal(t, l.Operation.Pull, got.Operation.Pull)
	assert.Equal(t, "dpkg e", l.Operation.Steps[1].Error, "the lease is not changed")
	assert.Equal(t, "EL is not reachable", got.Baseline.Unready)
	assert.Empty(t, got.Baseline.Node.EL.Error, "the probes' errors are the host's internals")
	assert.Empty(t, got.Baseline.Node.Probes[0].Error)
	assert.Equal(t, "dial 10.0.0.1", l.Baseline.Node.EL.Error, "the lease is not changed")

	assert.Same(t, l, (&Access{Full: true}).Lease(l))
}

// Without admin a settings change shows what it changed, and a fetch of the
// validator ranges not its error; with details but without admin too.
func TestAccessAuditAdmin(t *testing.T) {
	before := fleet.DefaultSettings()
	after := before
	after.MaxUnavailablePercent = 25
	after.PolicyClients = fleet.ClientPolicies{"el": {"geth": fleet.PolicyManual}}

	in := []fleet.AuditEntry{
		{Action: "settings.update", Target: "settings", Detail: map[string]any{
			"before": before, "after": after, "reason": "r",
		}},
		{Action: "action.pause", Target: "settings", Detail: map[string]any{
			"before": map[string]any{"paused": false, "minPeers": 3.0},
			"after":  map[string]any{"paused": true, "minPeers": 3.0},
		}},
		{Action: "sources.validator-ranges.refresh", Target: "validator-ranges", Detail: map[string]any{
			"changed": false, "error": `Get "https://inventory.internal/ranges": EOF`,
		}},
	}

	for _, access := range []Access{{}, {Full: true}} {
		got := access.Audit(in)

		assert.Equal(t, map[string]any{
			"before": map[string]any{"maxUnavailablePercent": 10.0, "policyClients": map[string]any{}},
			"after": map[string]any{
				"maxUnavailablePercent": 25.0, "policyClients": map[string]any{"el": map[string]any{"geth": "manual"}},
			},
			"reason": "r",
		}, got[0].Detail)
		assert.Equal(t, map[string]any{
			"before": map[string]any{"paused": false}, "after": map[string]any{"paused": true},
		}, got[1].Detail)
		assert.Equal(t, map[string]any{"changed": false}, got[2].Detail)
	}

	assert.Equal(t, in, (&Access{Full: true, Admin: true}).Audit(in))
	assert.Equal(t, in[2].Detail, (&Access{Admin: true}).Audit(in)[2].Detail)
	assert.Contains(t, in[2].Detail, "error", "the entries are not changed")
}

func TestAccessKey(t *testing.T) {
	assert.Equal(t, (&Access{Teams: []string{"b", "a"}}).Key(), (&Access{Teams: []string{"a", "b", "a"}}).Key())
	assert.NotEqual(t, (&Access{Teams: []string{"a"}}).Key(), (&Access{Teams: []string{"a"}, OwnedHosts: true}).Key())
	assert.NotEqual(t, (&Access{}).Key(), (&Access{Full: true}).Key())
	assert.NotEqual(t, (&Access{Full: true}).Key(), (&Access{Full: true, Admin: true}).Key())
	assert.NotEqual(t, (&Access{}).Key(), (&Access{Admin: true}).Key())
}
