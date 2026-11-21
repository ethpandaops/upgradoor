package agent

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestDirectiveIsAppliedExactlyOnce(t *testing.T) {
	e := newEnv(t, baseScenario())
	stop := e.start()

	// The controller repeats the directive for the whole test, as it does
	// until it has processed the ack.
	directive := protocol.Directive{ID: "d-reboot", Kind: protocol.DirectiveReboot, Reason: "kernel test"}
	e.ctrl.direct(directive)

	needID := protocol.ManualNeedID("d-reboot")

	req := e.ctrl.await("ack and manual need", func(req *protocol.SyncRequest) bool {
		return ackOf(req, "d-reboot") != nil
	})
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-reboot").Status)

	need := findNeed(t, &req, needID)
	assert.Equal(t, protocol.NeedReboot, need.Kind)
	assert.Equal(t, "d-reboot", need.DirectiveID)
	assert.Equal(t, []string{protocol.RebootReasonManual}, need.Reboot.Reasons)

	// A directive creates a need. It does not reboot.
	assert.Empty(t, e.host.Calls())

	// Repeated: still one need, and the ack is repeated too.
	seen := 0

	e.ctrl.await("ten more syncs", func(req *protocol.SyncRequest) bool {
		require.Equal(t, []string{needID}, needIDs(req))
		require.NotNil(t, ackOf(req, "d-reboot"), "the ack is repeated while the directive is")

		seen++

		return seen >= 10
	})

	// The answer survives a restart of the agent.
	stop()
	stop = e.start()

	req = e.ctrl.await("ack after restart", func(req *protocol.SyncRequest) bool {
		return ackOf(req, "d-reboot") != nil
	})
	assert.Equal(t, []string{needID}, needIDs(&req))

	handled, ok := e.stateOnDisk()["handledDirectives"].([]any)
	require.True(t, ok)
	assert.Len(t, handled, 1)

	// Served by a lease, the need is gone. The directive is still repeated
	// and must not bring it back: one directive, one reboot.
	e.ctrl.grant(e.lease("lease-reboot", need))

	op := e.ctrl.awaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needID).Status)
	assert.Equal(t, "boot-1", e.host.BootID())

	seen = 0

	e.ctrl.await("ten syncs after the reboot", func(req *protocol.SyncRequest) bool {
		if req.Operation != nil {
			return false
		}

		require.Empty(t, req.Needs)

		seen++

		return seen >= 10
	})

	stop()
	e.start()
	e.ctrl.awaitSyncs(5)

	rebootCalls := 0

	for _, c := range e.host.Calls() {
		if c == "reboot" {
			rebootCalls++
		}
	}

	assert.Equal(t, 1, rebootCalls)
	assert.Equal(t, "boot-1", e.host.BootID())
}

func TestManualRoll(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.start()

	e.ctrl.direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})

	needID := protocol.ManualNeedID("d-roll")

	req := e.ctrl.await("manual roll accepted", func(req *protocol.SyncRequest) bool {
		return ackOf(req, "d-roll") != nil
	})
	require.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-roll").Status)

	// The container is up to date: the need recreates it on the same image.
	need := findNeed(t, &req, needID)
	assert.Equal(t, "d-roll", need.DirectiveID)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Image: gethImage, FromDigest: gethV1, ToDigest: gethV1,
	}, need.Image)
	assert.Empty(t, disruptive(e.host.Calls()), "a directive does not bypass the lease")

	before := e.host.Container("geth").ID

	e.ctrl.grant(e.lease("lease-roll", need))

	op := e.ctrl.awaitFinished("lease-roll")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needID).Status)
	assert.Equal(t, []string{"recreate geth " + gethV1 + " start=true", "start geth", "start teku", "start validator"},
		disruptive(e.host.Calls()))
	assert.NotEqual(t, before, e.host.Container("geth").ID)

	// Served: the need does not come back although the directive is still
	// repeated.
	req = e.ctrl.await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
	assert.Empty(t, req.Needs)

	req = e.ctrl.awaitSyncs(5)
	assert.Empty(t, req.Needs)
}

