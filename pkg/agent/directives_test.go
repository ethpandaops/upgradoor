package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestDirectiveIsAppliedExactlyOnce(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	stop := e.start()

	// The controller repeats the directive for the whole test.
	e.ctrl.Direct(protocol.Directive{ID: "d-reboot", Kind: protocol.DirectiveReboot, Reason: "kernel test"})

	needID := protocol.ManualNeedID("d-reboot")

	req := e.ctrl.Await("ack and manual need", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-reboot") != nil
	})
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-reboot").Status)

	need := findNeed(t, &req, needID)
	assert.Equal(t, protocol.NeedReboot, need.Kind)
	assert.Equal(t, "d-reboot", need.DirectiveID)
	assert.Equal(t, []string{protocol.RebootReasonManual}, need.Reboot.Reasons)
	assert.Empty(t, e.host.Calls(), "a directive creates a need, it does not reboot")

	seen := 0

	e.ctrl.Await("ten more syncs", func(req *protocol.SyncRequest) bool {
		require.Equal(t, []string{needID}, agenttest.NeedIDs(req))
		require.NotNil(t, agenttest.AckOf(req, "d-reboot"), "the ack is repeated while the directive is")

		seen++

		return seen >= 10
	})

	// The answer survives a restart.
	stop()
	stop = e.start()

	req = e.ctrl.Await("ack after restart", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-reboot") != nil
	})
	assert.Equal(t, []string{needID}, agenttest.NeedIDs(&req))

	answers, ok := e.stateOnDisk()["answers"].([]any)
	require.True(t, ok)
	assert.Len(t, answers, 1)

	// Served, the need does not come back from the repeated directive.
	e.ctrl.Grant(e.lease("lease-reboot", need))

	op := e.ctrl.AwaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, &op, needID).Status)
	assert.Equal(t, "boot-1", e.host.BootID())

	seen = 0

	e.ctrl.Await("ten syncs after the reboot", func(req *protocol.SyncRequest) bool {
		if req.Operation != nil {
			return false
		}

		require.Empty(t, req.Needs)

		seen++

		return seen >= 10
	})

	stop()
	e.start()
	e.ctrl.AwaitSyncs(5)

	assert.Equal(t, 1, strings.Count(strings.Join(e.host.Calls(), "\n"), "reboot"))
	assert.Equal(t, "boot-1", e.host.BootID())
}

// The acks are those of the directives the last answer repeated, each once.
func TestAcksFollowTheRepeatedDirectives(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.start()

	onlyAck := func(id string) func(req *protocol.SyncRequest) bool {
		return func(req *protocol.SyncRequest) bool {
			return len(req.DirectiveAcks) == 1 && req.DirectiveAcks[0].ID == id
		}
	}

	e.ctrl.Direct(protocol.Directive{ID: "d-old", Kind: protocol.DirectiveReleaseHold})
	e.ctrl.Await("the old ack", onlyAck("d-old"))

	e.ctrl.Direct(protocol.Directive{ID: "d-new", Kind: protocol.DirectiveReleaseHold},
		protocol.Directive{ID: "d-new", Kind: protocol.DirectiveReleaseHold})
	e.ctrl.Await("the new ack", onlyAck("d-new"))

	seen := 0

	e.ctrl.Await("five more syncs", func(req *protocol.SyncRequest) bool {
		require.True(t, onlyAck("d-new")(req), "%v", req.DirectiveAcks)

		seen++

		return seen >= 5
	})

	e.ctrl.Direct()
	e.ctrl.Await("no ack", func(req *protocol.SyncRequest) bool { return req.DirectiveAcks == nil })

	seen = 0

	e.ctrl.Await("five more syncs", func(req *protocol.SyncRequest) bool {
		require.Empty(t, req.DirectiveAcks)

		seen++

		return seen >= 5
	})
}

