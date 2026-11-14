package scheduler

import (
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
	assert.Equal(t, "0 of 1 epochs with at least 9 of 10 validators attesting", l.Readiness)

	// The epoch the operation ended in does not count.
	b.advance(testEpoch)
	b.step()
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

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
			name:      "unused at the expiry: expired, the host waits again and is served again",
			run:       func(b *worldBuilder) { b.advance(ttl) },
			want:      fleet.LeaseExpired,
			regranted: true,
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
			name:    "silent for another leaseTtl after the expiry: failed",
			run:     func(b *worldBuilder) { b.advance(2*ttl, "a") },
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
	b.w.Settings.PolicyReboot = fleet.PolicyPaused
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
	r = b.mustRollout(key)
	r.State = fleet.RolloutActive
	r.Host("a").State = fleet.HostPending
	b.serve("b")
	b.step()

	// "a" is back and sorts first.
	require.Equal(t, []string{"a"}, b.leasedHosts())
	assert.Equal(t, protocol.NeedPackages, b.lease("a").Needs[0].Kind)
}

func TestRunningLeaseWithoutTheOperationFails(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.wants("a", packagesNeed("2.0"))
	b.step()
	b.report("a", protocol.PhaseApplying, true)
	b.step()

	b.advance(5 * time.Second)
	b.get("a").Status.Operation = nil

	out := b.step()
	assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
	assert.Equal(t, reasonLostOperation, out.Leases[0].Error)
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
				require.True(t, b.w.ClosedLeases[l.ID])
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
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.wantsImage("a", imgLighthouse, digestA)
	b.step()

	// The agent says done but keeps reporting the same need.
	b.report("a", protocol.PhaseDone, true)
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	out := b.step()

	require.Len(t, out.Leases, 1)
	assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
	assert.Contains(t, out.Leases[0].Error, "is still reported after the operation")
	assert.Equal(t, fleet.RolloutHalted, b.mustRollout(imageKey(imgLighthouse, digestA)).State)
	assert.Empty(t, b.leasedHosts(), "no loop of grants")
}

func TestForceRollout(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.host("boot", 5, fleet.RoleBootnode)
	b.host("val", 10)
	b.host("tool", 0)
	b.manualRollout("m1", true, false, "boot", "val", "tool")

	// No stages, no canary; one host per reconcile.
	out := b.step()
	assert.Equal(t, []string{"tool"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"val": "next", "boot": "next"}, queueReasons(out.Plan))
	assert.Empty(t, b.mustRollout("manual:m1").Canary)

	l := b.lease("tool")
	assert.True(t, l.Force)
	assert.Equal(t, "alice", l.GrantedBy)

	b.step()
	b.step()
	assert.Equal(t, []string{"boot", "tool", "val"}, b.leasedHosts())

	// Released as soon as the agent reports done: no soak.
	b.finish("val")

	out = b.step()
	assert.Nil(t, b.lease("val"))
	assert.Contains(t, auditActions(out), "lease.released")
	assert.Equal(t, fleet.HostDone, b.mustRollout("manual:m1").Host("val").State)
}

func TestForceStillAnswersToBudgetAndGate(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("v1", 50)
	b.host("v2", 50)
	b.manualRollout("m1", true, false, "v1", "v2")

	b.step()
	out := b.step()
	assert.Equal(t, []string{"v1"}, b.leasedHosts())
	assert.Equal(t, "budget 50% of 10%", queueReasons(out.Plan)["v2"])

	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 9 }
	b.refreshHealth()

	out = b.step()
	assert.Equal(t, "network gate: finality lag 9 epochs > 3", queueReasons(out.Plan)["v2"])
}

