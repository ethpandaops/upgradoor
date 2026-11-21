package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// manualRollout adds an approved manual rollout by "alice" with one accepted
// reboot directive per host, and makes every host report the manual need.
func (b *worldBuilder) manualRollout(id string, force, ignoreBudget bool, hosts ...string) *fleet.Rollout {
	r := &fleet.Rollout{
		ID: id, Kind: fleet.RolloutManual, Key: prefixManual + id, State: fleet.RolloutActive,
		Approved: true, Force: force, IgnoreBudget: ignoreBudget,
		CreatedBy: "alice", CreatedAt: b.w.Now, UpdatedAt: b.w.Now,
	}

	for _, name := range hosts {
		dir := "d-" + id + "-" + name
		r.Hosts = append(r.Hosts, fleet.RolloutHost{Host: name, State: fleet.HostExpected, UpdatedAt: b.w.Now})
		b.w.Directives = append(b.w.Directives, &fleet.Directive{
			Directive: protocol.Directive{ID: dir, Kind: protocol.DirectiveReboot},
			Host:      name, State: fleet.DirectiveAccepted, RolloutID: id, CreatedBy: "alice",
		})
		b.wants(name, protocol.Need{
			ID: protocol.ManualNeedID(dir), Kind: protocol.NeedReboot, DirectiveID: dir,
			Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
		})
	}

	b.w.Rollouts = append(b.w.Rollouts, r)

	return r
}

func TestLeaseLifecycle(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 10)
	b.wants("a", packagesNeed("2.0"))
	key := packagesKey("2.0")

	out := b.step()
	l := b.lease("a")
	require.NotNil(t, l)
	assert.Equal(t, fleet.LeaseGranted, l.State)
	assert.Equal(t, uint64(10), l.Weight)
	assert.Equal(t, uint64(10), l.Charged)
	assert.False(t, l.Force)
	assert.Equal(t, map[string]string{l.Needs[0].ID: b.mustRollout(key).ID}, l.Rollouts)
	assert.Equal(t, fleet.HostLeased, b.mustRollout(key).Host("a").State)
	assert.Equal(t, l.ID, b.mustRollout(key).Host("a").LeaseID)
	assert.Equal(t, SlotLeased, out.Plan.Hosts["a"].Slot)
	assert.Equal(t, l.ID, out.Plan.Hosts["a"].LeaseID)
	assert.Empty(t, out.Plan.Queue)
	assert.Equal(t, []string{"rollout.created", "lease.granted"}, auditActions(out))
	assert.Equal(t, actorScheduler, out.Audit[1].Actor)
	assert.Equal(t, "a", out.Audit[1].Target)

	b.advance(5 * time.Second)
	b.report("a", protocol.PhaseApplying, true)
	b.step()
	assert.Equal(t, fleet.LeaseRunning, b.lease("a").State)
	assert.Equal(t, protocol.PhaseApplying, b.lease("a").Operation.Phase)

	b.advance(5 * time.Second)
	b.finish("a")
	doneAt := b.w.Now

	out = b.step()
	l = b.lease("a")
	assert.Equal(t, fleet.LeaseVerifying, l.State)
	require.NotNil(t, l.AgentDoneAt)
	assert.Equal(t, doneAt, *l.AgentDoneAt)
	assert.Equal(t, SlotVerifying, out.Plan.Hosts["a"].Slot)
	assert.Equal(t, uint64(10), out.Plan.Budget.UnavailableWeight, "still unavailable while verifying")
	assert.Equal(t, noteAwaitSync, l.Readiness)

	// The epoch the operation ended in does not count.
	b.advance(testEpoch)
	b.step()
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)
	assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

	b.advance(testEpoch)

	out = b.step()
	assert.Nil(t, b.lease("a"))
	assert.Equal(t, []string{"lease.released", "rollout.completed"}, auditActions(out))
	assert.Equal(t, fleet.HostDone, b.closed(key).Host("a").State)
	assert.Equal(t, fleet.LeaseReleased, out.Leases[0].State)
	assert.NotNil(t, out.Leases[0].EndedAt)
	assert.Equal(t, SlotIdle, out.Plan.Hosts["a"].Slot)
	assert.Equal(t, uint64(0), out.Plan.Budget.UnavailableWeight)
}

func TestGrantedLeaseExpiry(t *testing.T) {
	ttl := fleet.DefaultSettings().LeaseTTL.Std()

	tests := []struct {
		name string
		run  func(b *worldBuilder)
		// want is the state of the first lease afterwards.
		want    fleet.LeaseState
		wantErr string
		// regranted: the host holds a new lease.
		regranted bool
	}{
		{
			name: "not yet expired",
			run:  func(b *worldBuilder) { b.advance(ttl - time.Second) },
			want: fleet.LeaseGranted,
		},
		{
			name: "a sync without an operation right at the expiry proves nothing yet: the agent may just have started",
			run:  func(b *worldBuilder) { b.advance(ttl) },
			want: fleet.LeaseGranted,
		},
		{
			name: "a sync just before the grace period ends: still granted",
			run:  func(b *worldBuilder) { b.advance(ttl + leaseStartGrace - time.Second) },
			want: fleet.LeaseGranted,
		},
		{
			name:      "unused after the grace period: expired, the host waits again and is served again",
			run:       func(b *worldBuilder) { b.advance(ttl + leaseStartGrace) },
			want:      fleet.LeaseExpired,
			regranted: true,
		},
		{
			name: "started at the deadline, the report arrives within the grace period: running",
			run: func(b *worldBuilder) {
				// The sync at the expiry was built before the start.
				b.advance(ttl)
				b.step()
				b.advance(5 * time.Second)
				b.report("a", protocol.PhaseApplying, true)
			},
			want: fleet.LeaseRunning,
		},
		{
			name: "silent through the expiry: it may have started, the lease stays",
			run:  func(b *worldBuilder) { b.advance(ttl+10*time.Minute, "a") },
			want: fleet.LeaseGranted,
		},
		{
			name: "silent, then back without an operation: expired",
			run: func(b *worldBuilder) {
				b.advance(ttl+10*time.Minute, "a")
				b.step()
				b.advance(time.Minute)
			},
			want:      fleet.LeaseExpired,
			regranted: true,
		},
		{
			name: "silent, then back with an operation: running",
			run: func(b *worldBuilder) {
				b.advance(ttl+10*time.Minute, "a")
				b.step()
				b.advance(time.Minute)
				b.report("a", protocol.PhaseStarting, true)
			},
			want: fleet.LeaseRunning,
		},
		{
			name: "silent until just before another leaseTtl after the grace period: still granted",
			run:  func(b *worldBuilder) { b.advance(2*ttl+leaseStartGrace-time.Second, "a") },
			want: fleet.LeaseGranted,
		},
		{
			name:    "silent for another leaseTtl after the grace period: failed",
			run:     func(b *worldBuilder) { b.advance(2*ttl+leaseStartGrace, "a") },
			want:    fleet.LeaseFailed,
			wantErr: "agent silent",
		},
		{
			name: "started just in time",
			run: func(b *worldBuilder) {
				b.advance(ttl - time.Second)
				b.report("a", protocol.PhaseDraining, false)
				b.step()
				b.advance(10 * time.Minute)
			},
			want: fleet.LeaseRunning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", 0)
			b.wants("a", packagesNeed("2.0"))
			b.step()

			first := b.lease("a")
			require.NotNil(t, first)

			tt.run(b)
			out := b.step()

			state := fleet.LeaseGranted
			errText := ""

			if cur := b.lease("a"); cur != nil && cur.ID == first.ID {
				state = cur.State
			}

			for _, l := range out.Leases {
				if l.ID == first.ID {
					state, errText = l.State, l.Error
				}
			}

			assert.Equal(t, tt.want, state)
			assert.Equal(t, tt.wantErr, errText)
			assert.Equal(t, tt.regranted, b.lease("a") != nil && b.lease("a").ID != first.ID)

			if tt.want == fleet.LeaseExpired {
				assert.Contains(t, auditActions(out), "lease.expired")
				assert.Equal(t, fleet.RolloutActive, b.mustRollout(packagesKey("2.0")).State, "not a failure")
			}

			if tt.want == fleet.LeaseFailed {
				assert.Equal(t, fleet.RolloutHalted, b.mustRollout(packagesKey("2.0")).State)
			}
		})
	}
}