func TestManualRoll(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.start()

	e.ctrl.Direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})

	needID := protocol.ManualNeedID("d-roll")

	req := e.ctrl.Await("manual roll accepted", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-roll") != nil
	})
	require.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-roll").Status)

	// Up to date: the need recreates the container on the same image.
	need := findNeed(t, &req, needID)
	assert.Equal(t, "d-roll", need.DirectiveID)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Group: agenttest.GroupExecution, Image: agenttest.GethImage,
		FromDigest: agenttest.GethV1, ToDigest: agenttest.GethV1,
	}, need.Image)
	assert.Empty(t, agenttest.Disruptive(e.host.Calls()), "a directive does not bypass the lease")

	before := e.host.Container("geth").ID

	e.ctrl.Grant(e.lease("lease-roll", need))

	op := e.ctrl.AwaitFinished("lease-roll")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, &op, needID).Status)
	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV1 + " start=true", "start geth", "start teku", "start validator",
	}, agenttest.Disruptive(e.host.Calls()))
	assert.NotEqual(t, before, e.host.Container("geth").ID)

	req = e.ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
	assert.Empty(t, req.Needs)

	req = e.ctrl.AwaitSyncs(5)
	assert.Empty(t, req.Needs, "served: the repeated directive does not bring the need back")
}

// A manual roll pulls nothing before its lease; the revision is known once
// the image is local.
func TestManualRollWaitsForItsLeaseToPull(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.start()

	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "rev-2")
	awaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	e.ctrl.Direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})

	req := e.ctrl.Await("manual roll accepted", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-roll") != nil
	})
	require.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-roll").Status)
	require.Equal(t, []string{protocol.ManualNeedID("d-roll")}, agenttest.NeedIDs(&req),
		"one container, one image need: the manual one")

	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Group: agenttest.GroupExecution, Image: agenttest.GethImage,
		FromDigest: agenttest.GethV1, ToDigest: agenttest.GethV2,
	}, req.Needs[0].Image)
	assert.Empty(t, e.host.Calls())
}

// withheld is the controller's lookup of the host's registry; while hold is
// set it has not looked one tag up.
type withheld struct {
	*agenttest.Host
	ref  string
	hold atomic.Bool
}

// withhold sets the controller's lookup to one that has not looked ref up.
func (e *env) withhold(ref string) *withheld {
	w := &withheld{Host: e.host, ref: ref}
	w.hold.Store(true)

	e.ctrl.Set(func(c *agenttest.Controller) { c.Lookup = w.Resolve })

	return w
}

func (r *withheld) Resolve(ctx context.Context, ref string) (string, error) {
	if r.hold.Load() && imageref.Same(ref, r.ref) {
		return "", nil
	}

	return r.Host.Resolve(ctx, ref)
}

// A roll directive withdrawn before the controller looked its tag up is
// dropped unanswered.
func TestRollDirectiveWithdrawnBeforeItsLookup(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	lookup := e.withhold(agenttest.GethImage)
	e.start()

	e.ctrl.Direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})
	e.ctrl.AwaitSyncs(3)
	e.ctrl.Direct()
	e.ctrl.AwaitSyncs(3)

	lookup.hold.Store(false)
	e.ctrl.Await("the tag looked up", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Registry != nil
	})
	e.ctrl.AwaitSyncs(3)

	for _, req := range e.ctrl.Syncs() {
		require.Nil(t, agenttest.AckOf(&req, "d-roll"))
		require.False(t, hasNeed(&req, protocol.ManualNeedID("d-roll")))
	}

	assert.Nil(t, e.stateOnDisk()["answers"], "nothing is remembered of the roll")
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

			e := newEnv(t, agenttest.BaseScenario())
			e.cfg.Policy.Verbs = tt.verbs
			e.start()

			tt.directive.ID = "d-1"
			e.ctrl.Direct(tt.directive)

			req := e.ctrl.Await("ack", func(req *protocol.SyncRequest) bool { return agenttest.AckOf(req, "d-1") != nil })
			ack := agenttest.AckOf(&req, "d-1")
			assert.Equal(t, protocol.DirectiveRejected, ack.Status)
			assert.Contains(t, ack.Error, tt.wantError)

			req = e.ctrl.AwaitSyncs(3)
			assert.Empty(t, req.Needs)
			assert.Empty(t, e.host.Calls())
		})
	}
}

