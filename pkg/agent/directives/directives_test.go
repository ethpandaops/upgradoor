package directives_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestDirectiveIsAppliedExactlyOnce(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	stop := e.Start()

	// The controller repeats the directive for the whole test, as it does
	// until it has processed the ack.
	directive := protocol.Directive{ID: "d-reboot", Kind: protocol.DirectiveReboot, Reason: "kernel test"}
	e.Ctrl.Direct(directive)

	needID := protocol.ManualNeedID("d-reboot")

	req := e.Ctrl.Await("ack and manual need", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-reboot") != nil
	})
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-reboot").Status)

	need := agenttest.FindNeed(t, &req, needID)
	assert.Equal(t, protocol.NeedReboot, need.Kind)
	assert.Equal(t, "d-reboot", need.DirectiveID)
	assert.Equal(t, []string{protocol.RebootReasonManual}, need.Reboot.Reasons)

	// A directive creates a need. It does not reboot.
	assert.Empty(t, e.Host.Calls())

	// Repeated: still one need, and the ack is repeated too.
	seen := 0

	e.Ctrl.Await("ten more syncs", func(req *protocol.SyncRequest) bool {
		require.Equal(t, []string{needID}, agenttest.NeedIDs(req))
		require.NotNil(t, agenttest.AckOf(req, "d-reboot"), "the ack is repeated while the directive is")

		seen++

		return seen >= 10
	})

	// The answer survives a restart of the agent.
	stop()
	stop = e.Start()

	req = e.Ctrl.Await("ack after restart", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-reboot") != nil
	})
	assert.Equal(t, []string{needID}, agenttest.NeedIDs(&req))

	handled, ok := e.StateOnDisk()["handledDirectives"].([]any)
	require.True(t, ok)
	assert.Len(t, handled, 1)

	// Served by a lease, the need is gone. The directive is still repeated
	// and must not bring it back: one directive, one reboot.
	e.Ctrl.Grant(e.Lease("lease-reboot", need))

	op := e.Ctrl.AwaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, needID).Status)
	assert.Equal(t, "boot-1", e.Host.BootID())

	seen = 0

	e.Ctrl.Await("ten syncs after the reboot", func(req *protocol.SyncRequest) bool {
		if req.Operation != nil {
			return false
		}

		require.Empty(t, req.Needs)

		seen++

		return seen >= 10
	})

	stop()
	e.Start()
	e.Ctrl.AwaitSyncs(5)

	rebootCalls := 0

	for _, c := range e.Host.Calls() {
		if c == "reboot" {
			rebootCalls++
		}
	}

	assert.Equal(t, 1, rebootCalls)
	assert.Equal(t, "boot-1", e.Host.BootID())
}

func TestManualRoll(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Start()

	e.Ctrl.Direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})

	needID := protocol.ManualNeedID("d-roll")

	req := e.Ctrl.Await("manual roll accepted", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-roll") != nil
	})
	require.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-roll").Status)

	// The container is up to date: the need recreates it on the same image.
	need := agenttest.FindNeed(t, &req, needID)
	assert.Equal(t, "d-roll", need.DirectiveID)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Group: agenttest.GroupExecution, Image: agenttest.GethImage, FromDigest: agenttest.GethV1, ToDigest: agenttest.GethV1,
	}, need.Image)
	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()), "a directive does not bypass the lease")

	before := e.Host.Container("geth").ID

	e.Ctrl.Grant(e.Lease("lease-roll", need))

	op := e.Ctrl.AwaitFinished("lease-roll")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, needID).Status)
	assert.Equal(t, []string{"recreate geth " + agenttest.GethV1 + " start=true", "start geth", "start teku", "start validator"},
		agenttest.Disruptive(e.Host.Calls()))
	assert.NotEqual(t, before, e.Host.Container("geth").ID)

	// Served: the need does not come back although the directive is still
	// repeated.
	req = e.Ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
	assert.Empty(t, req.Needs)

	req = e.Ctrl.AwaitSyncs(5)
	assert.Empty(t, req.Needs)
}