func TestRunningLeaseSilenceFailsAndHalts(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.host("b", 0)
	b.wants("a", packagesNeed("2.0"))
	b.wants("b", packagesNeed("2.0"))
	b.wants("b", rebootNeed())
	b.w.Settings.PolicyReboot = fleet.PolicyManual
	key := packagesKey("2.0")

	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())
	b.report("a", protocol.PhaseRebooting, true)
	b.step()

	// Progress reports keep a running lease alive without limit.
	for range 5 {
		b.advance(20 * time.Minute)
		b.step()
	}

	require.Equal(t, fleet.LeaseRunning, b.lease("a").State)

	// Silence for leaseTtl fails it.
	b.advance(30*time.Minute, "a")
	b.step()
	require.Equal(t, fleet.LeaseRunning, b.lease("a").State, "exactly leaseTtl is not yet over it")

	b.advance(time.Second, "a")

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
	assert.Equal(t, "agent silent", out.Leases[0].Error)
	assert.Equal(t, []string{"rollout.halted", "lease.failed"}, auditActions(out))

	r := b.mustRollout(key)
	assert.Equal(t, fleet.RolloutHalted, r.State)
	assert.Equal(t, "a: agent silent", r.Reason)
	assert.Equal(t, fleet.HostFailed, r.Host("a").State)
	assert.Equal(t, "agent silent", r.Host("a").Error)

	// Nothing more is granted for the halted rollout, however long it takes.
	b.w.Settings.PolicyReboot = fleet.PolicyAuto

	for range 3 {
		b.advance(time.Hour, "a")
		b.step()
	}

	l := b.lease("b")
	require.NotNil(t, l, "the reboot rollout is not halted")
	require.Len(t, l.Needs, 1)
	assert.Equal(t, protocol.NeedReboot, l.Needs[0].Kind)
	assert.Equal(t, fleet.HostPending, b.mustRollout(key).Host("b").State)

	// A retry (failed hosts back to pending, state active) lifts the block.
	// While "a" still reports the operation it works without a lease: it
	// counts as leased and is granted nothing.
	r = b.mustRollout(key)
	r.State = fleet.RolloutActive
	r.Host("a").State = fleet.HostPending
	b.serve("b")

	out = b.step()
	require.Empty(t, b.leasedHosts())
	assert.Equal(t, reasonWorking, queueReasons(out.Plan)["a"])
	assert.Equal(t, 1, out.Plan.Budget.LeasedHosts)
	assert.Equal(t, SlotLeased, out.Plan.Hosts["a"].Slot)

	// Its operation is over.
	st := *b.get("a").Status
	st.Operation = nil
	b.get("a").Status = &st
	b.step()

	// "a" is back and sorts first.
	require.Equal(t, []string{"a"}, b.leasedHosts())
	assert.Equal(t, protocol.NeedPackages, b.lease("a").Needs[0].Kind)
}

func TestOperationThatDisruptedNothing(t *testing.T) {
	t.Run("the need is gone: done", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 10)
		b.wants("a", packagesNeed("2.0"))
		b.step()

		b.report("a", protocol.PhaseDone, false, protocol.StepSuperseded)
		b.get("a").Status.Needs = nil

		out := b.step()
		assert.Equal(t, fleet.LeaseExpired, out.Leases[0].State)
		assert.Equal(t, []string{"lease.expired", "rollout.completed"}, auditActions(out))
		assert.Equal(t, fleet.HostDone, b.closed(packagesKey("2.0")).Host("a").State)
		assert.Equal(t, uint64(0), out.Plan.Budget.UnavailableWeight)
	})

	t.Run("the need is still there: pending, not failed", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 10)
		b.host("b", 10)
		b.wants("a", packagesNeed("2.0"))
		b.wants("b", packagesNeed("2.0"))
		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())

		b.report("a", protocol.PhaseDone, false, protocol.StepRefused)
		b.w.Settings.Paused = true

		out := b.step()
		assert.Equal(t, fleet.LeaseExpired, out.Leases[0].State)

		r := b.mustRollout(packagesKey("2.0"))
		assert.Equal(t, fleet.RolloutActive, r.State)
		assert.Equal(t, fleet.HostPending, r.Host("a").State)
		assert.Empty(t, r.Host("a").LeaseID)
	})
}

func TestStepOutcomes(t *testing.T) {
	imgKey := imageKey(imgLighthouse, digestA)
	pkgKey := packagesKey("2.0")

	tests := []struct {
		name string
		// phase and statuses of the image step and the packages step.
		phase    protocol.Phase
		statuses []protocol.StepStatus
		// silent: the agent never reports and goes silent instead.
		silent    bool
		wantLease fleet.LeaseState
		wantImage fleet.HostState
		wantPkgs  fleet.HostState
		// halted lists which rollouts end up halted.
		haltImage, haltPkgs bool
	}{
		{
			name:      "both done",
			phase:     protocol.PhaseDone,
			statuses:  []protocol.StepStatus{protocol.StepDone, protocol.StepDone},
			wantLease: fleet.LeaseReleased,
			wantImage: fleet.HostDone,
			wantPkgs:  fleet.HostDone,
		},
		{
			name:      "a superseded step returns to pending",
			phase:     protocol.PhaseDone,
			statuses:  []protocol.StepStatus{protocol.StepDone, protocol.StepSuperseded},
			wantLease: fleet.LeaseReleased,
			wantImage: fleet.HostDone,
			wantPkgs:  fleet.HostPending,
		},
		{
			name:      "a refused step returns to pending",
			phase:     protocol.PhaseDone,
			statuses:  []protocol.StepStatus{protocol.StepRefused, protocol.StepDone},
			wantLease: fleet.LeaseReleased,
			wantImage: fleet.HostPending,
			wantPkgs:  fleet.HostDone,
		},
		{
			name:      "the failure is another step's: the done step's rollout goes on",
			phase:     protocol.PhaseFailed,
			statuses:  []protocol.StepStatus{protocol.StepDone, protocol.StepFailed},
			wantLease: fleet.LeaseFailed,
			wantImage: fleet.HostDone,
			wantPkgs:  fleet.HostFailed,
			haltPkgs:  true,
		},
		{
			name:      "the image step failed: the digest is blocked, packages go on",
			phase:     protocol.PhaseFailed,
			statuses:  []protocol.StepStatus{protocol.StepFailed, protocol.StepDone},
			wantLease: fleet.LeaseFailed,
			wantImage: fleet.HostFailed,
			wantPkgs:  fleet.HostDone,
			haltImage: true,
		},
		{
			name:      "failed with no failed step: in doubt, all of them",
			phase:     protocol.PhaseFailed,
			statuses:  []protocol.StepStatus{protocol.StepDone, protocol.StepDone},
			wantLease: fleet.LeaseFailed,
			wantImage: fleet.HostFailed,
			wantPkgs:  fleet.HostFailed,
			haltImage: true,
			haltPkgs:  true,
		},
		{
			name:      "a superseded step beside a failed one fails too",
			phase:     protocol.PhaseFailed,
			statuses:  []protocol.StepStatus{protocol.StepSuperseded, protocol.StepFailed},
			wantLease: fleet.LeaseFailed,
			wantImage: fleet.HostFailed,
			wantPkgs:  fleet.HostFailed,
			haltImage: true,
			haltPkgs:  true,
		},
		{
			name:      "agent silent: all of them",
			silent:    true,
			wantLease: fleet.LeaseFailed,
			wantImage: fleet.HostFailed,
			wantPkgs:  fleet.HostFailed,
			haltImage: true,
			haltPkgs:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", 0)
			b.host("z", 0)
			b.wantsImage("a", imgLighthouse, digestA)
			b.wants("a", packagesNeed("2.0"))
			// "z" keeps both rollouts open and shows whether they go on.
			b.wantsImage("z", imgLighthouse, digestA)
			b.wants("z", packagesNeed("2.0"))
			b.step()

			l := b.lease("a")
			require.NotNil(t, l)
			require.Len(t, l.Needs, 2)
			require.Equal(t, protocol.NeedImage, l.Needs[0].Kind)
			require.Nil(t, b.lease("z"), "host limit 1")

			var final *fleet.Lease

			if tt.silent {
				b.report("a", protocol.PhaseApplying, true)
				b.step()
				b.advance(31*time.Minute, "a")
			} else {
				b.report("a", tt.phase, true, tt.statuses...)
				// The agent no longer reports what it finished.
				b.get("a").Status.Needs = nil
				b.step()
				b.advance(5 * time.Second)
				b.step()
				b.advance(2 * testEpoch)
			}

			b.w.Settings.Paused = true // look at the result before "z" is served

			for _, out := range []*Outcome{b.step()} {
				for _, x := range out.Leases {
					if x.ID == l.ID {
						final = x
					}
				}
			}

			if final == nil {
				// The lease ended in an earlier step.
				require.Nil(t, b.lease(l.Host))
			} else {
				assert.Equal(t, tt.wantLease, final.State)
			}

			img, pkgs := b.mustRollout(imgKey), b.mustRollout(pkgKey)

			wantImage, wantPkgs := tt.wantImage, tt.wantPkgs
			// A member whose step returned to pending and whose need is gone
			// is done by the membership rule; for an image it is expected
			// again while the container still runs the old digest.
			if wantImage == fleet.HostPending {
				wantImage = fleet.HostExpected
			}

			if wantPkgs == fleet.HostPending {
				wantPkgs = fleet.HostDone
			}

			assert.Equal(t, wantImage, img.Host("a").State)
			assert.Equal(t, wantPkgs, pkgs.Host("a").State)
			assert.Equal(t, tt.haltImage, img.State == fleet.RolloutHalted)
			assert.Equal(t, tt.haltPkgs, pkgs.State == fleet.RolloutHalted)

			// "z" is granted exactly the rollouts that are not halted.
			b.w.Settings.Paused = false
			b.step()

			kinds := map[protocol.NeedKind]bool{}

			if lz := b.lease("z"); lz != nil {
				for _, n := range lz.Needs {
					kinds[n.Kind] = true
				}
			}

			assert.Equal(t, !tt.haltImage, kinds[protocol.NeedImage])
			assert.Equal(t, !tt.haltPkgs, kinds[protocol.NeedPackages])
		})
	}
}