func TestManualNeedIsDroppedWhenTheContainerStopsQualifying(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.start()

	e.ctrl.Direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "teku"})
	e.ctrl.Await("manual need", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.ManualNeedID("d-roll"))
	})

	// Pinned to a digest before the slot comes.
	require.NoError(t, e.host.SetContainerImage("teku", "consensys/teku@sha256:"+strings.Repeat("d", 64), "sha256:debug"))

	req := e.ctrl.Await("need gone", func(req *protocol.SyncRequest) bool {
		return !hasNeed(req, protocol.ManualNeedID("d-roll"))
	})
	assert.Empty(t, req.Needs)

	// Put back, it stays gone: the directive was answered.
	require.NoError(t, e.host.SetContainerImage("teku", agenttest.TekuImage, agenttest.TekuV1))

	req = e.ctrl.AwaitSyncs(3)
	assert.Empty(t, req.Needs)
}

func TestWithdrawDirectiveDropsTheManualNeed(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.start()

	e.ctrl.Direct(protocol.Directive{ID: "d-reboot", Kind: protocol.DirectiveReboot})
	e.ctrl.Await("manual need", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.ManualNeedID("d-reboot"))
	})

	e.ctrl.Direct(protocol.Directive{ID: "d-withdraw", Kind: protocol.DirectiveWithdraw, Withdraws: "d-reboot"})

	req := e.ctrl.Await("withdrawn", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-withdraw") != nil && !hasNeed(req, protocol.ManualNeedID("d-reboot"))
	})
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-withdraw").Status)

	req = e.ctrl.AwaitSyncs(3)
	assert.Empty(t, req.Needs, "the repeated reboot directive was answered: it creates nothing again")
}

// Each directive sees what the ones before it in the same answer did.
func TestDirectivesSeeTheHoldOfTheirOwnResponse(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.start()
	e.ctrl.AwaitSyncs(2)

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

	e.ctrl.Direct(
		protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance"},
		protocol.Directive{ID: "d-reboot-held", Kind: protocol.DirectiveReboot},
	)

	req := e.ctrl.Await("hold and reboot answered", acked("d-hold", "d-reboot-held"))
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-hold").Status)

	ack := agenttest.AckOf(&req, "d-reboot-held")
	assert.Equal(t, protocol.DirectiveRejected, ack.Status)
	assert.Contains(t, ack.Error, "controller hold")

	e.ctrl.Direct(
		protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold},
		protocol.Directive{ID: "d-reboot-free", Kind: protocol.DirectiveReboot},
	)

	req = e.ctrl.Await("release and reboot answered", acked("d-release", "d-reboot-free"))
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-release").Status)
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-reboot-free").Status)

	req = e.ctrl.Await("manual need", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.ManualNeedID("d-reboot-free"))
	})
	assert.False(t, req.Hold.Active())
	assert.False(t, hasNeed(&req, protocol.ManualNeedID("d-reboot-held")))
	assert.Empty(t, e.host.Calls())
}