func TestIgnoreBudget(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 0
	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 9 }
	b.refreshHealth()
	b.host("v1", 50)
	b.host("v2", 50)
	b.manualRollout("m1", true, true, "v1", "v2")
	// An observed cause on the same host does not ride along past the gate.
	b.wants("v1", packagesNeed("2.0"))

	b.step()
	b.step()

	assert.Equal(t, []string{"v1", "v2"}, b.leasedHosts(), "budget, host limit and gate are skipped")
	require.Len(t, b.lease("v1").Needs, 1)
	assert.Equal(t, protocol.NeedReboot, b.lease("v1").Needs[0].Kind)
	assert.Equal(t, fleet.HostPending, b.mustRollout(packagesKey("2.0")).Host("v1").State)

	t.Run("the global pause still applies", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.Paused = true
		b.host("v1", 50)
		b.manualRollout("m1", true, true, "v1")

		out := b.step()

		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "paused", queueReasons(out.Plan)["v1"])
	})
}

func TestStaggeredManualRolloutFollowsTheStages(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.PolicyReboot = fleet.PolicyPaused // manual rollouts answer to no policy
	b.host("v1", 5)
	b.host("v2", 6)
	b.host("tool", 0)
	b.manualRollout("m1", false, false, "v1", "v2", "tool")

	out := b.step()
	assert.Equal(t, []string{"tool"}, b.leasedHosts())
	assert.Equal(t, "after hosts without validators", queueReasons(out.Plan)["v1"])
	assert.False(t, b.lease("tool").Force)

	b.serve("tool")
	assert.Equal(t, []string{"v1"}, b.leasedHosts())
	assert.Equal(t, "v1", b.mustRollout("manual:m1").Canary)
}

func TestManualDirectives(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("yes", 0)
	b.host("no", 0)
	b.host("slow", 0)

	r := &fleet.Rollout{
		ID: "m1", Kind: fleet.RolloutManual, Key: "manual:m1", State: fleet.RolloutActive,
		Approved: true, CreatedBy: "alice", CreatedAt: b.w.Now,
	}

	for _, name := range []string{"yes", "no", "slow"} {
		r.Hosts = append(r.Hosts, fleet.RolloutHost{Host: name, State: fleet.HostExpected})
		b.w.Directives = append(b.w.Directives, &fleet.Directive{
			Directive: protocol.Directive{ID: "d-" + name, Kind: protocol.DirectiveReboot},
			Host:      name, State: fleet.DirectivePending, RolloutID: "m1", CreatedBy: "alice",
		})
	}

	b.w.Rollouts = []*fleet.Rollout{r}

	b.step()
	assert.Equal(t, map[string]fleet.HostState{
		"yes": fleet.HostExpected, "no": fleet.HostExpected, "slow": fleet.HostExpected,
	}, memberStates(b.mustRollout("manual:m1")), "nothing answered yet; the rollout stays open")

	b.get("yes").Status.DirectiveAcks = []protocol.DirectiveAck{{ID: "d-yes", Status: protocol.DirectiveAccepted}}
	b.wants("yes", protocol.Need{
		ID: protocol.ManualNeedID("d-yes"), Kind: protocol.NeedReboot, DirectiveID: "d-yes",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	})
	b.get("no").Status.DirectiveAcks = []protocol.DirectiveAck{
		{ID: "d-no", Status: protocol.DirectiveRejected, Error: "verb reboot not allowed"},
	}

	b.step()

	m := b.mustRollout("manual:m1")
	assert.Equal(t, fleet.HostLeased, m.Host("yes").State)
	assert.Equal(t, fleet.HostSkipped, m.Host("no").State)
	assert.Equal(t, "verb reboot not allowed", m.Host("no").Error)
	assert.Equal(t, fleet.HostExpected, m.Host("slow").State)
	assert.Empty(t, b.rollout("reboot"), "a manual need opens no observed-cause rollout")

	states := map[string]fleet.DirectiveState{}
	for _, d := range b.w.Directives {
		states[d.ID] = d.State
	}

	assert.Equal(t, map[string]fleet.DirectiveState{
		"d-yes": fleet.DirectiveAccepted, "d-no": fleet.DirectiveRejected, "d-slow": fleet.DirectivePending,
	}, states)

	// The silent host stops blocking after hostStaleAfter; its directive is
	// cancelled when the rollout closes.
	b.serve("yes")
	b.step()

	closed := b.closed("manual:m1")
	require.NotNil(t, closed)
	assert.Equal(t, fleet.RolloutCompleted, closed.State)

	for _, d := range b.w.Directives {
		if d.ID == "d-slow" {
			assert.Equal(t, fleet.DirectiveCancelled, d.State)
		}
	}
}