func TestSupersededStepStaysPendingWhileTheNeedIsReported(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.wants("a", packagesNeed("2.0"), rebootNeed())
	b.step()

	l := b.lease("a")
	require.Len(t, l.Needs, 2)

	// Packages done, reboot refused; the agent still reports the reboot.
	b.report("a", protocol.PhaseDone, true, protocol.StepDone, protocol.StepRefused)
	b.get("a").Status.Needs = []protocol.Need{rebootNeed()}
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)
	b.w.Settings.Paused = true
	b.step()

	assert.Equal(t, fleet.HostPending, b.mustRollout("reboot").Host("a").State)
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)
}

func TestNeedStillReportedAfterTheOperationFails(t *testing.T) {
	tests := []struct {
		name string
		need protocol.Need
		key  string
		// weight of the host: validator hosts are verified by liveness.
		weight uint64
	}{
		{name: "image", need: imageNeed("c", imgLighthouse, digestA), key: imageKey(imgLighthouse, digestA)},
		{name: "packages", need: packagesNeed("2.0"), key: packagesKey("2.0")},
		{name: "reboot: the host came back still requiring one", need: rebootNeed(), key: keyReboot},
		{name: "reboot on a validator host", need: rebootNeed(), key: keyReboot, weight: 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", tt.weight)
			b.wants("a", tt.need)
			b.step()
			require.Equal(t, []string{"a"}, b.leasedHosts())

			// The agent says done but keeps reporting the same need.
			b.report("a", protocol.PhaseDone, true)
			b.step()
			b.advance(5 * time.Second)
			b.step()
			b.advance(2 * testEpoch)

			out := b.step()

			require.Len(t, out.Leases, 1)
			assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
			assert.Equal(t, "need "+tt.need.ID+" is still reported after the operation", out.Leases[0].Error)

			r := b.mustRollout(tt.key)
			assert.Equal(t, fleet.RolloutHalted, r.State)
			assert.Equal(t, fleet.HostFailed, r.Host("a").State)

			// No loop: nothing is granted, tick after tick, until a person
			// retries.
			for range 5 {
				b.advance(testEpoch)
				b.step()
				assert.Empty(t, b.leasedHosts(), "no loop of grants")
			}

			assert.Empty(t, b.closedRollouts, "the cause stays open and halted")
		})
	}
}

func TestDoneReportAloneDoesNotReleaseWithoutALaterSync(t *testing.T) {
	// With readyEpochs 0 the readiness check of a validator host holds at once, but
	// the needs of the sync that reported done may predate the operation.
	b := newWorldBuilder(t)
	b.w.Settings.ReadyEpochs = 0
	b.host("a", 10)
	b.wants("a", rebootNeed())
	b.step()

	b.report("a", protocol.PhaseDone, true)
	b.step()

	l := b.lease("a")
	require.NotNil(t, l)
	assert.Equal(t, fleet.LeaseVerifying, l.State)
	assert.Equal(t, noteAwaitSync, l.Readiness)

	// The next sync no longer lists the need: released.
	b.get("a").Status.Needs = nil
	b.advance(5 * time.Second)

	out := b.step()
	require.Len(t, out.Leases, 1)
	assert.Equal(t, fleet.LeaseReleased, out.Leases[0].State)
}

func TestRunningLeaseWhoseOperationIsGoneFails(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.host("b", 0)
	b.wants("a", packagesNeed("2.0"))
	b.wants("b", packagesNeed("2.0"))
	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())

	b.report("a", protocol.PhaseApplying, true)
	b.step()
	require.Equal(t, fleet.LeaseRunning, b.lease("a").State)

	// The agent lost its state file: it syncs without the operation.
	b.advance(5 * time.Second)

	st := *b.get("a").Status
	st.Operation = nil
	b.get("a").Status = &st

	out := b.step()
	require.Len(t, out.Leases, 1)
	assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
	assert.Equal(t, "the agent no longer reports the operation", out.Leases[0].Error)

	r := b.mustRollout(packagesKey("2.0"))
	assert.Equal(t, fleet.RolloutHalted, r.State)
	assert.Equal(t, "a: the agent no longer reports the operation", r.Reason)
	assert.Empty(t, b.leasedHosts(), "b is not granted in a halted rollout")
}

func TestReadinessEpochCounting(t *testing.T) {
	// The agent reports done 10 s into epoch 100.
	tests := []struct {
		name   string
		weight uint64
		epochs uint64
		// rows maps epochs to how many of the host's validators attested.
		rows map[uint64]uint64
		want bool
		note string
	}{
		{
			name:   "the epoch the operation ended in does not count",
			epochs: 1, rows: map[uint64]uint64{100: 10},
			note: "validators attesting: 0 of 1 epochs with at least 9 of 10",
		},
		{
			name:   "one complete epoch after done",
			epochs: 1, rows: map[uint64]uint64{100: 0, 101: 10},
			want: true,
			note: "ready: no node probe configured, validators attesting",
		},
		{name: "minParticipation of the host's weight is enough", epochs: 1, rows: map[uint64]uint64{101: 9}, want: true},
		{name: "one validator short", epochs: 1, rows: map[uint64]uint64{101: 8}},
		{name: "two epochs wanted, one seen", epochs: 2, rows: map[uint64]uint64{101: 10}, note: "validators attesting: 1 of 2 epochs with at least 9 of 10"},
		{name: "two consecutive epochs", epochs: 2, rows: map[uint64]uint64{101: 10, 102: 10}, want: true},
		{name: "the run must be unbroken", epochs: 2, rows: map[uint64]uint64{101: 10, 102: 3}},
		{name: "a new run after a bad epoch", epochs: 2, rows: map[uint64]uint64{101: 3, 102: 10, 103: 10}, want: true},
		{name: "a missing epoch breaks the run", epochs: 2, rows: map[uint64]uint64{101: 10, 103: 10}},
		{name: "three epochs", epochs: 3, rows: map[uint64]uint64{101: 10, 102: 9, 103: 10}, want: true},
		{name: "a single validator must attest", weight: 1, epochs: 1, rows: map[uint64]uint64{101: 0}, note: "validators attesting: 0 of 1 epochs with at least 1 of 1"},
		{name: "a single validator attests", weight: 1, epochs: 1, rows: map[uint64]uint64{101: 1}, want: true},
		{name: "readyEpochs 0", epochs: 0, rows: map[uint64]uint64{100: 0}, want: true},
		{name: "no rows after done at all", epochs: 1, rows: map[uint64]uint64{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.autoLive = false
			b.w.Settings.ReadyEpochs = tt.epochs

			weight := tt.weight
			if weight == 0 {
				weight = 10
			}

			b.host("a", weight)
			b.host("other", 10)

			// The node part of the check holds throughout: no probe is
			// configured and the run of syncs started with the done report.
			doneAt := b.w.Now
			lease := &fleet.Lease{
				ID: "l1", Host: "a", State: fleet.LeaseVerifying, AgentDoneAt: &doneAt,
				SyncedSince: &doneAt, SyncedUntil: &doneAt, Weight: weight, Charged: weight,
				GrantedAt: doneAt, ExpiresAt: doneAt.Add(time.Hour),
			}
			b.w.Leases = []*fleet.Lease{lease}

			newest := uint64(100)

			for epoch, live := range tt.rows {
				b.live(epoch, "a", live)
				newest = max(newest, epoch)
			}

			// Another host always has a row, also in epochs "a" misses.
			for epoch := uint64(99); epoch <= newest; epoch++ {
				b.live(epoch, "other", 10)
			}

			b.advance(time.Duration(newest-99) * testEpoch)

			p := newPass(b.w)
			ready, note := p.ready(p.leases[0], p.hosts["a"])

			assert.Equal(t, tt.want, ready)

			if tt.note != "" {
				assert.Equal(t, tt.note, note)
			}

			out := b.step()
			assert.Equal(t, tt.want, b.lease("a") == nil, "the lease is released exactly when ready")
			assert.Equal(t, tt.want, len(out.Leases) == 1 && out.Leases[0].State == fleet.LeaseReleased)
		})
	}
}