// An override directive is taken within the group's policy, written and audited.
func TestOverrideDirective(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.cfg.Policy.Groups[agenttest.GroupExecution] = plan.Group{Overrides: []string{protocol.OverrideImage}}
	e.cfg.Policy.Groups[agenttest.GroupConsensus] = plan.Group{Overrides: []string{protocol.OverrideFlags}}
	e.start()
	e.ctrl.AwaitSyncs(2)

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

	e.ctrl.Direct(all...)

	req := e.ctrl.Await("every refusal", func(req *protocol.SyncRequest) bool {
		return len(req.DirectiveAcks) == len(all)
	})

	for name, tt := range refused {
		ack := agenttest.AckOf(&req, tt.d.ID)
		require.NotNil(t, ack, name)
		assert.Equal(t, protocol.DirectiveRejected, ack.Status, name)
		assert.Contains(t, ack.Error, tt.want, name)
	}

	_, err := os.Stat(e.cfg.Local.OverridesFile)
	require.ErrorIs(t, err, os.ErrNotExist, "a refused instruction writes nothing")
	assert.Empty(t, req.Needs)

	// The controller's override is taken next to a hand-written one.
	e.writeOverrides("overrides:\n  teku:\n    flags: {add: [\"--hand\"]}\n    by: me\n")
	e.ctrl.Direct(image("ok-1", "geth", agenttest.FixImage, &until), flags("ok-2", "teku", []string{"--ui"}, &until))

	req = e.ctrl.Await("both answers", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "ok-1") != nil && agenttest.AckOf(req, "ok-2") != nil
	})
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "ok-1").Status, agenttest.AckOf(&req, "ok-1").Error)
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "ok-2").Status, agenttest.AckOf(&req, "ok-2").Error)

	content := e.readOverrides()
	assert.Contains(t, content, " sam@example.org: set image "+agenttest.FixImage+"; was nothing\n")
	assert.Contains(t, content, "  \"geth\":\n    image: \""+agenttest.FixImage+"\"\n    by: \"sam@example.org\"\n"+
		"    reason: \"sync\"\n    until: "+until.Format(time.RFC3339)+"\n")
	assert.Contains(t, content, " sam@example.org: set flags +--ui; was flags +--hand\n")

	need := awaitOverrideNeed(t, e, "geth", false)
	assert.Equal(t, agenttest.FixImage, need.Override.Image)

	// Taking the image away needs no end and leaves nothing of the entry.
	e.ctrl.Direct(image("ok-3", "geth", "", nil))
	e.ctrl.Await("the clear", func(req *protocol.SyncRequest) bool { return agenttest.AckOf(req, "ok-3") != nil })

	content = e.readOverrides()
	assert.NotContains(t, content, "\"geth\":")
	assert.Contains(t, content, " sam@example.org: cleared; was image "+agenttest.FixImage+"\n")

	var refusals, accepted []string

	for _, rec := range e.auditRecords() {
		if rec.Source != "controller" {
			continue
		}

		if rec.Directive == "r8" {
			assert.Equal(t, "unknown", rec.User, "r8 names nobody")
		} else {
			assert.Equal(t, "sam@example.org", rec.User, rec.Directive)
			assert.Equal(t, []string{"geth"}, rec.Teams, rec.Directive)
			assert.Equal(t, "team_image_override", rec.Feature, rec.Directive)
		}

		switch rec.Directive {
		case "r4":
			assert.Equal(t, "image "+agenttest.FixImage, rec.After, "what was asked for")
		case "r8":
			assert.Equal(t, plan.NoOverride, rec.After, "nothing could be built")
		}

		if rec.Result == "refused" {
			refusals = append(refusals, rec.Directive)
		} else {
			accepted = append(accepted, rec.Directive+" "+rec.Action)
		}
	}

	assert.Len(t, refusals, len(refused), "every refusal is in the audit log")
	assert.Equal(t, []string{"ok-1 override.set", "ok-2 override.set", "ok-3 override.clear"}, accepted)
}

// An unwritten hold is tried again; an unwritten override is rejected.
func TestDirectivesWhoseWriteFails(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("a read-only directory stops nobody running as root")
	}

	e := overrideEnv(t)
	e.start()
	e.ctrl.AwaitSyncs(2)

	dir := filepath.Dir(e.cfg.Local.OverridesFile)
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	until := e.now().Add(time.Hour)
	e.ctrl.Direct(
		protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance"},
		protocol.Directive{
			ID: "d-override", Kind: protocol.DirectiveOverride, Container: "geth",
			Override: &protocol.OverrideInstruction{Part: protocol.OverrideImage, Image: agenttest.FixImage, Until: &until},
		},
	)

	req := e.ctrl.Await("the override answered", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-override") != nil
	})
	ack := agenttest.AckOf(&req, "d-override")
	assert.Equal(t, protocol.DirectiveRejected, ack.Status)
	assert.NotEmpty(t, ack.Error)

	req = e.ctrl.AwaitSyncs(3)
	assert.Nil(t, agenttest.AckOf(&req, "d-hold"), "a hold that is not written is not answered")
	assert.False(t, req.Hold.Controller)

	require.NoError(t, os.Chmod(dir, 0o755))

	req = e.ctrl.Await("the hold answered", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-hold") != nil
	})
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-hold").Status)
	assert.True(t, req.Hold.Controller)
	assert.Equal(t, protocol.DirectiveRejected, agenttest.AckOf(&req, "d-override").Status, "answered once")

	assert.Contains(t, auditActions(e.auditRecords()), "override.set failed")
}