func TestAdoption(t *testing.T) {
	op := func(phase protocol.Phase, disrupted bool) *protocol.Operation {
		return &protocol.Operation{
			LeaseID: "lost-1", Phase: phase, Disrupted: disrupted, Error: "boom",
			Steps: []protocol.Step{{NeedID: packagesNeed("2.0").ID, Status: protocol.StepRunning}},
		}
	}

	tests := []struct {
		name      string
		op        *protocol.Operation
		closed    bool
		wantState fleet.LeaseState
		wantNone  bool
		wantHalt  bool
	}{
		{name: "running operation", op: op(protocol.PhaseApplying, true), wantState: fleet.LeaseRunning},
		{name: "done and disrupted: verifying", op: op(protocol.PhaseDone, true), wantState: fleet.LeaseVerifying},
		{name: "failed: recorded and halted", op: op(protocol.PhaseFailed, true), wantState: fleet.LeaseFailed, wantHalt: true},
		{name: "done without disruption: nothing to hold", op: op(protocol.PhaseDone, false), wantNone: true},
		{name: "a known closed lease is not adopted", op: op(protocol.PhaseApplying, true), closed: true, wantNone: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", 10)
			b.host("b", 10)
			b.wants("a", packagesNeed("2.0"))
			b.wants("b", packagesNeed("2.0"))
			b.w.Settings.Paused = true
			b.step()
			b.w.Settings.Paused = false

			b.get("a").Status.Operation = tt.op
			b.w.ClosedLeases["lost-1"] = tt.closed

			out := b.step()

			var adopted *fleet.Lease

			for _, l := range out.Leases {
				if l.ID == "lost-1" {
					adopted = l
				}
			}

			if tt.wantNone {
				assert.Nil(t, adopted)
				assert.NotContains(t, auditActions(out), "lease.adopted")

				return
			}

			require.NotNil(t, adopted)
			assert.Equal(t, tt.wantState, adopted.State)
			assert.Equal(t, "a", adopted.Host)
			assert.Equal(t, grantedByAdoption, adopted.GrantedBy)
			assert.Equal(t, uint64(10), adopted.Weight)
			assert.Contains(t, auditActions(out), "lease.adopted")

			r := b.mustRollout(packagesKey("2.0"))
			assert.Equal(t, r.ID, adopted.Rollouts[packagesNeed("2.0").ID])
			assert.Equal(t, tt.wantHalt, r.State == fleet.RolloutHalted)

			if tt.wantHalt {
				assert.Equal(t, fleet.HostFailed, r.Host("a").State)
				assert.Empty(t, b.leasedHosts())

				return
			}

			// The host counts as holding a lease: it is the canary and "b"
			// waits behind it.
			assert.Equal(t, fleet.HostLeased, r.Host("a").State)
			assert.Equal(t, "a", r.Canary)
			assert.Equal(t, []string{"a"}, b.leasedHosts())
			assert.Equal(t, uint64(10), out.Plan.Budget.UnavailableWeight)
			assert.Equal(t, "after canary a", queueReasons(out.Plan)["b"])
		})
	}

	t.Run("an unknown operation replaces a lease the agent has not started", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 0)
		b.wants("a", packagesNeed("2.0"))
		b.step()

		granted := b.lease("a")
		require.NotNil(t, granted)

		b.get("a").Status.Operation = op(protocol.PhaseStarting, true)

		out := b.step()

		assert.Equal(t, []string{"lease.expired", "lease.adopted"}, auditActions(out))
		assert.Equal(t, "lost-1", b.lease("a").ID)
		assert.Len(t, b.w.Leases, 1, "one lease per host")
		assert.True(t, b.w.ClosedLeases[granted.ID])
	})
}