func TestProbeReadiness(t *testing.T) {
	synced := protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Peers: 5}}
	syncing := protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Syncing: true, Peers: 5}}

	start := func(t *testing.T, weight uint64) *worldBuilder {
		t.Helper()

		b := newWorldBuilder(t)
		b.host("a", weight)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.finish("a")
		b.get("a").Status.Node = syncing

		return b
	}

	t.Run("host without validators", func(t *testing.T) {
		b := start(t, 0)

		b.step()
		assert.Equal(t, "waiting for a sync after the operation", b.lease("a").Readiness)

		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "node is syncing", b.lease("a").Readiness)
		assert.Nil(t, b.lease("a").SyncedSince)

		b.advance(5 * time.Second)
		b.get("a").Status.Node = synced
		since := b.w.Now
		b.step()
		require.NotNil(t, b.lease("a").SyncedSince)
		assert.Equal(t, since, *b.lease("a").SyncedSince)
		assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

		// A sync that is not synced starts the run over.
		b.advance(testEpoch / 2)
		b.get("a").Status.Node = syncing
		b.step()
		assert.Nil(t, b.lease("a").SyncedSince)

		b.advance(5 * time.Second)
		b.get("a").Status.Node = synced
		b.step()
		b.advance(testEpoch - time.Second)
		b.step()
		require.NotNil(t, b.lease("a"), "one second short of readyEpochs epoch durations")

		b.advance(time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
		assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)
	})

	t.Run("no probe configured: ready after the time", func(t *testing.T) {
		b := start(t, 0)
		b.get("a").Status.Node = protocol.NodeHealth{}

		b.step()
		b.advance(5 * time.Second)
		b.step()
		b.advance(testEpoch - time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))

		b.advance(time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
	})

	t.Run("validator host waits for liveness while it is unknown", func(t *testing.T) {
		b := start(t, 10)
		b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
		b.refreshHealth()
		b.get("a").Status.Node = synced

		b.step()
		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "waiting for attestation data from the beacon node", b.lease("a").Readiness)

		b.advance(testEpoch)

		out := b.step()
		require.NotNil(t, b.lease("a"), "the probes alone do not release a host with validators")
		assert.Equal(t, "waiting for attestation data from the beacon node", b.lease("a").Readiness)
		assert.False(t, out.Plan.Gate.OK)
	})

	t.Run("a host that went silent is not ready", func(t *testing.T) {
		b := start(t, 0)
		b.get("a").Status.Node = synced

		b.step()
		b.advance(5 * time.Second)
		b.step()
		b.advance(testEpoch, "a")
		b.step()

		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "agent silent", b.lease("a").Readiness)
	})
}

func TestReadinessTimeout(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MinParticipationPercent = 20
	b.host("a", 10)
	b.host("b", 30)
	b.wants("a", packagesNeed("2.0"))
	b.wants("b", packagesNeed("2.0"))
	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())

	// Done, but the validators never attest again.
	b.finish("a")
	b.liveOverride["a"] = 0
	b.step()
	b.advance(5 * time.Second)
	b.step()

	b.advance(30*time.Minute - 5*time.Second)
	b.step()
	require.NotNil(t, b.lease("a"), "exactly readinessTimeout is not yet over it")
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	b.advance(time.Second)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
	assert.Equal(t, "not ready after 30m0s: validators attesting: 0 of 1 epochs with at least 2 of 10",
		out.Leases[0].Error)

	r := b.mustRollout(packagesKey("2.0"))
	assert.Equal(t, fleet.RolloutHalted, r.State)
	assert.Equal(t, fleet.HostFailed, r.Host("a").State)
	assert.Empty(t, b.leasedHosts(), "the next host is never granted automatically after a failure")
	assert.Contains(t, queueReasons(out.Plan)["b"], "rollout halted: a: not ready after 30m0s")
}

// restartAfter models a controller outage: the clock moves on without any
// sync, then the scheduler starts.
func (b *worldBuilder) restartAfter(outage time.Duration) {
	b.w.Now = b.w.Now.Add(outage)
	b.w.Since = b.w.Now
	b.refreshHealth()
}

// silentStep moves the clock without any sync and reconciles.
func (b *worldBuilder) silentStep(d time.Duration) *Outcome {
	b.t.Helper()
	b.w.Now = b.w.Now.Add(d)
	b.refreshHealth()

	return b.step()
}

func TestControllerOutageDoesNotFailRunningLeases(t *testing.T) {
	ttl := fleet.DefaultSettings().LeaseTTL.Std()
	key := packagesKey("2.0")

	start := func(t *testing.T) *worldBuilder {
		t.Helper()

		b := newWorldBuilder(t)
		b.host("a", 0)
		b.host("b", 0)
		b.wants("a", packagesNeed("2.0"))
		b.wants("b", packagesNeed("2.0"))
		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())
		b.report("a", protocol.PhaseApplying, true)
		b.step()
		require.Equal(t, fleet.LeaseRunning, b.lease("a").State)

		return b
	}

	t.Run("the first reconcile after the outage fails nothing", func(t *testing.T) {
		b := start(t)
		b.restartAfter(3 * ttl)

		out := b.step()

		require.NotNil(t, b.lease("a"))
		assert.Equal(t, fleet.LeaseRunning, b.lease("a").State)
		assert.Equal(t, fleet.RolloutActive, b.mustRollout(key).State)
		assert.Empty(t, out.Audit)
		assert.Empty(t, out.Leases)
	})

	t.Run("an agent that reports again carries on", func(t *testing.T) {
		b := start(t)
		b.restartAfter(3 * ttl)
		b.step()

		b.advance(10 * time.Second)
		b.serve("a")

		assert.Equal(t, fleet.HostDone, b.mustRollout(key).Host("a").State)
		assert.Equal(t, []string{"b"}, b.leasedHosts())
	})

	t.Run("silence for leaseTtl since the start fails the lease", func(t *testing.T) {
		b := start(t)
		b.restartAfter(3 * ttl)
		b.step()

		b.silentStep(ttl)
		require.NotNil(t, b.lease("a"), "exactly leaseTtl since the start is not yet over it")

		out := b.silentStep(time.Second)
		require.Nil(t, b.lease("a"))
		assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
		assert.Equal(t, "agent silent", out.Leases[0].Error)
		assert.Equal(t, fleet.RolloutHalted, b.mustRollout(key).State)
	})

	t.Run("a sync after the start counts from the sync", func(t *testing.T) {
		b := start(t)
		b.restartAfter(3 * ttl)
		b.step()
		b.advance(ttl / 2)
		b.step()

		b.silentStep(ttl)
		require.NotNil(t, b.lease("a"))

		b.silentStep(time.Second)
		assert.Nil(t, b.lease("a"))
	})

	t.Run("without an outage silence counts from the last sync", func(t *testing.T) {
		b := start(t)
		b.w.Since = b.w.Now.Add(-time.Hour)

		b.silentStep(ttl + time.Second)
		assert.Nil(t, b.lease("a"))
		assert.Equal(t, fleet.RolloutHalted, b.mustRollout(key).State)
	})
}

func TestControllerOutageAndGrantedLeases(t *testing.T) {
	ttl := fleet.DefaultSettings().LeaseTTL.Std()
	key := packagesKey("2.0")

	start := func(t *testing.T) *worldBuilder {
		t.Helper()

		b := newWorldBuilder(t)
		b.w.Settings.PolicyReboot = fleet.PolicyManual
		b.host("a", 0)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		require.Equal(t, fleet.LeaseGranted, b.lease("a").State)

		// Long enough for the old rule: 2 * leaseTtl + grace of silence.
		b.restartAfter(4 * ttl)

		return b
	}

	t.Run("not failed as silent on the first reconcile", func(t *testing.T) {
		b := start(t)

		out := b.step()

		require.NotNil(t, b.lease("a"))
		assert.Equal(t, fleet.LeaseGranted, b.lease("a").State)
		assert.Empty(t, out.Audit)
	})

	t.Run("the agent says it did not start: expired, not a failure", func(t *testing.T) {
		b := start(t)
		b.step()
		b.w.Settings.PolicyPackages = fleet.PolicyManual
		b.advance(10 * time.Second)

		out := b.step()

		require.Nil(t, b.lease("a"))
		assert.Equal(t, []string{"lease.expired"}, auditActions(out))
		assert.Equal(t, fleet.RolloutActive, b.mustRollout(key).State)
		assert.Equal(t, fleet.HostPending, b.mustRollout(key).Host("a").State)
	})

	t.Run("the agent started while the controller was down: running", func(t *testing.T) {
		b := start(t)
		b.step()
		b.advance(10 * time.Second)
		b.report("a", protocol.PhaseApplying, true)
		b.step()

		assert.Equal(t, fleet.LeaseRunning, b.lease("a").State)
	})

	t.Run("still silent leaseTtl after the start: failed", func(t *testing.T) {
		b := start(t)
		b.step()

		b.silentStep(ttl - time.Second)
		require.NotNil(t, b.lease("a"))

		out := b.silentStep(time.Second)
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "agent silent", out.Leases[0].Error)
		assert.Equal(t, fleet.RolloutHalted, b.mustRollout(key).State)
	})
}