func TestManualRollResolvesAndPullsFirst(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	// The regular poll will not come during this test.
	e.Cfg.Registry.PollInterval = time.Hour
	// ... and the controller hands out no lookup duty.
	e.Ctrl.Set(func(c *agenttest.Controller) { c.NoLookups = true })
	e.Start()

	e.Ctrl.Await("first registry check", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Registry != nil
	})

	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "rev-2")

	req := e.Ctrl.AwaitSyncs(5)
	require.Empty(t, req.Needs, "the agent has not looked at the registry again")

	e.Ctrl.Direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})

	req = e.Ctrl.Await("manual roll accepted", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-roll") != nil
	})
	require.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-roll").Status)

	// One container, one image need: the manual one, at the tag's current
	// digest. Its image is pulled in a pull window.
	require.Equal(t, []string{protocol.ManualNeedID("d-roll")}, agenttest.NeedIDs(&req))

	req = e.Ctrl.Await("the image pulled", func(req *protocol.SyncRequest) bool {
		return agenttest.HasPulledNeed(req, protocol.ManualNeedID("d-roll"))
	})
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Group: agenttest.GroupExecution, Image: agenttest.GethImage, FromDigest: agenttest.GethV1, ToDigest: agenttest.GethV2, ToRevision: "rev-2",
	}, req.Needs[0].Image)
	assert.Equal(t, []string{"pull " + agenttest.GethImage}, e.Host.Calls())
}

func TestRecheckDirective(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Cfg.Registry.PollInterval = time.Hour
	e.Ctrl.Set(func(c *agenttest.Controller) { c.NoLookups = true })
	e.Start()

	e.Ctrl.Await("first registry check", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "teku").Registry != nil
	})

	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")

	req := e.Ctrl.AwaitSyncs(5)
	require.Empty(t, req.Needs)

	e.Ctrl.Direct(protocol.Directive{ID: "d-recheck", Kind: protocol.DirectiveRecheck})

	needID := protocol.ImageNeedID("teku", agenttest.TekuV2)
	req = e.Ctrl.Await("need after recheck", func(req *protocol.SyncRequest) bool { return agenttest.HasNeed(req, needID) })
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-recheck").Status)
	assert.Empty(t, agenttest.FindNeed(t, &req, needID).DirectiveID, "recheck creates no manual need")
}

func TestDirectivesAreRejectedByLocalPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		verbs     []protocol.Verb
		directive protocol.Directive
		wantError string
	}{
		{
			name:      "roll without the verb",
			verbs:     []protocol.Verb{protocol.VerbReboot, protocol.VerbPackages},
			directive: protocol.Directive{Kind: protocol.DirectiveRoll, Container: "geth"},
			wantError: "verb roll is not allowed",
		},
		{
			name:      "reboot without the verb",
			verbs:     []protocol.Verb{protocol.VerbRoll},
			directive: protocol.Directive{Kind: protocol.DirectiveReboot},
			wantError: "verb reboot is not allowed",
		},
		{
			name:      "roll of an unmanaged container",
			verbs:     []protocol.Verb{protocol.VerbRoll},
			directive: protocol.Directive{Kind: protocol.DirectiveRoll, Container: "dora"},
			wantError: "dora is not managed",
		},
		{
			name:      "roll without a container",
			verbs:     []protocol.Verb{protocol.VerbRoll},
			directive: protocol.Directive{Kind: protocol.DirectiveRoll},
			wantError: "names no container",
		},
		{
			name:      "a kind this agent does not know",
			verbs:     []protocol.Verb{protocol.VerbRoll, protocol.VerbReboot, protocol.VerbPackages},
			directive: protocol.Directive{Kind: "exec", Reason: "curl | sh"},
			wantError: "unknown directive kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := agenttest.NewEnv(t, agenttest.BaseScenario())
			e.Cfg.Verbs = tt.verbs
			e.Start()

			tt.directive.ID = "d-1"
			e.Ctrl.Direct(tt.directive)

			req := e.Ctrl.Await("ack", func(req *protocol.SyncRequest) bool { return agenttest.AckOf(req, "d-1") != nil })
			ack := agenttest.AckOf(&req, "d-1")
			assert.Equal(t, protocol.DirectiveRejected, ack.Status)
			assert.Contains(t, ack.Error, tt.wantError)

			req = e.Ctrl.AwaitSyncs(3)
			assert.Empty(t, req.Needs)
			assert.Empty(t, e.Host.Calls())
		})
	}
}

