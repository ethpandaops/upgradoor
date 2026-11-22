package plan

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of an action's timeout.
const (
	MinActionTimeout = time.Second
	MaxActionTimeout = 24 * time.Hour
	// maxActionArgs bounds an action's argument vector.
	maxActionArgs = 64
)

// Action is one entry of the actions section of agent.yaml: a command the
// controller may have the agent run under a lease, by name only.
type Action struct {
	Description string `yaml:"description"`
	// Command is the argument vector; Command[0] is an absolute program. It
	// is run without a shell.
	Command []string        `yaml:"command"`
	Impact  protocol.Impact `yaml:"impact"`
	// Timeout ends the run; the run then fails.
	Timeout time.Duration `yaml:"timeout"`
	// Feature ("<service>:<feature>") and Teams allow a person to start the
	// action, next to admin.
	Feature string   `yaml:"feature"`
	Teams   []string `yaml:"teams"`
	// When is the condition under which the action is due by itself; empty
	// for an action that runs only when a person starts it.
	When protocol.ActionCondition `yaml:"when"`
}

// validateActions checks the actions section.
func validateActions(actions map[string]Action) error {
	if len(actions) > protocol.MaxActions {
		return fmt.Errorf("actions: %d, at most %d", len(actions), protocol.MaxActions)
	}

	// Sorted, so a file with several mistakes always names the same one.
	for _, name := range slices.Sorted(maps.Keys(actions)) {
		a := actions[name]
		if err := a.validate(name); err != nil {
			return fmt.Errorf("actions: %s: %w", name, err)
		}
	}

	return nil
}

func (a *Action) validate(name string) error {
	switch {
	case !protocol.ValidActionName(name):
		return errors.New("not an action name ([a-z0-9][a-z0-9_-]{0,62})")
	case len(a.Description) > protocol.MaxActionDescription || Line(a.Description) != a.Description:
		return fmt.Errorf("description: one line of at most %d bytes", protocol.MaxActionDescription)
	case !a.Impact.Valid():
		return fmt.Errorf("impact: %q, want independent or node", a.Impact)
	case a.Timeout < MinActionTimeout || a.Timeout > MaxActionTimeout:
		return fmt.Errorf("timeout: %s, want %s to %s", a.Timeout, MinActionTimeout, MaxActionTimeout)
	case a.Feature != "" && !protocol.ValidFeature(a.Feature):
		return fmt.Errorf("feature: %q is not <service>:<feature>", a.Feature)
	case len(a.Teams) > protocol.MaxOwners:
		return fmt.Errorf("teams: %d, at most %d", len(a.Teams), protocol.MaxOwners)
	case a.When != "" && !a.When.Valid():
		return fmt.Errorf("when: unknown condition %q (%s)", a.When, protocol.ConditionSecurityUpdates)
	}

	for _, team := range a.Teams {
		if !protocol.ValidOwner(team) {
			return fmt.Errorf("teams: %q is not a team name", team)
		}
	}

	return validateArgv(a.Command)
}

// validateArgv checks an argument vector: an absolute, clean program
// path and arguments without NUL or line breaks.
func validateArgv(argv []string) error {
	switch {
	case len(argv) == 0:
		return errors.New("command: empty")
	case len(argv) > maxActionArgs:
		return fmt.Errorf("command: %d arguments, at most %d", len(argv), maxActionArgs)
	case !path.IsAbs(argv[0]) || path.Clean(argv[0]) != argv[0]:
		return fmt.Errorf("command: %q is not a clean absolute path", argv[0])
	}

	for _, arg := range argv {
		if arg == "" || strings.ContainsAny(arg, "\x00\r\n") {
			return fmt.Errorf("command: invalid argument %q", arg)
		}
	}

	return nil
}