func TestControllerOutageDoesNotMakeHostsStale(t *testing.T) {
	stale := fleet.DefaultSettings().HostStaleAfter.Std()

	t.Run("a host is stale only after hostStaleAfter of silence since the start", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 0)
		b.restartAfter(time.Hour)

		assert.Equal(t, SlotIdle, b.step().Plan.Hosts["a"].Slot)
		assert.Equal(t, SlotIdle, b.silentStep(stale).Plan.Hosts["a"].Slot)
		assert.Equal(t, SlotStale, b.silentStep(time.Second).Plan.Hosts["a"].Slot)

		b.advance(time.Second)
		assert.Equal(t, SlotIdle, b.step().Plan.Hosts["a"].Slot)
	})

	t.Run("an expected host keeps being waited for", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("tool", 0)
		b.host("val", 10)
		b.wantsImage("tool", imgLighthouse, digestA)
		b.get("val").Status.Containers = []protocol.Container{
			container("c", imgLighthouse, digestOld, digestOld, b.w.Now),
		}

		out := b.step()
		require.Equal(t, "after canary val", queueReasons(out.Plan)["tool"])

		// Neither the silence of "val" nor the age of the rollout is news:
		// nobody could report to a controller that was down.
		b.restartAfter(time.Hour)

		out = b.step()
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "after canary val", queueReasons(out.Plan)["tool"])

		// The hosts sync again; "val" has hostStaleAfter from the start.
		b.advance(stale)

		out = b.step()
		assert.Equal(t, "after canary val", queueReasons(out.Plan)["tool"])

		b.advance(time.Second)
		b.step()
		assert.Equal(t, []string{"tool"}, b.leasedHosts())
	})
}

// A controller outage does not fail a verifying lease: the readiness timeout
// runs from the scheduler's start at the earliest. A host with validators
// cannot be ready while liveness is unknown, and nobody looked while the
// controller was down.
func TestOutageDoesNotFailAVerifyingLease(t *testing.T) {
	b := verifying(t, 5, 10)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	// The controller is down for 31 minutes. It starts again; nobody has
	// synced yet and the beacon service has no liveness yet.
	b.w.Now = b.w.Now.Add(31 * time.Minute)
	b.w.Since = b.w.Now
	b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
	b.refreshHealth()

	out := b.step()
	require.NotNil(t, b.lease("a"), "the outage alone fails no lease")
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)
	assert.NotContains(t, auditActions(out), "lease.failed")
	assert.Equal(t, fleet.RolloutActive, b.mustRollout(packagesKey("2.0")).State)

	// Everything comes back: the host is released as usual.
	b.healthMod = nil
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)
	b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)

	// A host that does not come back still fails, readinessTimeout after the
	// start.
	t.Run("the timeout runs from the start", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.liveOverride["a"] = 0
		b.w.Now = b.w.Now.Add(31 * time.Minute)
		b.w.Since = b.w.Now
		b.refreshHealth()
		b.step()
		require.NotNil(t, b.lease("a"))

		b.advance(b.w.Settings.ReadinessTimeout.Std())
		b.step()
		require.NotNil(t, b.lease("a"), "not before the timeout has passed since the start")

		b.advance(time.Second)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Contains(t, auditActions(out), "lease.failed")
	})
}

// A member whose step was done in a lease that failed on another step is no
// success of its rollout.
func TestStepDoneInAFailedLeaseIsNoSuccess(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	for _, name := range []string{"a", "b", "c"} {
		b.host(name, 10)
		b.wantsImage(name, imgLighthouse, digestA)
	}

	b.wants("a", rebootNeed())
	b.step()

	key := imageKey(imgLighthouse, digestA)
	require.Equal(t, []string{"a"}, b.leasedHosts())
	require.Equal(t, "a", b.mustRollout(key).Canary)
	require.Len(t, b.lease("a").Needs, 2)

	// The image step is done, the reboot fails.
	statuses := make([]protocol.StepStatus, 0, 2)

	for _, n := range b.lease("a").Needs {
		if n.Kind == protocol.NeedImage {
			statuses = append(statuses, protocol.StepDone)
		} else {
			statuses = append(statuses, protocol.StepFailed)
		}
	}

	b.report("a", protocol.PhaseFailed, true, statuses...)
	b.get("a").Status.Needs = []protocol.Need{rebootNeed()}
	b.get("a").Status.Containers = []protocol.Container{
		container("c-"+imgLighthouse, imgLighthouse, digestA, digestA, b.w.Now),
	}

	out := b.step()
	r := b.mustRollout(key)

	require.Equal(t, fleet.RolloutHalted, b.mustRollout(keyReboot).State)
	require.Equal(t, fleet.RolloutActive, r.State, "the image step was done: the image rollout is not halted")
	assert.Equal(t, fleet.HostDone, r.Host("a").State)
	assert.Empty(t, r.Host("a").LeaseID, "done, but not through a lease that was released")

	// No first success: one new canary goes alone.
	assert.Equal(t, []string{"b"}, b.leasedHosts())
	assert.Equal(t, "b", r.Canary)
	assert.Equal(t, map[string]string{"c": "after canary b"}, queueReasons(out.Plan))

	b.serve("b")
	assert.Equal(t, []string{"c"}, b.leasedHosts())
}

// The run of healthy syncs must span the window.
func TestHealthyRunMustSpanTheWindow(t *testing.T) {
	b := verifying(t, 5, 0)
	// An epoch is shorter than hostStaleAfter: the silence is not staleness.
	b.w.Settings.HostStaleAfter = fleet.Duration(3 * testEpoch)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a").SyncedSince)

	// No sync from the host for an epoch and more.
	b.advance(testEpoch+time.Minute, "a")
	b.step()
	require.NotNil(t, b.lease("a"), "an old sync says nothing about now")
	assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

	// It syncs again, still healthy: the run now spans the window.
	b.advance(5 * time.Second)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
}

// The attestation bar is the smallest count that is at least
// minParticipationPercent of the host's validators.
func TestAttestationBarRoundsUp(t *testing.T) {
	for _, tt := range []struct {
		percent float64
		weight  uint64
		want    uint64
	}{
		{90, 1, 1}, {90, 2, 2}, {90, 3, 3}, {90, 9, 9}, {90, 10, 9}, {90, 11, 10},
		{90, 128, 116}, {90, 100, 90}, {66, 3, 2}, {50, 3, 2}, {50, 4, 2},
		{100, 5, 5}, {0, 5, 1}, {33.3, 3, 1}, {80, 5, 4},
	} {
		assert.Equal(t, tt.want, attestationBar(tt.percent, tt.weight), "%v%% of %d", tt.percent, tt.weight)
	}

	// One of two validators attesting is 50%, not 90%.
	b := verifying(t, 5, 2)
	b.liveOverride["a"] = 1
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	b.advance(3 * testEpoch)
	b.step()

	require.NotNil(t, b.lease("a"))
	assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 2 of 2", b.lease("a").Readiness)

	delete(b.liveOverride, "a")
	b.advance(testEpoch)
	b.step()
	assert.Nil(t, b.lease("a"))
}

// forked gives the builder's chain a fork at an epoch that changes the slot
// duration, and keeps the beacon view's epochs in line with the clock.
func forked(b *worldBuilder, at uint64, slot time.Duration) {
	base := testSecondsPerSlot * time.Second
	phases := []fleet.SlotPhase{
		{Epoch: 0, Start: testGenesis, SlotMs: uint64(base / time.Millisecond)},
		{Epoch: at, Start: testGenesis.Add(time.Duration(at) * testEpoch), SlotMs: uint64(slot / time.Millisecond)},
	}

	b.healthMod = func(n *fleet.NetworkHealth) {
		n.Phases = phases
		n.SecondsPerSlot = uint64(slot / time.Second)

		current, _ := n.EpochAt(b.w.Now)
		n.CurrentEpoch = current
		n.FinalizedEpoch = current - 2
		n.LivenessEpoch = current - 1
	}
	b.refreshHealth()
}