func TestManualNeedIsDroppedWhenTheContainerStopsQualifying(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Start()

	e.Ctrl.Direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "teku"})
	e.Ctrl.Await("manual need", func(req *protocol.SyncRequest) bool {
		return agenttest.HasNeed(req, protocol.ManualNeedID("d-roll"))
	})

	// Somebody pins the container to a digest before the slot comes.
	require.NoError(t, e.Host.SetContainerImage("teku", "consensys/teku@sha256:"+strings.Repeat("d", 64), "sha256:debug"))

	req := e.Ctrl.Await("need gone", func(req *protocol.SyncRequest) bool {
		return !agenttest.HasNeed(req, protocol.ManualNeedID("d-roll"))
	})
	assert.Empty(t, req.Needs)

	// Putting it back does not revive it: the directive was answered.
	require.NoError(t, e.Host.SetContainerImage("teku", agenttest.TekuImage, agenttest.TekuV1))

	req = e.Ctrl.AwaitSyncs(3)
	assert.Empty(t, req.Needs)
}

// Directives are applied in the order of the response, and each one sees
// what the ones before it did: a reboot that follows a hold is refused, a
// reboot that follows the release is not.
func TestDirectivesSeeTheHoldOfTheirOwnResponse(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Start()
	e.Ctrl.AwaitSyncs(2)

	acked := func(ids ...string) func(req *protocol.SyncRequest) bool {
		return func(req *protocol.SyncRequest) bool {
			for _, id := range ids {
				if agenttest.AckOf(req, id) == nil {
					return false
				}
			}

			return true
		}
	}

	e.Ctrl.Direct(
		protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance"},
		protocol.Directive{ID: "d-reboot-held", Kind: protocol.DirectiveReboot},
	)

	req := e.Ctrl.Await("hold and reboot answered", acked("d-hold", "d-reboot-held"))
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-hold").Status)

	ack := agenttest.AckOf(&req, "d-reboot-held")
	assert.Equal(t, protocol.DirectiveRejected, ack.Status)
	assert.Contains(t, ack.Error, "controller hold")

	e.Ctrl.Direct(
		protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold},
		protocol.Directive{ID: "d-reboot-free", Kind: protocol.DirectiveReboot},
	)

	req = e.Ctrl.Await("release and reboot answered", acked("d-release", "d-reboot-free"))
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-release").Status)
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-reboot-free").Status)

	// Only the reboot that was accepted left a manual need behind.
	req = e.Ctrl.Await("manual need", func(req *protocol.SyncRequest) bool {
		return agenttest.HasNeed(req, protocol.ManualNeedID("d-reboot-free"))
	})
	assert.False(t, req.Hold.Active())
	assert.False(t, agenttest.HasNeed(&req, protocol.ManualNeedID("d-reboot-held")))
	assert.Empty(t, e.Host.Calls())
}

