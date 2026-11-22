package protocol

import (
	"regexp"
	"strings"
)

// Host actions are commands that agent.yaml defines and names. The
// controller only ever names an action; the argv stays on the host.

// VerbAction is the verb of action needs. It is not configured in verbs: a
// host allows it when its agent.yaml defines any action.
const VerbAction Verb = "action"

// NeedAction runs one configured action.
const NeedAction NeedKind = "action"

// DirectiveAction creates a manual action need for Directive.Action.
const DirectiveAction DirectiveKind = "action"

// ActionCondition is a built-in condition under which an action is due by
// itself.
type ActionCondition string

// Action conditions.
const (
	// ConditionSecurityUpdates holds while the package inventory has pending
	// security updates outside the blocked set.
	ConditionSecurityUpdates ActionCondition = "securityUpdates"
)

// Valid reports whether the condition is a known one.
func (c ActionCondition) Valid() bool { return c == ConditionSecurityUpdates }

// Bounds of the action reports.
const (
	// MaxActions bounds the actions of a host.
	MaxActions = 32
	// MaxActionDescription bounds an action's description, in bytes.
	MaxActionDescription = 200
	// MaxActionOutput bounds the output tail of an action run.
	MaxActionOutput = 16 << 10
	// maxActionName bounds an action name (actionNamePattern).
	maxActionName = 63
)

var actionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidActionName reports whether name is an action name: lowercase letters,
// digits, "-" and "_", starting with a letter or a digit, at most 63 bytes.
func ValidActionName(name string) bool {
	return len(name) <= maxActionName && actionNamePattern.MatchString(name)
}

// featurePattern is a feature as a token's services claim grants it:
// "<service>:<feature>".
var featurePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}:[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidFeature reports whether feature is "<service>:<feature>".
func ValidFeature(feature string) bool { return featurePattern.MatchString(feature) }

// SplitFeature splits a valid feature into its service and its name.
func SplitFeature(feature string) (service, name string, ok bool) {
	if !ValidFeature(feature) {
		return "", "", false
	}

	service, name, ok = strings.Cut(feature, ":")

	return service, name, ok
}

// ActionInfo is one configured action as the agent reports it. The command
// is not reported.
type ActionInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Impact      Impact `json:"impact"`
	// Feature is the login feature ("<service>:<feature>") that allows a
	// person to start the action, next to admin.
	Feature string `json:"feature,omitempty"`
	// Teams are the teams whose members may start the action.
	Teams []string `json:"teams,omitempty"`
	// When is the condition under which the action is due by itself; empty
	// for an action that only runs when a person starts it.
	When ActionCondition `json:"when,omitempty"`
	// Due is true while When holds.
	Due bool `json:"due,omitempty"`
}

// ActionNeed asks to run one configured action.
type ActionNeed struct {
	Name   string `json:"name"`
	Impact Impact `json:"impact"`
	// When is the condition that made the need; empty for a manual one.
	When ActionCondition `json:"when,omitempty"`
}

// ActionNeedID is the ID of the need an action's condition makes.
func ActionNeedID(name string) string {
	return "action/" + name
}

// ActionResult is how one action run ended.
type ActionResult struct {
	// ExitCode is the exit status; -1 when the program did not exit by
	// itself (timeout, signal, not started).
	ExitCode int `json:"exitCode"`
	// Output is the tail of the combined standard output and error, at most
	// MaxActionOutput bytes.
	Output string `json:"output,omitempty"`
	// TimedOut is set when the run was ended by its timeout.
	TimedOut bool `json:"timedOut,omitempty"`
	// Unknown is set when the agent restarted while the action ran: its
	// outcome is not known.
	Unknown bool `json:"unknown,omitempty"`
}