// Epochs are placed in time by the chain's slot phases, not by genesis plus
// the current slot duration: across a fork that changes the slot duration a
// lease is released on the epochs that began after the operation.
func TestReadinessAcrossASlotDurationFork(t *testing.T) {
	t.Run("the slot duration halves", func(t *testing.T) {
		// The fork is at epoch 99; the clock is 394 s past it, in epoch 101
		// of 192 s epochs.
		b := newWorldBuilder(t)
		forked(b, 99, 6*time.Second)
		require.Equal(t, uint64(101), b.w.Health.CurrentEpoch)
		require.Equal(t, 192*time.Second, b.w.Health.EpochDuration())

		b.host("a", 10).Status.Node = nodeWith(8, 8)
		b.host("idle", 90)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness,
			"the epoch of the operation proves nothing")

		// Epoch 101 ends: it began before the operation was done.
		b.advance(192 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

		// Epoch 102 began after it and is complete: ready.
		b.advance(192 * time.Second)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))
	})

	t.Run("the slot duration doubles", func(t *testing.T) {
		// The fork is at epoch 99; the clock is 394 s past it, still in epoch
		// 99 of 768 s epochs.
		b := newWorldBuilder(t)
		forked(b, 99, 24*time.Second)
		require.Equal(t, uint64(99), b.w.Health.CurrentEpoch)

		b.host("a", 10).Status.Node = nodeWith(8, 8)
		b.host("idle", 90)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()

		// Epoch 99 ends, full of attestations, but it began before the
		// operation was done.
		b.advance(768 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"), "an epoch from before the operation proves nothing")
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

		b.advance(768 * time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
	})

	t.Run("a slot that is not a whole number of seconds", func(t *testing.T) {
		// 4.5 s slots from genesis on: 144 s epochs.
		b := newWorldBuilder(t)
		b.healthMod = func(n *fleet.NetworkHealth) {
			n.Phases = fleet.SinglePhase(testGenesis, 4500*time.Millisecond)
			n.SecondsPerSlot = 4

			current, _ := n.EpochAt(b.w.Now)
			n.CurrentEpoch = current
			n.FinalizedEpoch = current - 2
			n.LivenessEpoch = current - 1
		}
		b.refreshHealth()
		require.Equal(t, 144*time.Second, b.w.Health.EpochDuration())

		b.host("a", 10).Status.Node = nodeWith(8, 8)
		b.host("idle", 90)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

		b.advance(2 * 144 * time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
	})

	t.Run("an epoch that cannot be placed fails safe", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.get("a").Status.Node = nodeWith(8, 8)
		b.healthMod = func(n *fleet.NetworkHealth) { n.Phases = nil }
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)

		out := b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "epoch timing unknown", b.lease("a").Readiness)
		assert.False(t, out.Plan.Gate.OK)
	})
}

// A run of healthy syncs with a gap of more than hostStaleAfter is two runs.
func TestSilenceEndsTheHealthyRun(t *testing.T) {
	b := verifying(t, 5, 0)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a").SyncedSince)

	// Ten minutes of silence: the agent is stale, the run is over.
	b.advance(10*time.Minute, "a")
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, "agent silent", b.lease("a").Readiness)
	assert.Nil(t, b.lease("a").SyncedSince)

	// One more healthy sync starts a new run; it does not release.
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, b.w.Now, *b.lease("a").SyncedSince)
	assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

	b.advance(testEpoch)
	b.step()
	assert.Nil(t, b.lease("a"))

	// The same when no reconcile saw the silence: the gap between two syncs
	// of the run is what counts.
	t.Run("a gap nobody watched", func(t *testing.T) {
		b := verifying(t, 5, 0)
		b.get("a").Status.Node = nodeWith(8, 8)
		b.advance(5 * time.Second)
		b.step()

		first := *b.lease("a").SyncedSince
		b.advance(10*time.Minute+5*time.Second, "a")
		b.get("a").LastSeen = b.w.Now
		b.step()

		require.NotNil(t, b.lease("a"))
		assert.NotEqual(t, first, *b.lease("a").SyncedSince)
		assert.Equal(t, b.w.Now, *b.lease("a").SyncedSince)
	})

	// A controller restart ends the run as well: the first sync after it does
	// not release.
	t.Run("a controller restart", func(t *testing.T) {
		b := verifying(t, 5, 0)
		b.get("a").Status.Node = nodeWith(8, 8)
		b.advance(5 * time.Second)
		b.step()

		b.advance(testEpoch+time.Minute, "a", "other1", "other2", "other3", "other4")
		b.w.Since = b.w.Now

		b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "waiting for a sync after the controller's start", b.lease("a").Readiness)

		b.advance(5 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"), "the run begins anew after the start")
		assert.Equal(t, b.w.Now, *b.lease("a").SyncedSince)

		b.advance(testEpoch)
		b.step()
		assert.Nil(t, b.lease("a"))
	})
}

func TestReleasedMemberThatIsNotDoneCarriesNoLeaseID(t *testing.T) {
	t.Run("a released member that is not done carries no lease ID", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 0)
		b.wants("a", packagesNeed("2.0"), rebootNeed())
		b.step()
		require.Len(t, b.lease("a").Needs, 2)

		// The reboot is done, the packages step was superseded: still needed.
		statuses := make([]protocol.StepStatus, 0, 2)

		for _, n := range b.lease("a").Needs {
			if n.Kind == protocol.NeedReboot {
				statuses = append(statuses, protocol.StepDone)
			} else {
				statuses = append(statuses, protocol.StepSuperseded)
			}
		}

		b.report("a", protocol.PhaseDone, true, statuses...)
		b.get("a").Status.Needs = []protocol.Need{packagesNeed("2.0")}
		b.w.Settings.Paused = true
		b.step()
		b.advance(5 * time.Second)
		b.step()
		b.advance(2 * testEpoch)

		out := b.step()
		require.Contains(t, auditActions(out), "lease.released")

		m := b.mustRollout(packagesKey("2.0")).Host("a")
		assert.Equal(t, fleet.HostPending, m.State)
		assert.Empty(t, m.LeaseID)
		assert.Equal(t, fleet.RolloutCompleted, b.closed(keyReboot).State)
	})
}

// beforeGenesis moves the builder's clock to d before genesis, with a beacon
// view as a node gives it then: epoch 0, nothing finalized, no liveness.
func beforeGenesis(b *worldBuilder, d time.Duration) {
	b.autoLive = false
	b.healthMod = func(n *fleet.NetworkHealth) {
		current, _ := n.EpochAt(b.w.Now)
		n.CurrentEpoch = current
		n.FinalizedEpoch = current - min(current, 2)
		n.FinalityLagEpochs = min(current, 2)
		n.LivenessKnown = false
		n.LivenessEpoch = 0
	}
	b.w.Now = testGenesis.Add(-d)
	b.refreshHealth()
}

// A run of healthy syncs that began before genesis is placed from genesis on.
func TestHealthyRunThatBeganBeforeGenesis(t *testing.T) {
	b := newWorldBuilder(t)
	beforeGenesis(b, 10*time.Minute)

	b.host("a", 0).Status.Node = nodeWith(8, 8)
	b.host("b", 0).Status.Node = nodeWith(8, 8)
	b.wants("a", packagesNeed("2.0"))

	out := b.step()
	require.True(t, out.Plan.Gate.OK, "gate: %v", out.Plan.Gate.Reasons)
	require.Equal(t, []string{"a"}, b.leasedHosts())

	// The operation is done three minutes before genesis.
	b.advance(7 * time.Minute)
	b.finish("a")
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	// A healthy sync every minute. The window is the chain's first epoch: at
	// six minutes past genesis the run is nine minutes old and still short of
	// it.
	for b.w.Now.Before(testGenesis.Add(6 * time.Minute)) {
		b.advance(time.Minute)
		b.step()

		l := b.lease("a")
		require.NotNil(t, l, "released at %s past genesis", b.w.Now.Sub(testGenesis))
		require.Equal(t, fleet.LeaseVerifying, l.State)
		assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", l.Readiness,
			"at %s past genesis", b.w.Now.Sub(testGenesis))
	}

	since := *b.lease("a").SyncedSince
	assert.Equal(t, testGenesis.Add(-2*time.Minute), since, "the run itself began before genesis")

	// Seven minutes past genesis the first epoch is over.
	b.advance(time.Minute)

	out = b.step()
	require.Nil(t, b.lease("a"))
	assert.Contains(t, auditActions(out), "lease.released")
	assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)

	// A run that begins after genesis is measured as before.
	t.Run("a run from after genesis", func(t *testing.T) {
		b := newWorldBuilder(t)
		beforeGenesis(b, time.Minute)

		b.host("a", 0).Status.Node = nodeWith(8, 8)
		b.host("b", 0).Status.Node = nodeWith(8, 8)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.advance(3 * time.Minute)
		b.finish("a")
		b.step()
		b.advance(time.Minute)
		b.step()
		require.Equal(t, testGenesis.Add(3*time.Minute), *b.lease("a").SyncedSince)

		b.advance(testEpoch - time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))

		b.advance(time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
	})
}

// comparing builds a fleet in which "a" has just reported its operation
// done: a and one reference host per node of refs ("ref1", ...), all without
// validators. readyEpochs is 0, so a is ready at its first healthy sync and
// the comparison with the fleet is all that can keep its lease; prepare runs
// before the first reconcile and may change that.
func comparing(t *testing.T, share float64, prepare func(b *worldBuilder), refs ...protocol.NodeHealth) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)
	b.w.Settings.ReadyPeerShare = share
	b.w.Settings.ReadyEpochs = 0
	b.w.Settings.MinPeers = 1
	b.w.Settings.MaxUnavailablePercent = 100

	b.host("a", 0)
	b.wants("a", packagesNeed("2.0"))

	for i, node := range refs {
		b.host(fmt.Sprintf("ref%d", i+1), 0).Status.Node = node
	}

	if prepare != nil {
		prepare(b)
	}

	b.step()
	require.Contains(t, b.leasedHosts(), "a")
	b.finish("a")
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	return b
}

