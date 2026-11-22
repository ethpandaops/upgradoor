package lease

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// retryWait lets the runner retry a host that does not answer a few times.
const retryWait = 50 * time.Millisecond

// An expired lease is declined, never failed, and nothing is touched.
func TestExpiredLeaseNeverStarts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expires time.Duration
		ttl     int64
	}{
		{name: "expiresAt has passed", expires: -time.Second, ttl: 60},
		{name: "no lifetime left at receipt", expires: time.Hour, ttl: 0},
		{name: "controller clock behind", expires: -time.Hour, ttl: 1800},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sc := agenttest.BaseScenario()
			sc.RebootRequired = true

			e := newEnv(t, sc)
			e.pull(agenttest.GethImage, agenttest.GethV2)
			e.start(t.Context())

			l := e.lease("lease-late", gethNeed(agenttest.GethV2), agenttest.RebootNeed())
			l.ExpiresAt = e.now().Add(tt.expires)
			l.TTLSeconds = tt.ttl
			e.follow(l)

			op := e.awaitFinished("lease-late")
			assert.Equal(t, protocol.PhaseDone, op.Phase, "an expired lease is not a failure")
			assert.False(t, op.Disrupted)
			assert.Empty(t, op.Error)

			want := fmt.Sprintf("lease deadline %s passed before the operation started",
				e.current().Current().Deadline.UTC().Format(time.RFC3339))

			for _, s := range op.Steps {
				assert.Equal(t, protocol.StepRefused, s.Status)
				assert.Equal(t, want, s.Error)
			}

			assert.Empty(t, e.disruptive())
			assert.Equal(t, "boot", e.host.BootID())
		})
	}
}

// A hold declines the lease, also while Docker does not answer or when it arrives during planning.
func TestHeldHostDeclinesTheLease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// at is the overrides read after which the hold is set; 0 from the start.
		at     int
		docker error
	}{
		{name: "held"},
		{name: "held while docker does not answer", docker: errors.New("docker is gone")},
		{name: "hold set during planning", at: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, agenttest.BaseScenario())
			e.pull(agenttest.GethImage, agenttest.GethV2)

			hold := func(o *plan.Overrides) {
				o.Holds = map[string]plan.Hold{protocol.HolderHost: {Reason: "maintenance"}}
			}
			if tt.at == 0 {
				e.files.set(hold)
			} else {
				e.files.onLoad(func(n int) {
					if n == tt.at {
						e.files.set(hold)
					}
				})
			}

			if tt.docker != nil {
				e.host.Fail("observe", tt.docker)
			}

			e.start(t.Context())

			need := gethNeed(agenttest.GethV2)
			e.grant("lease-held", need)

			op := e.awaitFinished("lease-held")
			assert.Equal(t, protocol.PhaseDone, op.Phase)
			assert.Equal(t, protocol.StepRefused, stepOf(t, op, need.ID).Status)
			assert.Equal(t, "host is on local hold: maintenance", stepOf(t, op, need.ID).Error)
			assert.Empty(t, e.disruptive())
		})
	}
}

// The start excludes a withdrawal or cancel that arrives during planning.
func TestStartExcludesWithdrawalAndCancel(t *testing.T) {
	t.Parallel()

	for _, cancel := range []bool{false, true} {
		reason := reasonWithdrawn
		if cancel {
			reason = reasonCancelled
		}

		t.Run(reason, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, agenttest.BaseScenario())
			e.pull(agenttest.GethImage, agenttest.GethV2)
			r := e.start(t.Context())

			need := gethNeed(agenttest.GethV2)
			l := e.lease("lease-1", need)

			e.files.onLoad(func(n int) {
				if n != 1 {
					return
				}

				if cancel {
					c := *l
					c.Cancel = true
					r.Follow(&c, nil)
				} else {
					r.Follow(nil, nil)
				}
			})
			require.True(t, r.Follow(l, nil))

			op := e.awaitFinished("lease-1")
			assert.Equal(t, protocol.PhaseDone, op.Phase)
			assert.False(t, op.Disrupted)
			assert.Equal(t, reason, stepOf(t, op, need.ID).Error)
			assert.False(t, e.current().Current().Started)
			assert.Empty(t, e.disruptive())
		})
	}
}

// A deadline that passes during planning declines the lease.
func TestDeadlinePassesWhileTheOperationIsPrepared(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.files.onLoad(func(n int) {
		if n == 2 {
			e.offset.Store(int64(2 * time.Minute))
		}
	})
	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-1", need)

	op := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.Equal(t, "lease deadline passed while the operation was prepared", stepOf(t, op, need.ID).Error)
	assert.Empty(t, e.disruptive())
}

// An unreadable system state keeps a reboot from being planned until the deadline.
func TestSystemStateThatCannotBeRead(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.Fail("status", errors.New("dpkg is busy"))
	e.start(t.Context())

	e.grant("lease-1", agenttest.RebootNeed())

	time.Sleep(retryWait)
	e.offset.Store(int64(2 * time.Minute))

	op := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.Equal(t, "lease deadline passed before the host could be examined: "+
		"cannot read the system state: agenttest: status: dpkg is busy",
		stepOf(t, op, protocol.RebootNeedID).Error)
	assert.Empty(t, e.disruptive())
}

// Docker not answering is retried while the lease is good.
func TestHostThatCannotBeExaminedDoesNotFailTheLease(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.host.Fail("observe", errors.New("docker daemon is restarting"))
	r := e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-retry", need)

	time.Sleep(retryWait)
	assert.Nil(t, r.Current().Reported(), "nothing is decided while the host cannot be examined")
	assert.Empty(t, e.disruptive())

	e.host.Fail("observe", nil)

	op := e.awaitFinished("lease-retry")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, need.ID).Status)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
}

// A lease that runs out while the host cannot be examined is declined, not failed.
func TestLeaseRunsOutWhileTheHostCannotBeExamined(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.host.Fail("observe", errors.New("docker daemon is gone"))
	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-blind", need)

	time.Sleep(retryWait)
	e.offset.Store(int64(2 * time.Minute))

	op := e.awaitFinished("lease-blind")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Empty(t, op.Error)

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepRefused, step.Status)
	assert.Contains(t, step.Error, "before the host could be examined")
	assert.Contains(t, step.Error, "docker daemon is gone")
	assert.Empty(t, e.disruptive())
	assert.Equal(t, agenttest.GethV1, e.host.Container("geth").Digest)
}