func TestManualRollResolvesAndPullsFirst(t *testing.T) {
	e := newEnv(t, baseScenario())
	// The regular poll will not come during this test.
	e.cfg.Registry.PollInterval = time.Hour
	e.start()

	e.ctrl.await("first registry check", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "geth").Registry != nil
	})

	e.host.SetRegistry(gethImage, gethV2, "rev-2")

	req := e.ctrl.awaitSyncs(5)
	require.Empty(t, req.Needs, "the agent has not looked at the registry again")

	e.ctrl.direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})

	req = e.ctrl.await("manual roll accepted", func(req *protocol.SyncRequest) bool {
		return ackOf(req, "d-roll") != nil
	})
	require.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-roll").Status)

	// One container, one image need: the manual one, at the tag's current
	// digest, with the image already local.
	require.Equal(t, []string{protocol.ManualNeedID("d-roll")}, needIDs(&req))
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Image: gethImage, FromDigest: gethV1, ToDigest: gethV2, ToRevision: "rev-2",
	}, req.Needs[0].Image)
	assert.Equal(t, []string{"pull " + gethImage}, e.host.Calls())
}

func TestRecheckDirective(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.cfg.Registry.PollInterval = time.Hour
	e.start()

	e.ctrl.await("first registry check", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "teku").Registry != nil
	})

	e.host.SetRegistry(tekuImage, tekuV2, "")

	req := e.ctrl.awaitSyncs(5)
	require.Empty(t, req.Needs)

	e.ctrl.direct(protocol.Directive{ID: "d-recheck", Kind: protocol.DirectiveRecheck})

	needID := protocol.ImageNeedID("teku", tekuV2)
	req = e.ctrl.await("need after recheck", func(req *protocol.SyncRequest) bool { return hasNeed(req, needID) })
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-recheck").Status)
	assert.Empty(t, findNeed(t, &req, needID).DirectiveID, "recheck creates no manual need")
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

			e := newEnv(t, baseScenario())
			e.cfg.Verbs = tt.verbs
			e.start()

			tt.directive.ID = "d-1"
			e.ctrl.direct(tt.directive)

			req := e.ctrl.await("ack", func(req *protocol.SyncRequest) bool { return ackOf(req, "d-1") != nil })
			ack := ackOf(&req, "d-1")
			assert.Equal(t, protocol.DirectiveRejected, ack.Status)
			assert.Contains(t, ack.Error, tt.wantError)

			req = e.ctrl.awaitSyncs(3)
			assert.Empty(t, req.Needs)
			assert.Empty(t, e.host.Calls())
		})
	}
}

func TestManualNeedIsDroppedWhenTheContainerStopsQualifying(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.start()

	e.ctrl.direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "teku"})
	e.ctrl.await("manual need", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.ManualNeedID("d-roll"))
	})

	// Somebody pins the container to a digest before the slot comes.
	require.NoError(t, e.host.SetContainerImage("teku", "consensys/teku@sha256:"+strings.Repeat("d", 64), "sha256:debug"))

	req := e.ctrl.await("need gone", func(req *protocol.SyncRequest) bool {
		return !hasNeed(req, protocol.ManualNeedID("d-roll"))
	})
	assert.Empty(t, req.Needs)

	// Putting it back does not revive it: the directive was answered.
	require.NoError(t, e.host.SetContainerImage("teku", tekuImage, tekuV1))

	req = e.ctrl.awaitSyncs(3)
	assert.Empty(t, req.Needs)
}

// Directives are applied in the order of the response, and each one sees
// what the ones before it did: a reboot that follows a hold is refused, a
// reboot that follows the release is not.
func TestDirectivesSeeTheHoldOfTheirOwnResponse(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.start()
	e.ctrl.awaitSyncs(2)

	acked := func(ids ...string) func(req *protocol.SyncRequest) bool {
		return func(req *protocol.SyncRequest) bool {
			for _, id := range ids {
				if ackOf(req, id) == nil {
					return false
				}
			}

			return true
		}
	}

	e.ctrl.direct(
		protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance"},
		protocol.Directive{ID: "d-reboot-held", Kind: protocol.DirectiveReboot},
	)

	req := e.ctrl.await("hold and reboot answered", acked("d-hold", "d-reboot-held"))
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-hold").Status)

	ack := ackOf(&req, "d-reboot-held")
	assert.Equal(t, protocol.DirectiveRejected, ack.Status)
	assert.Contains(t, ack.Error, "controller hold")

	e.ctrl.direct(
		protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold},
		protocol.Directive{ID: "d-reboot-free", Kind: protocol.DirectiveReboot},
	)

	req = e.ctrl.await("release and reboot answered", acked("d-release", "d-reboot-free"))
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-release").Status)
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-reboot-free").Status)

	// Only the reboot that was accepted left a manual need behind.
	req = e.ctrl.await("manual need", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.ManualNeedID("d-reboot-free"))
	})
	assert.False(t, req.Hold.Active())
	assert.False(t, hasNeed(&req, protocol.ManualNeedID("d-reboot-held")))
	assert.Empty(t, e.host.Calls())
}