// seen lets "a" sync with the node, five seconds on, and reconciles. The
// hosts in silent do not sync.
func (b *worldBuilder) seen(node protocol.NodeHealth, silent ...string) *fleet.Lease {
	b.t.Helper()

	b.get("a").Status.Node = node
	b.advance(5*time.Second, silent...)
	b.step()

	return b.lease("a")
}

// A host that is ready otherwise and whose client has fewer peers than the
// share of the fleet's median keeps its lease, with the reason; at the share
// it is released. Catches: ignore the share.
func TestPeerShareBelowWaitsAtOrAboveReleases(t *testing.T) {
	b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

	for _, tt := range []struct {
		node protocol.NodeHealth
		note string
	}{
		{nodeWith(31, 4), "CL peers 4, the fleet's median is 31 (needs 16)"},
		{nodeWith(15, 31), "EL peers 15, the fleet's median is 31 (needs 16)"},
		{nodeWith(4, 4), "EL peers 4, the fleet's median is 31 (needs 16)"},
	} {
		l := b.seen(tt.node)
		require.NotNil(t, l)
		assert.Equal(t, fleet.LeaseVerifying, l.State)
		assert.Equal(t, tt.note, l.Readiness)
		assert.NotNil(t, l.SyncedSince, "the run of healthy syncs stays")
	}

	assert.Equal(t, []fleet.PeerCheck{
		{Layer: "EL", Peers: 4, Median: 31, Needs: 16},
		{Layer: "CL", Peers: 4, Median: 31, Needs: 16},
	}, b.lease("a").PeerChecks, "both clients, for whoever looks")

	// Exactly the share is enough.
	b.get("a").Status.Node = nodeWith(16, 16)
	b.advance(5 * time.Second)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, noteReadyNode, releasedReadiness(t, out))
	assert.Equal(t, []fleet.PeerCheck{
		{Layer: "EL", Peers: 16, Median: 31, Needs: 16},
		{Layer: "CL", Peers: 16, Median: 31, Needs: 16},
	}, b.lastLease("a").PeerChecks, "what it was released on")
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)
}

// A host that stays below the fleet fails readiness like any host that does
// not come back, and halts its rollout with the comparison as the reason.
func TestPeerShareBelowFailsAfterTheReadinessTimeout(t *testing.T) {
	b := comparing(t, 0.5, func(b *worldBuilder) {
		b.host("z", 0)
		b.wants("z", packagesNeed("2.0"))
	}, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

	for range 31 {
		b.advance(55 * time.Second)
		b.seen(nodeWith(31, 4))
	}

	require.Nil(t, b.lease("a"))

	failed := b.lastLease("a")
	assert.Equal(t, fleet.LeaseFailed, failed.State)
	assert.Equal(t, "not ready after 30m0s: CL peers 4, the fleet's median is 31 (needs 16)", failed.Error)

	r := b.mustRollout(packagesKey("2.0"))
	assert.Equal(t, fleet.RolloutHalted, r.State)
	assert.Equal(t, "a: "+failed.Error, r.Reason)
}

// Fewer than three reference hosts for a client: that client is not compared
// (the fixed number of peers still is). Catches: apply with fewer than three
// references.
func TestPeerShareNeedsThreeReferences(t *testing.T) {
	b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31))

	require.Nil(t, b.seen(nodeWith(4, 4)), "two references are no fleet")
	assert.Empty(t, b.lastLease("a").PeerChecks)

	// Three hosts probe the CL, two of them the EL as well.
	clOnly := protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Peers: 31}}

	t.Run("per client", func(t *testing.T) {
		b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), clOnly)

		require.Nil(t, b.seen(nodeWith(4, 31)), "two EL references: the EL is not compared")
		assert.Equal(t, []fleet.PeerCheck{{Layer: "CL", Peers: 31, Median: 31, Needs: 16}}, b.lastLease("a").PeerChecks)

		b = comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), clOnly)

		l := b.seen(nodeWith(4, 15))
		require.NotNil(t, l)
		assert.Equal(t, "CL peers 15, the fleet's median is 31 (needs 16)", l.Readiness)
	})

	t.Run("a client the host does not probe is not compared", func(t *testing.T) {
		b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

		require.Nil(t, b.seen(protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Peers: 16}}))
		assert.Equal(t, []fleet.PeerCheck{{Layer: "CL", Peers: 16, Median: 31, Needs: 16}}, b.lastLease("a").PeerChecks)

		b = comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

		require.Nil(t, b.seen(protocol.NodeHealth{}), "no probe, nothing to compare")
		assert.Empty(t, b.lastLease("a").PeerChecks)
		assert.Equal(t, "ready: no node probe configured", b.lastLease("a").Readiness)
	})
}

// held gives the host an open lease that its agent works on.
func (b *worldBuilder) held(host string) {
	id := "held-" + host
	b.w.Leases = append(b.w.Leases, &fleet.Lease{
		ID: id, Host: host, State: fleet.LeaseRunning, Rollouts: map[string]string{}, GrantedBy: actorScheduler,
		GrantedAt: b.w.Now, ExpiresAt: b.w.Now.Add(time.Hour),
	})

	st := *b.get(host).Status
	st.Operation = &protocol.Operation{LeaseID: id, Phase: protocol.PhaseApplying, Disrupted: true}
	b.get(host).Status = &st
}

