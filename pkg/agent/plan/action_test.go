package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const securityAction = `actions:
  security-upgrades:
    description: Install pending security updates (unattended-upgrades)
    command: [/usr/bin/sudo, -n, /usr/local/lib/upgradoor/actions/security-upgrades]
    impact: independent
    timeout: 30m
    feature: details
    teams: [devops]
    when: securityUpdates
`

func TestActionsConfig(t *testing.T) {
	p, err := loadPolicy(t, securityAction)
	require.NoError(t, err)

	assert.Equal(t, Action{
		Description: "Install pending security updates (unattended-upgrades)",
		Command:     []string{"/usr/bin/sudo", "-n", "/usr/local/lib/upgradoor/actions/security-upgrades"},
		Impact:      protocol.ImpactIndependent, Timeout: 30 * time.Minute,
		Feature: "details", Teams: []string{"devops"}, When: protocol.ConditionSecurityUpdates,
	}, p.Actions["security-upgrades"])
	assert.True(t, p.Allows(protocol.VerbAction))
	assert.Equal(t, []protocol.Verb{protocol.VerbAction}, p.ReportedVerbs())

	none, err := loadPolicy(t, "verbs: [roll]\n")
	require.NoError(t, err)
	assert.False(t, none.Allows(protocol.VerbAction))
	assert.Equal(t, []protocol.Verb{protocol.VerbRoll}, none.ReportedVerbs())

	action := func(fields string) string {
		return "actions:\n  a:\n    command: [/bin/true]\n    impact: node\n    timeout: 1m\n" + fields
	}

	for name, tt := range map[string]struct {
		yaml string
		want string
	}{
		"a name that is no identifier": {
			"actions:\n  Bad Name: {command: [/bin/true], impact: node, timeout: 1m}\n", "not an action name",
		},
		"no command":         {"actions:\n  a: {impact: node, timeout: 1m}\n", "command: empty"},
		"a relative program": {"actions:\n  a: {command: [true], impact: node, timeout: 1m}\n", "not a clean absolute path"},
		"an unclean program": {
			"actions:\n  a: {command: [/usr/../bin/true], impact: node, timeout: 1m}\n", "not a clean absolute path",
		},
		"an empty argument":        {"actions:\n  a: {command: [/bin/true, ''], impact: node, timeout: 1m}\n", "invalid argument"},
		"a line break":             {"actions:\n  a: {command: [/bin/true, \"a\\nb\"], impact: node, timeout: 1m}\n", "invalid argument"},
		"an unknown impact":        {"actions:\n  a: {command: [/bin/true], impact: host, timeout: 1m}\n", `impact: "host"`},
		"no timeout":               {"actions:\n  a: {command: [/bin/true], impact: node}\n", "timeout: 0s"},
		"a timeout too long":       {"actions:\n  a: {command: [/bin/true], impact: node, timeout: 25h}\n", "timeout: 25h0m0s"},
		"an unknown feature":       {action("    feature: actions\n"), `feature: unknown "actions"`},
		"a service and a feature":  {action("    feature: upgradoor:manage\n"), `feature: unknown "upgradoor:manage"`},
		"a team twin":              {action("    feature: team_manage\n"), `feature: unknown "team_manage"`},
		"a team that is no name":   {action("    teams: [Dev Ops]\n"), `"Dev Ops" is not a team name`},
		"an unknown condition":     {action("    when: always\n"), `unknown condition "always"`},
		"two lines of description": {action("    description: \"a\\nb\"\n"), "description: one line"},
		"an unknown key":           {action("    shell: true\n"), "field shell not found"},
		"action as a verb":         {"verbs: [action]\n", `unknown verb "action"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadPolicy(t, tt.yaml)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestActionDue(t *testing.T) {
	a := &Action{When: protocol.ConditionSecurityUpdates}
	open := &System{SecurityOpen: 2, InventoryHash: "h1"}

	assert.True(t, ActionDue(a, open, ""))
	assert.True(t, ActionDue(a, open, "h0"), "the inventory changed since the last run")
	assert.False(t, ActionDue(a, open, "h1"), "a run that left updates behind is not repeated")
	assert.False(t, ActionDue(a, &System{InventoryHash: "h1"}, ""), "nothing to install")
	assert.False(t, ActionDue(a, nil, ""), "system state unknown")
	assert.False(t, ActionDue(&Action{}, open, ""), "an action without a condition")
}

func actionPolicy() *Policy {
	p := needsPolicy(allVerbs...)
	p.Actions = map[string]Action{
		"security-upgrades": {
			Command: []string{"/bin/true"}, Impact: protocol.ImpactIndependent, Timeout: time.Minute,
			When: protocol.ConditionSecurityUpdates, Feature: "details", Teams: []string{"devops"},
		},
		"restart-exporter": {Command: []string{"/bin/true"}, Impact: protocol.ImpactNode, Timeout: time.Minute},
	}

	return p
}

func actionView(p *Policy, securityOpen int) *View {
	facts := hostFacts()
	facts.System.SecurityOpen = securityOpen
	facts.System.InventoryHash = "h1"
	v := Classify(p, facts, known(), hostChecks(), testNow)

	return &v
}

func TestActionNeedsAndReport(t *testing.T) {
	p := actionPolicy()
	want := protocol.Need{
		ID: protocol.ActionNeedID("security-upgrades"), Kind: protocol.NeedAction,
		Action: &protocol.ActionNeed{
			Name: "security-upgrades", Impact: protocol.ImpactIndependent, When: protocol.ConditionSecurityUpdates,
		},
	}

	v := actionView(p, 1)
	assert.Contains(t, Needs(p, v, nil), want)

	report := Actions(p, v)
	require.Len(t, report, 2)
	assert.Equal(t, protocol.ActionInfo{Name: "restart-exporter", Impact: protocol.ImpactNode}, report[0])
	assert.Equal(t, protocol.ActionInfo{
		Name: "security-upgrades", Impact: protocol.ImpactIndependent, Feature: "details",
		Teams: []string{"devops"}, When: protocol.ConditionSecurityUpdates, Due: true,
	}, report[1])

	// No condition, a run since the inventory last changed.
	assert.NotContains(t, ids(Needs(p, actionView(p, 0), nil)), want.ID)
	assert.False(t, Actions(p, actionView(p, 0))[1].Due)

	ran := actionView(p, 1)
	ran.ActionsRan = map[string]string{"security-upgrades": "h1"}
	assert.NotContains(t, ids(Needs(p, ran, nil)), want.ID)

	held := actionView(p, 1)
	held.Hold = protocol.Hold{Local: true}
	assert.Contains(t, ids(Needs(p, held, nil)), want.ID, "a hold hides no need")

	// A manual need of the action covers the observed one.
	manual, reject := ManualAction(p, v, "security-upgrades", "d-1")
	require.Empty(t, reject)
	assert.Equal(t, protocol.Need{
		ID: protocol.ManualNeedID("d-1"), Kind: protocol.NeedAction, DirectiveID: "d-1",
		Action: &protocol.ActionNeed{Name: "security-upgrades", Impact: protocol.ImpactIndependent},
	}, manual)

	got := ids(Needs(p, v, []protocol.Need{manual}))
	assert.Contains(t, got, manual.ID)
	assert.NotContains(t, got, want.ID)
}

func TestManualAction(t *testing.T) {
	p := actionPolicy()

	_, reject := ManualAction(p, actionView(p, 0), "rm-rf", "d-1")
	assert.Equal(t, `unknown action "rm-rf"`, reject)

	held := actionView(p, 0)
	held.Hold = staffHold
	_, reject = ManualAction(p, held, "restart-exporter", "d-1")
	assert.NotEmpty(t, reject)

	need, reject := ManualAction(p, actionView(p, 0), "restart-exporter", "d-2")
	require.Empty(t, reject)
	assert.Equal(t, protocol.ImpactNode, need.Action.Impact)

	// A stored manual need of an action that is gone from agent.yaml is dropped.
	delete(p.Actions, "restart-exporter")
	out := Reconcile(p, actionView(p, 0), []protocol.Need{need})
	assert.Empty(t, out.Kept)
	assert.Equal(t, "action restart-exporter is no longer configured", out.Dropped[need.ID])
}

func TestValidateActions(t *testing.T) {
	p := actionPolicy()
	observed := protocol.Need{
		ID: protocol.ActionNeedID("security-upgrades"), Kind: protocol.NeedAction,
		Action: &protocol.ActionNeed{Name: "security-upgrades"},
	}
	unknown := protocol.Need{
		ID: protocol.ActionNeedID("rm-rf"), Kind: protocol.NeedAction, Action: &protocol.ActionNeed{Name: "rm-rf"},
	}

	lp, err := Validate(p, leaseOf(observed, unknown), actionView(p, 1), nil)
	require.NoError(t, err)
	assert.Equal(t, []ActionRun{{ID: observed.ID, Name: "security-upgrades"}}, lp.Actions)
	assert.Equal(t, protocol.StepRefused, lp.Verdicts[unknown.ID].Status)

	lp, err = Validate(p, leaseOf(observed), actionView(p, 0), nil)
	require.NoError(t, err)
	assert.True(t, lp.Empty())
	assert.Equal(t, protocol.StepSuperseded, lp.Verdicts[observed.ID].Status)

	// The verb is the configured actions': without any, an action is refused.
	lp, err = Validate(needsPolicy(allVerbs...), leaseOf(observed), actionView(p, 1), nil)
	require.NoError(t, err)
	assert.Equal(t, "verb action is not allowed on this host", lp.Verdicts[observed.ID].Error)
}