// An override directive is taken only within what the host's own
// configuration lets the controller override: the opt-in per container, its
// image repositories, a separate opt-in for flags, and an end that is not too
// far away. What is taken is written into the overrides file with the user
// the controller named, and everything is in the host's audit log.
func TestOverrideDirective(t *testing.T) {
	t.Parallel()

	e := agenttest.NewOverrideEnv(t)
	e.Cfg.Groups[agenttest.GroupExecution] = config.Group{Overrides: []string{protocol.OverrideImage}}
	e.Cfg.Groups[agenttest.GroupConsensus] = config.Group{Overrides: []string{protocol.OverrideFlags}}
	e.Start()
	e.Ctrl.AwaitSyncs(2)

	until := e.Now().Add(time.Hour).UTC().Truncate(time.Second)
	sam := &protocol.Actor{User: "sam@example.org", Teams: []string{"geth"}, Feature: "team_image_override"}
	image := func(id, container, ref string, end *time.Time) protocol.Directive {
		return protocol.Directive{
			ID: id, Kind: protocol.DirectiveOverride, Container: container, Reason: "sync", By: sam,
			Override: &protocol.OverrideInstruction{Part: protocol.OverrideImage, Image: ref, Until: end},
		}
	}
	flags := func(id, container string, add []string, end *time.Time) protocol.Directive {
		return protocol.Directive{
			ID: id, Kind: protocol.DirectiveOverride, Container: container, Reason: "debug", By: sam,
			Override: &protocol.OverrideInstruction{Part: protocol.OverrideFlags, AddFlags: add, Until: end},
		}
	}
	tooFar := e.Now().Add(8 * 24 * time.Hour)

	refused := map[string]struct {
		d    protocol.Directive
		want string
	}{
		"no opt-in for images": {image("r2", "teku", "consensys/teku:fix", &until), `group "consensus" of container teku does not allow image overrides`},
		"no opt-in for flags":  {flags("r3", "geth", []string{"--x"}, &until), `group "execution" of container geth does not allow flag overrides`},
		"no end":               {image("r4", "geth", agenttest.FixImage, nil), "needs an end"},
		"an end too far away":  {image("r5", "geth", agenttest.FixImage, &tooFar), "ends more than"},
		"not managed":          {image("r6", "dora", agenttest.FixImage, &until), "is not managed"},
		"a digest":             {image("r7", "geth", "ethpandaops/geth@sha256:abc", &until), ""},
		"nothing in it": {
			protocol.Directive{ID: "r8", Kind: protocol.DirectiveOverride, Container: "geth"},
			"names no container or no override",
		},
	}

	all := make([]protocol.Directive, 0, len(refused))
	for _, tt := range refused {
		all = append(all, tt.d)
	}

	e.Ctrl.Direct(all...)

	req := e.Ctrl.Await("every refusal", func(req *protocol.SyncRequest) bool {
		return len(req.DirectiveAcks) == len(all)
	})

	for name, tt := range refused {
		ack := agenttest.AckOf(&req, tt.d.ID)
		require.NotNil(t, ack, name)
		assert.Equal(t, protocol.DirectiveRejected, ack.Status, name)
		assert.Contains(t, ack.Error, tt.want, name)
	}

	_, err := os.Stat(e.Cfg.OverridesFile)
	require.ErrorIs(t, err, os.ErrNotExist, "a refused instruction writes nothing")
	assert.Empty(t, req.Needs)

	// A person wrote flags for teku by hand, which the host does not let the
	// controller do. The controller's image for geth is taken next to it.
	e.WriteOverrides("overrides:\n  teku:\n    flags: {add: [\"--hand\"]}\n    by: me\n")
	e.Ctrl.Direct(image("ok-1", "geth", agenttest.FixImage, &until), flags("ok-2", "teku", []string{"--ui"}, &until))

	req = e.Ctrl.Await("both answers", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "ok-1") != nil && agenttest.AckOf(req, "ok-2") != nil
	})
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "ok-1").Status, agenttest.AckOf(&req, "ok-1").Error)
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "ok-2").Status, agenttest.AckOf(&req, "ok-2").Error)

	raw, err := os.ReadFile(e.Cfg.OverridesFile)
	require.NoError(t, err)

	content := string(raw)
	assert.Contains(t, content, " sam@example.org: set image "+agenttest.FixImage+"; was nothing\n")
	assert.Contains(t, content, "  \"geth\":\n    image: \""+agenttest.FixImage+"\"\n    by: \"sam@example.org\"\n"+
		"    reason: \"sync\"\n    until: "+until.Format(time.RFC3339)+"\n")
	assert.Contains(t, content, " sam@example.org: set flags +--ui; was flags +--hand\n",
		"the previous flags are in the comment line")

	need := agenttest.AwaitOverrideNeed(t, e, "geth", false)
	assert.Equal(t, agenttest.FixImage, need.Override.Image)

	// Taking the image away needs no end, and leaves nothing of the entry.
	e.Ctrl.Direct(image("ok-3", "geth", "", nil))
	e.Ctrl.Await("the clear", func(req *protocol.SyncRequest) bool { return agenttest.AckOf(req, "ok-3") != nil })

	raw, err = os.ReadFile(e.Cfg.OverridesFile)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "\"geth\":")
	assert.Contains(t, string(raw), " sam@example.org: cleared; was image "+agenttest.FixImage+"\n")

	records := e.AuditRecords()

	var refusals, accepted []string

	for _, rec := range records {
		if rec.Source != "controller" {
			continue
		}

		assert.Equal(t, "sam@example.org", rec.User, rec.Directive)

		if rec.Directive != "r8" {
			assert.Equal(t, []string{"geth"}, rec.Teams, rec.Directive)
			assert.Equal(t, "team_image_override", rec.Feature, rec.Directive)
		}

		if rec.Result == "refused" {
			refusals = append(refusals, rec.Directive)
		} else {
			accepted = append(accepted, rec.Directive+" "+rec.Action)
		}
	}

	assert.Len(t, refusals, len(refused)-1, "every refusal that named a container is in the audit log")
	assert.Equal(t, []string{"ok-1 override.set", "ok-2 override.set", "ok-3 override.clear"}, accepted)
}