// An override directive is taken only within what the host's own
// configuration lets the controller override: the opt-in per container, its
// image repositories, a separate opt-in for flags, and an end that is not too
// far away. What is taken is written into the overrides file with the user
// the controller named, and everything is in the host's audit log.
func TestOverrideDirective(t *testing.T) {
	e := overrideEnv(t)
	e.cfg.Containers[0].Overrides.Images = []string{"ethpandaops/geth", "ethpandaops/geth-*"}
	e.cfg.Containers[1].Overrides.Flags = true
	e.start()
	e.ctrl.awaitSyncs(2)

	until := e.now().Add(time.Hour).UTC().Truncate(time.Second)
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
	tooFar := e.now().Add(8 * 24 * time.Hour)

	refused := map[string]struct {
		d    protocol.Directive
		want string
	}{
		"another repository":   {image("r1", "geth", "evil/geth:fix", &until), "is not in a repository this host allows"},
		"no opt-in for images": {image("r2", "teku", "consensys/teku:fix", &until), "does not allow image overrides of container teku"},
		"no opt-in for flags":  {flags("r3", "geth", []string{"--x"}, &until), "does not allow flag overrides of container geth"},
		"no end":               {image("r4", "geth", fixImage, nil), "needs an end"},
		"an end too far away":  {image("r5", "geth", fixImage, &tooFar), "ends more than"},
		"not managed":          {image("r6", "dora", fixImage, &until), "is not managed"},
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

	e.ctrl.direct(all...)

	req := e.ctrl.await("every refusal", func(req *protocol.SyncRequest) bool {
		return len(req.DirectiveAcks) == len(all)
	})

	for name, tt := range refused {
		ack := ackOf(&req, tt.d.ID)
		require.NotNil(t, ack, name)
		assert.Equal(t, protocol.DirectiveRejected, ack.Status, name)
		assert.Contains(t, ack.Error, tt.want, name)
	}

	_, err := os.Stat(e.cfg.OverridesFile)
	require.ErrorIs(t, err, os.ErrNotExist, "a refused instruction writes nothing")
	assert.Empty(t, req.Needs)

	// A person wrote flags for teku by hand, which the host does not let the
	// controller do. The controller's image for geth is taken next to it.
	e.writeOverrides("overrides:\n  teku:\n    flags: {add: [\"--hand\"]}\n    by: me\n")
	e.ctrl.direct(image("ok-1", "geth", fixImage, &until), flags("ok-2", "teku", []string{"--ui"}, &until))

	req = e.ctrl.await("both answers", func(req *protocol.SyncRequest) bool {
		return ackOf(req, "ok-1") != nil && ackOf(req, "ok-2") != nil
	})
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "ok-1").Status, ackOf(&req, "ok-1").Error)
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "ok-2").Status, ackOf(&req, "ok-2").Error)

	raw, err := os.ReadFile(e.cfg.OverridesFile)
	require.NoError(t, err)

	content := string(raw)
	assert.Contains(t, content, " sam@example.org: set image "+fixImage+"; was nothing\n")
	assert.Contains(t, content, "  \"geth\":\n    image: \""+fixImage+"\"\n    by: \"sam@example.org\"\n"+
		"    reason: \"sync\"\n    until: "+until.Format(time.RFC3339)+"\n")
	assert.Contains(t, content, " sam@example.org: set flags +--ui; was flags +--hand\n",
		"the previous flags are in the comment line")

	need := awaitOverrideNeed(t, e, "geth", false)
	assert.Equal(t, fixImage, need.Override.Image)

	// Taking the image away needs no end, and leaves nothing of the entry.
	e.ctrl.direct(image("ok-3", "geth", "", nil))
	e.ctrl.await("the clear", func(req *protocol.SyncRequest) bool { return ackOf(req, "ok-3") != nil })

	raw, err = os.ReadFile(e.cfg.OverridesFile)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "\"geth\":")
	assert.Contains(t, string(raw), " sam@example.org: cleared; was image "+fixImage+"\n")

	records := e.auditRecords()

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