// ActionDue reports whether the condition of the action holds: for
// securityUpdates, the inventory has security updates outside the blocked
// set and it changed since the action last ran (ran is the inventory hash
// after that run). A run that left updates behind is not repeated until the
// inventory changes.
func ActionDue(a *Action, sys *System, ran string) bool {
	switch {
	case a.When != protocol.ConditionSecurityUpdates || sys == nil:
		return false
	case sys.SecurityOpen == 0:
		return false
	default:
		return ran == "" || sys.InventoryHash != ran
	}
}

// Actions is what the sync reports of the configured actions, sorted by
// name.
func Actions(policy *Policy, view *View) []protocol.ActionInfo {
	out := make([]protocol.ActionInfo, 0, len(policy.Actions))

	for _, name := range slices.Sorted(maps.Keys(policy.Actions)) {
		a := policy.Actions[name]
		out = append(out, protocol.ActionInfo{
			Name: name, Description: a.Description, Impact: a.Impact, Feature: a.Feature,
			Teams: slices.Clone(a.Teams), When: a.When,
			Due: ActionDue(&a, view.System, view.ActionsRan[name]),
		})
	}

	return out
}

// actionNeeds are the needs of the actions whose condition holds, except
// those a manual need of the same action covers.
func actionNeeds(policy *Policy, view *View, covered map[string]struct{}) []protocol.Need {
	needs := make([]protocol.Need, 0, 1)

	for _, name := range slices.Sorted(maps.Keys(policy.Actions)) {
		a := policy.Actions[name]
		if _, ok := covered[name]; ok || !ActionDue(&a, view.System, view.ActionsRan[name]) {
			continue
		}

		needs = append(needs, protocol.Need{
			ID: protocol.ActionNeedID(name), Kind: protocol.NeedAction,
			Action: &protocol.ActionNeed{Name: name, Impact: a.Impact, When: a.When},
		})
	}

	return needs
}

// ManualAction answers an action directive with a manual action need, or
// why not. The controller names the action only; what runs is agent.yaml's.
func ManualAction(policy *Policy, view *View, name, directiveID string) (need protocol.Need, reject string) {
	a, ok := policy.Actions[name]

	switch {
	case !ok:
		return protocol.Need{}, fmt.Sprintf("unknown action %q", Line(name))
	case view.Hold.Active():
		return protocol.Need{}, HoldReason(view.Hold)
	}

	return protocol.Need{
		ID:          protocol.ManualNeedID(directiveID),
		Kind:        protocol.NeedAction,
		DirectiveID: directiveID,
		Action:      &protocol.ActionNeed{Name: name, Impact: a.Impact},
	}, ""
}

// ActionRun is one action a lease runs.
type ActionRun struct {
	// ID is the need served.
	ID   string
	Name string
}

// action validates an action need: the action must be configured, and an
// observed need must still be due. The argv is never taken from the lease.
// A manual need runs the action the agent stored for it.
func (v *validation) action(n *protocol.Need) {
	if n.DirectiveID != "" {
		if i := slices.IndexFunc(v.manual, func(m protocol.Need) bool { return m.ID == n.ID }); i >= 0 {
			n = &v.manual[i]
		}
	}

	if n.Action == nil || n.Action.Name == "" {
		v.verdict(n.ID, protocol.StepRefused, "malformed action need")

		return
	}

	name := n.Action.Name
	a, ok := v.policy.Actions[name]

	switch {
	case !ok:
		v.verdict(n.ID, protocol.StepRefused, fmt.Sprintf("unknown action %q", Line(name)))
	case n.DirectiveID == "" && !ActionDue(&a, v.view.System, v.view.ActionsRan[name]):
		v.verdict(n.ID, protocol.StepSuperseded, "the condition of action "+name+" no longer holds")
	case slices.ContainsFunc(v.plan.Actions, func(r ActionRun) bool { return r.Name == name }):
		v.verdict(n.ID, protocol.StepSuperseded, "another need of this lease runs action "+name)
	default:
		v.plan.Actions = append(v.plan.Actions, ActionRun{ID: n.ID, Name: name})
	}
}