// The median is that of the reference hosts: hosts that hold a lease, are
// broken (for whatever group, not only the rollout's), are stale or whose
// node is not at head with the required peers are not among them, whatever
// they report. Catches: compare against all hosts including the leased ones;
// broken for the rollout's group only; references below the required peers.
func TestPeerShareMedianIsOfTheReferenceHosts(t *testing.T) {
	loud := []string{"x1", "x2", "x3", "x4"}

	for name, exclude := range map[string]func(b *worldBuilder){
		"hosts that hold a lease": func(b *worldBuilder) {
			for _, host := range loud {
				b.held(host)
			}
		},
		// The rollout is about the packages; these hosts are broken for an image.
		"hosts that are broken for another group": func(b *worldBuilder) {
			for _, host := range loud {
				b.broke(host, imageNeed("c", imgGeth, digestA))
			}
		},
		"hosts that are broken for the rollout's group": func(b *worldBuilder) {
			for _, host := range loud {
				b.broke(host, packagesNeed("1.5"))
			}
		},
		"hosts whose node is not at head": func(b *worldBuilder) {
			b.get("x1").Status.Node.EL.Syncing = true
			b.get("x2").Status.Node.CL.Syncing = true
			b.get("x3").Status.Node.CL.ELOffline = true
			b.get("x4").Status.Node.EL.Reachable = false
		},
		"hosts that are silent": func(b *worldBuilder) {
			b.advance(3*time.Minute, loud...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := comparing(t, 0.5, func(b *worldBuilder) {
				for _, host := range loud {
					b.host(host, 0).Status.Node = nodeWith(100, 100)
				}

				exclude(b)
			}, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

			// With the four loud hosts the median of seven would be 100.
			require.Nil(t, b.seen(nodeWith(5, 5), loud...))
			assert.Equal(t, []fleet.PeerCheck{
				{Layer: "EL", Peers: 5, Median: 10, Needs: 5},
				{Layer: "CL", Peers: 5, Median: 10, Needs: 5},
			}, b.lastLease("a").PeerChecks)
		})
	}

	// The same four hosts as references: they count.
	b := comparing(t, 0.5, func(b *worldBuilder) {
		for _, host := range loud {
			b.host(host, 0).Status.Node = nodeWith(100, 100)
		}
	}, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

	l := b.seen(nodeWith(49, 60))
	require.NotNil(t, l)
	assert.Equal(t, "EL peers 49, the fleet's median is 100 (needs 50)", l.Readiness)

	t.Run("hosts with fewer than the required peers", func(t *testing.T) {
		// Seven nodes ask for 3 peers; four hosts have 2. With them the median
		// of seven would be 2, and a host with 3 would pass.
		b := comparing(t, 0.5, func(b *worldBuilder) {
			b.w.Settings.MinPeers = 3

			for _, host := range []string{"q1", "q2", "q3", "q4"} {
				b.host(host, 0).Status.Node = nodeWith(2, 2)
			}
		}, nodeWith(10, 10), nodeWith(10, 10))

		require.Nil(t, b.seen(nodeWith(3, 3)), "two references: not compared")

		b = comparing(t, 0.5, func(b *worldBuilder) {
			b.w.Settings.MinPeers = 3

			for _, host := range []string{"q1", "q2", "q3", "q4"} {
				b.host(host, 0).Status.Node = nodeWith(2, 2)
			}
		}, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

		l := b.seen(nodeWith(3, 3))
		require.NotNil(t, l)
		assert.Equal(t, "EL peers 3, the fleet's median is 10 (needs 5)", l.Readiness)
	})

	t.Run("of an even number, the lower middle value", func(t *testing.T) {
		b := comparing(t, 0.5, nil, nodeWith(10, 10), nodeWith(20, 20), nodeWith(30, 30), nodeWith(40, 40))

		l := b.seen(nodeWith(10, 9))
		require.NotNil(t, l)
		assert.Equal(t, "CL peers 9, the fleet's median is 20 (needs 10)", l.Readiness)
	})

	t.Run("the host itself is no reference", func(t *testing.T) {
		// Three others at 10; the host reports 100 and is not counted in.
		b := comparing(t, 1, nil, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

		require.Nil(t, b.seen(nodeWith(100, 100)))
		assert.Equal(t, []fleet.PeerCheck{
			{Layer: "EL", Peers: 100, Median: 10, Needs: 10},
			{Layer: "CL", Peers: 100, Median: 10, Needs: 10},
		}, b.lastLease("a").PeerChecks)
	})
}

// nodeWith is a node at head whose clients have the given numbers of peers.
func nodeWith(elPeers, clPeers int) protocol.NodeHealth {
	return protocol.NodeHealth{
		EL: &protocol.ELHealth{Reachable: true, Peers: elPeers},
		CL: &protocol.CLHealth{Reachable: true, Peers: clPeers},
	}
}

// verifying builds a fleet of hosts hosts, each running a node, in which "a"
// (with weight validators) has just reported its operation done.
func verifying(t *testing.T, hosts int, weight uint64) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)
	b.host("a", weight)
	b.wants("a", packagesNeed("2.0"))

	for i := 1; i < hosts; i++ {
		b.host("other"+string(rune('0'+i)), 0).Status.Node = nodeWith(9, 9)
	}

	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())
	b.finish("a")
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	return b
}

func TestReadinessNeedsPeers(t *testing.T) {
	// Seven nodes: minPeers 3 is what the node must have.
	b := verifying(t, 7, 0)

	for _, tt := range []struct {
		node protocol.NodeHealth
		note string
	}{
		{nodeWith(0, 9), "waiting for peers: EL has 0 of 3"},
		{nodeWith(9, 1), "waiting for peers: CL has 1 of 3"},
		{nodeWith(2, 2), "waiting for peers: EL has 2 of 3"},
	} {
		b.get("a").Status.Node = tt.node
		b.advance(5 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, tt.note, b.lease("a").Readiness)
		assert.Nil(t, b.lease("a").SyncedSince)
	}

	b.get("a").Status.Node = nodeWith(3, 3)
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a").SyncedSince)
	assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

	// Losing a peer starts the time over.
	b.get("a").Status.Node = nodeWith(3, 2)
	b.advance(testEpoch)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Nil(t, b.lease("a").SyncedSince)

	b.get("a").Status.Node = nodeWith(3, 3)
	b.advance(5 * time.Second)
	b.step()
	b.advance(testEpoch)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
}

func TestReadinessMinPeersOnSmallFleets(t *testing.T) {
	for _, tt := range []struct {
		name     string
		hosts    int
		tooling  int // hosts without a node, on top of hosts
		minPeers int
		peers    int
		note     string // empty: ready
	}{
		{name: "a single node still needs a peer", hosts: 1, minPeers: 3, peers: 0, note: "waiting for peers: EL has 0 of 1"},
		{name: "two nodes", hosts: 2, minPeers: 3, peers: 1},
		{name: "three nodes need one peer", hosts: 3, minPeers: 3, peers: 1},
		{name: "three nodes, no peer", hosts: 3, minPeers: 3, peers: 0, note: "waiting for peers: EL has 0 of 1"},
		{name: "four nodes need two", hosts: 4, minPeers: 3, peers: 2},
		{name: "four nodes, one peer", hosts: 4, minPeers: 3, peers: 1, note: "waiting for peers: EL has 1 of 2"},
		{name: "five nodes need two", hosts: 5, minPeers: 3, peers: 2},
		{name: "six nodes need three", hosts: 6, minPeers: 3, peers: 2, note: "waiting for peers: EL has 2 of 3"},
		{name: "seven nodes need three", hosts: 7, minPeers: 3, peers: 3},
		{name: "a large fleet asks for minPeers, not more", hosts: 9, minPeers: 3, peers: 3},
		{name: "minPeers 1", hosts: 9, minPeers: 1, peers: 1},
		{name: "minPeers 5 on nine nodes is half of the others", hosts: 9, minPeers: 5, peers: 4},
		{name: "minPeers 5, three peers", hosts: 9, minPeers: 5, peers: 3, note: "waiting for peers: EL has 3 of 4"},
		// A host without a node is nobody's peer.
		{name: "hosts without a node do not count", hosts: 3, tooling: 4, minPeers: 3, peers: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := verifying(t, tt.hosts, 0)

			for i := range tt.tooling {
				b.host("tooling"+string(rune('0'+i)), 0)
			}

			// A host that has never synced runs no node the controller knows of.
			b.host("unseen", 0).Status = nil

			b.w.Settings.MinPeers = tt.minPeers
			b.w.Settings.ReadyEpochs = 0
			b.get("a").Status.Node = nodeWith(tt.peers, tt.peers)
			b.advance(5 * time.Second)
			b.step()

			if tt.note == "" {
				assert.Nil(t, b.lease("a"), "released")

				return
			}

			require.NotNil(t, b.lease("a"))
			assert.Equal(t, tt.note, b.lease("a").Readiness)
		})
	}
}

// TestReadinessOfAValidatorHostNeedsBoth: the node at head with peers in its
// newest sync and the validators attesting for readyEpochs; attesting
// validators make a run of healthy syncs unnecessary.
func TestReadinessOfAValidatorHostNeedsBoth(t *testing.T) {
	healthy := nodeWith(8, 8)
	syncing := nodeWith(8, 8)
	syncing.CL.Syncing = true

	t.Run("validators attest but the node is syncing", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.get("a").Status.Node = syncing
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)
		b.step()

		require.NotNil(t, b.lease("a"), "full attestation alone does not release")
		assert.Equal(t, "node is syncing", b.lease("a").Readiness)

		// The node catches up: its validators attest already, so it is released
		// with the first healthy sync.
		b.get("a").Status.Node = healthy
		b.advance(5 * time.Second)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))
	})

	t.Run("the node is at head with peers but the validators do not attest", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.liveOverride["a"] = 0
		b.get("a").Status.Node = healthy
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)
		b.step()

		require.NotNil(t, b.lease("a"), "healthy probes alone do not release")
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

		// They attest in the next epoch.
		delete(b.liveOverride, "a")
		b.advance(testEpoch)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))
	})

	t.Run("too few peers although the validators attest", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.get("a").Status.Node = nodeWith(8, 1)
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)
		b.step()

		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "waiting for peers: CL has 1 of 2", b.lease("a").Readiness)
	})
}

// releasedReadiness returns the readiness note of the lease.released audit
// entry of an outcome.
func releasedReadiness(t *testing.T, out *Outcome) string {
	t.Helper()

	for _, e := range out.Audit {
		if e.Action == "lease.released" {
			note, ok := e.Detail["readiness"].(string)
			require.True(t, ok)

			return note
		}
	}

	t.Fatal("no lease.released audit entry")

	return ""
}

// A host with validators is not released on its probes alone while the beacon
// service has not settled an epoch yet. It publishes the epoch timing before
// the first epoch settles, so every controller restart passes through this
// state, with the run of healthy syncs already stored on the lease.
func TestValidatorHostIsNotReadyWhileLivenessIsUnknown(t *testing.T) {
	b := verifying(t, 5, 10)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	// The controller restarts: timing is known, liveness is not yet.
	b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
	b.refreshHealth()
	b.liveOverride["a"] = 0

	out := b.step()
	require.NotNil(t, b.lease("a"), "not released without the attestation check")
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)
	assert.Equal(t, "waiting for attestation data from the beacon node", b.lease("a").Readiness)
	assert.NotNil(t, b.lease("a").SyncedSince, "the run of healthy syncs is kept")
	assert.NotContains(t, auditActions(out), "lease.released")

	// Liveness arrives and says the validators do not attest.
	b.healthMod = nil
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

	// They attest for a complete epoch: released.
	delete(b.liveOverride, "a")
	b.advance(testEpoch)

	out = b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))

	// A host without validators does not wait for the beacon node.
	t.Run("host without validators", func(t *testing.T) {
		b := verifying(t, 5, 0)
		b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
		b.refreshHealth()
		b.get("a").Status.Node = nodeWith(8, 8)
		b.advance(5 * time.Second)
		b.step()
		b.advance(testEpoch)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
	})
}

// Readiness notes of a host that probes its node.
const (
	noteNodeWaiting = "node at head with peers, waiting for it to stay so for 6m24s"
	noteReadyNode   = "ready: node at head with peers"
)
