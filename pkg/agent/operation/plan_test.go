package operation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestExpiredLeaseNeverStarts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expires time.Duration
		ttl     int64
	}{
		{name: "expiresAt has passed", expires: -time.Second, ttl: 60},
		{name: "no lifetime left at receipt", expires: time.Hour, ttl: 0},
		// A controller clock far behind: expiresAt looks old although the
		// lease is fresh. The earlier bound wins, in either direction.
		{name: "controller clock behind", expires: -time.Hour, ttl: 1800},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sc := agenttest.BaseScenario()
			sc.RebootRequired = true

			e := agenttest.NewEnv(t, sc)
			e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
			e.Start()

			needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.RebootNeedID)

			lease := e.Lease("lease-late", needs...)
			lease.ExpiresAt = e.Now().Add(tt.expires)
			lease.TTLSeconds = tt.ttl
			e.Ctrl.Grant(lease)

			op := e.Ctrl.AwaitFinished("lease-late")
			assert.Equal(t, protocol.PhaseDone, op.Phase, "an expired lease is not a failure")
			assert.False(t, op.Disrupted)
			assert.Empty(t, op.Error)

			for _, s := range op.Steps {
				assert.Equal(t, protocol.StepRefused, s.Status)
				assert.Contains(t, s.Error, "deadline")
			}

			assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))
			assert.Equal(t, "boot", e.Host.BootID())

			// The needs are still open and are restated.
			req := e.Ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool {
				return req.Operation == nil
			})
			assert.Len(t, req.Needs, 2)
		})
	}
}

func TestLeaseCannotWidenWhatTheAgentDoes(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := agenttest.NewEnv(t, sc)
	e.Cfg.Verbs = []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages}
	e.Host.SetRegistry("ethpandaops/dora:latest", "sha256:dora-2", "")
	e.Start()

	e.Ctrl.AwaitSyncs(2)

	openssl := []protocol.PackageChange{
		{Name: "docker-ce", From: "28.0", To: "28.1"},
		{Name: "openssl", From: "3.5.1", To: "3.5.2"},
	}
	needs := []protocol.Need{
		// reboot is not an allowed verb on this host
		agenttest.RebootNeed(),
		// dora is not a managed container
		agenttest.ImageNeedFor("dora", "ethpandaops/dora:latest", "sha256:dora-1", "sha256:dora-2"),
		// openssl is not in the blocked set
		{
			ID: "packages/widened", Kind: protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: openssl},
		},
		// a kind outside the closed verb set
		{ID: "exec/1", Kind: "exec"},
		// a manual need the agent never created
		{
			ID: protocol.ManualNeedID("forged"), Kind: protocol.NeedImage, DirectiveID: "forged",
			Image: &protocol.ImageNeed{Container: "geth", Image: agenttest.GethImage, FromDigest: agenttest.GethV1, ToDigest: agenttest.GethV1},
		},
		// geth already runs this digest
		agenttest.ImageNeedFor("geth", agenttest.GethImage, "sha256:geth-0", agenttest.GethV1),
		// teku is on another tag than the need says
		agenttest.ImageNeedFor("teku", "consensys/teku:latest", agenttest.TekuV1, agenttest.TekuV2),
	}
	e.Ctrl.Grant(e.Lease("lease-wide", needs...))

	op := e.Ctrl.AwaitFinished("lease-wide")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	want := []struct {
		status protocol.StepStatus
		reason string
	}{
		{protocol.StepRefused, "verb reboot is not allowed"},
		{protocol.StepRefused, "dora is not managed"},
		{protocol.StepRefused, `"openssl" is not in the configured blocked set`},
		{protocol.StepRefused, "unknown need kind"},
		{protocol.StepSuperseded, "manual need is no longer open"},
		{protocol.StepSuperseded, "already runs the wanted digest"},
		{protocol.StepSuperseded, "the need was for consensys/teku:latest"},
	}

	require.Len(t, op.Steps, len(want))

	for i, w := range want {
		assert.Equal(t, w.status, op.Steps[i].Status, needs[i].ID)
		assert.Contains(t, op.Steps[i].Error, w.reason, needs[i].ID)
	}

	assert.Empty(t, e.Host.Calls(), "nothing was touched, not even pulled")
	assert.Equal(t, "boot", e.Host.BootID())
}

// A Docker daemon that does not answer when the lease arrives is no reason
// to report a failed operation, which would halt the rollout: the agent
// looks again for as long as the lease is good.
func TestHostThatCannotBeExaminedDoesNotFailTheLease(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	agenttest.GrantUnexamined(t, e, "docker daemon is restarting", e.Lease("lease-retry", needs...))
	e.Ctrl.AwaitSyncs(8)

	for _, s := range e.Ctrl.AllSyncs() {
		require.Nil(t, s.Operation, "nothing is decided while the host cannot be examined")
	}

	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))

	e.Host.Fail("observe", nil)

	op := e.Ctrl.AwaitFinished("lease-retry")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, agenttest.GethV2, e.Host.Container("geth").Digest)
}

// If the host cannot be examined until the lease has run out, the lease
// was never started. That is not a failure either.
func TestLeaseRunsOutWhileTheHostCannotBeExamined(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	// Let the agent look (in vain) while the lease is still good, then let
	// the lease run out.
	agenttest.GrantUnexamined(t, e, "docker daemon is gone", e.Lease("lease-blind", needs...))
	e.Ctrl.AwaitSyncs(8)
	e.ClockOffset.Store(int64(2 * time.Minute))

	op := e.Ctrl.AwaitFinished("lease-blind")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Empty(t, op.Error)

	step := agenttest.StepOf(t, op, needs[0].ID)
	assert.Equal(t, protocol.StepRefused, step.Status)
	assert.Contains(t, step.Error, "before the host could be examined")
	assert.Contains(t, step.Error, "docker daemon is gone")
	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))
	assert.Equal(t, agenttest.GethV1, e.Host.Container("geth").Digest)
}