func TestReadinessEpochCounting(t *testing.T) {
	// The agent reports done 10 s into epoch 100.
	tests := []struct {
		name   string
		weight uint64
		soak   uint64
		// rows maps epochs to how many of the host's validators attested.
		rows map[uint64]uint64
		want bool
		note string
	}{
		{
			name: "the epoch the operation ended in does not count",
			soak: 1, rows: map[uint64]uint64{100: 10},
			note: "0 of 1 epochs with at least 9 of 10 validators attesting",
		},
		{
			name: "one complete epoch after done",
			soak: 1, rows: map[uint64]uint64{100: 0, 101: 10},
			want: true,
			note: "1 of 1 epochs with at least 9 of 10 validators attesting",
		},
		{name: "minParticipation of the host's weight is enough", soak: 1, rows: map[uint64]uint64{101: 9}, want: true},
		{name: "one validator short", soak: 1, rows: map[uint64]uint64{101: 8}},
		{name: "two epochs wanted, one seen", soak: 2, rows: map[uint64]uint64{101: 10}, note: "1 of 2 epochs with at least 9 of 10 validators attesting"},
		{name: "two consecutive epochs", soak: 2, rows: map[uint64]uint64{101: 10, 102: 10}, want: true},
		{name: "the run must be unbroken", soak: 2, rows: map[uint64]uint64{101: 10, 102: 3}},
		{name: "a new run after a bad epoch", soak: 2, rows: map[uint64]uint64{101: 3, 102: 10, 103: 10}, want: true},
		{name: "a missing epoch breaks the run", soak: 2, rows: map[uint64]uint64{101: 10, 103: 10}},
		{name: "three epochs", soak: 3, rows: map[uint64]uint64{101: 10, 102: 9, 103: 10}, want: true},
		{name: "a single validator must attest", weight: 1, soak: 1, rows: map[uint64]uint64{101: 0}, note: "0 of 1 epochs with at least 1 of 1 validators attesting"},
		{name: "a single validator attests", weight: 1, soak: 1, rows: map[uint64]uint64{101: 1}, want: true},
		{name: "no soak configured", soak: 0, rows: map[uint64]uint64{100: 0}, want: true},
		{name: "no rows after done at all", soak: 1, rows: map[uint64]uint64{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.autoLive = false
			b.w.Settings.SoakEpochs = tt.soak

			weight := tt.weight
			if weight == 0 {
				weight = 10
			}

			b.host("a", weight)
			b.host("other", 10)

			doneAt := b.w.Now
			lease := &fleet.Lease{
				ID: "l1", Host: "a", State: fleet.LeaseVerifying, AgentDoneAt: &doneAt,
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
		assert.Equal(t, "probes do not report synced", b.lease("a").Readiness)
		assert.Nil(t, b.lease("a").SyncedSince)

		b.advance(5 * time.Second)
		b.get("a").Status.Node = synced
		since := b.w.Now
		b.step()
		require.NotNil(t, b.lease("a").SyncedSince)
		assert.Equal(t, since, *b.lease("a").SyncedSince)
		assert.Equal(t, "probes synced, soaking for 6m24s", b.lease("a").Readiness)

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
		require.NotNil(t, b.lease("a"), "one second short of soakEpochs epoch durations")

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

	t.Run("validator host falls back to the probes when liveness is unknown", func(t *testing.T) {
		b := start(t, 10)
		b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
		b.refreshHealth()
		b.get("a").Status.Node = synced

		b.step()
		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "probes synced, soaking for 6m24s", b.lease("a").Readiness)

		b.advance(testEpoch)

		out := b.step()
		assert.Nil(t, b.lease("a"))
		assert.False(t, out.Plan.Gate.OK, "the gate fails, the open lease still finishes")
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

	b.advance(30 * time.Minute)
	b.step()
	require.NotNil(t, b.lease("a"), "exactly readinessTimeout is not yet over it")
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	b.advance(time.Second)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
	assert.Equal(t, "not ready after 30m0s: 0 of 1 epochs with at least 2 of 10 validators attesting",
		out.Leases[0].Error)

	r := b.mustRollout(packagesKey("2.0"))
	assert.Equal(t, fleet.RolloutHalted, r.State)
	assert.Equal(t, fleet.HostFailed, r.Host("a").State)
	assert.Empty(t, b.leasedHosts(), "the next host is never granted automatically after a failure")
	assert.Contains(t, queueReasons(out.Plan)["b"], "rollout halted: a: not ready after 30m0s")
}
