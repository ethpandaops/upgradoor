package fleet

import "strings"

// Audit actors that are no person.
const (
	// AuditActorScheduler is the actor of what the scheduler decides itself.
	AuditActorScheduler = "scheduler"
	// AuditAgentPrefix starts the actor of what an agent's sync records:
	// "agent:<host>".
	AuditAgentPrefix = "agent:"
	// AuditSSHPrefix starts the actor of an SSH login group a host reported:
	// "ssh:<identity or key fingerprint>".
	AuditSSHPrefix = "ssh:"
	// AuditTargetSettings is the target of every change of the settings.
	AuditTargetSettings = "settings"
)

// AuditActorKind sorts the actors of the audit trail.
type AuditActorKind string

// Actor kinds. People are everyone but the scheduler, the agents and the SSH
// logins: logins, tokens, "anonymous" of an open API.
const (
	AuditPeople    AuditActorKind = "people"
	AuditScheduler AuditActorKind = "scheduler"
	AuditAgents    AuditActorKind = "agents"
	AuditSSH       AuditActorKind = "ssh"
)

// AuditActorKindOf returns the kind of an actor.
func AuditActorKindOf(actor string) AuditActorKind {
	switch {
	case actor == AuditActorScheduler:
		return AuditScheduler
	case strings.HasPrefix(actor, AuditAgentPrefix):
		return AuditAgents
	case strings.HasPrefix(actor, AuditSSHPrefix):
		return AuditSSH
	default:
		return AuditPeople
	}
}

// AuditGroup is what an audit action is about, for filtering.
type AuditGroup string

// Audit groups.
const (
	AuditGroupRollouts     AuditGroup = "rollouts"
	AuditGroupHostActions  AuditGroup = "host-actions"
	AuditGroupLeases       AuditGroup = "leases"
	AuditGroupHolds        AuditGroup = "holds"
	AuditGroupOverrides    AuditGroup = "overrides"
	AuditGroupInstructions AuditGroup = "instructions"
	AuditGroupSettings     AuditGroup = "settings"
	AuditGroupSources      AuditGroup = "sources"
	AuditGroupHosts        AuditGroup = "hosts"
	AuditGroupAccess       AuditGroup = "access"
	AuditGroupSSH          AuditGroup = "ssh"
)

// AuditTargetKind is what the target of an audit entry names.
type AuditTargetKind string

// Target kinds.
const (
	AuditTargetHost    AuditTargetKind = "host"
	AuditTargetRollout AuditTargetKind = "rollout"
	AuditTargetSetting AuditTargetKind = "settings"
	AuditTargetSources AuditTargetKind = "sources"
	AuditTargetRequest AuditTargetKind = "request"
)

// AuditAction describes one action the controller records.
type AuditAction struct {
	Action string          `json:"action"`
	Group  AuditGroup      `json:"group"`
	Target AuditTargetKind `json:"target"`
	// Scheduler: the scheduler records it on its own, many times over.
	Scheduler bool `json:"scheduler,omitempty"`
}

// AuditActions are every action the controller records. A pause or resume
// of the whole scheduler targets the settings; every entry with that target
// belongs to the settings group and kind, whatever its action.
var AuditActions = []AuditAction{
	{Action: "rollout.created", Group: AuditGroupRollouts, Target: AuditTargetRollout, Scheduler: true},
	{Action: "rollout.paused", Group: AuditGroupRollouts, Target: AuditTargetRollout, Scheduler: true},
	{Action: "rollout.halted", Group: AuditGroupRollouts, Target: AuditTargetRollout, Scheduler: true},
	{Action: "rollout.completed", Group: AuditGroupRollouts, Target: AuditTargetRollout, Scheduler: true},
	{Action: "rollout.superseded", Group: AuditGroupRollouts, Target: AuditTargetRollout, Scheduler: true},
	{Action: "rollout.dropped", Group: AuditGroupRollouts, Target: AuditTargetRollout, Scheduler: true},
	{Action: "action.roll", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.reboot", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.pause", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.resume", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.ignore-gate", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.ignore-budget", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.skip", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.retry", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.abort", Group: AuditGroupRollouts, Target: AuditTargetRollout},
	{Action: "action.host-action", Group: AuditGroupHostActions, Target: AuditTargetRollout},
	{Action: "lease.granted", Group: AuditGroupLeases, Target: AuditTargetHost, Scheduler: true},
	{Action: "lease.released", Group: AuditGroupLeases, Target: AuditTargetHost, Scheduler: true},
	{Action: "lease.failed", Group: AuditGroupLeases, Target: AuditTargetHost, Scheduler: true},
	{Action: "lease.expired", Group: AuditGroupLeases, Target: AuditTargetHost, Scheduler: true},
	{Action: "lease.cancelled", Group: AuditGroupLeases, Target: AuditTargetHost, Scheduler: true},
	{Action: "action.approve-lease", Group: AuditGroupLeases, Target: AuditTargetHost},
	{Action: "action.end-lease", Group: AuditGroupLeases, Target: AuditTargetHost},
	{Action: "action.hold", Group: AuditGroupHolds, Target: AuditTargetHost},
	{Action: "action.unhold", Group: AuditGroupHolds, Target: AuditTargetHost},
	{Action: "action.mark-offline", Group: AuditGroupHolds, Target: AuditTargetHost},
	{Action: "action.unmark-offline", Group: AuditGroupHolds, Target: AuditTargetHost},
	{Action: "action.image-override", Group: AuditGroupOverrides, Target: AuditTargetHost},
	{Action: "action.flag-override", Group: AuditGroupOverrides, Target: AuditTargetHost},
	{Action: "directive.accepted", Group: AuditGroupInstructions, Target: AuditTargetHost, Scheduler: true},
	{Action: "directive.rejected", Group: AuditGroupInstructions, Target: AuditTargetHost, Scheduler: true},
	{Action: "directive.cancelled", Group: AuditGroupInstructions, Target: AuditTargetHost, Scheduler: true},
	{Action: "settings.update", Group: AuditGroupSettings, Target: AuditTargetSetting},
	{Action: "action.client-policy", Group: AuditGroupSettings, Target: AuditTargetSetting},
	{Action: "action.arm-deploy-window", Group: AuditGroupSettings, Target: AuditTargetSetting},
	{Action: "sources.validator-ranges.refresh", Group: AuditGroupSources, Target: AuditTargetSources},
	{Action: "host.registered", Group: AuditGroupHosts, Target: AuditTargetHost},
	{Action: "host.certificate-changed", Group: AuditGroupHosts, Target: AuditTargetHost},
	{Action: "action.forget", Group: AuditGroupHosts, Target: AuditTargetHost},
	{Action: "action.forget.failed", Group: AuditGroupHosts, Target: AuditTargetHost},
	{Action: "auth.refused", Group: AuditGroupAccess, Target: AuditTargetRequest},
	{Action: "debug.pprof", Group: AuditGroupAccess, Target: AuditTargetRequest},
	{Action: AuditActionSSHSession, Group: AuditGroupSSH, Target: AuditTargetHost},
}

// AuditActionSSHSession records one closed SSH login group of a host.
const AuditActionSSHSession = "ssh.session"

// AuditActionOf returns the description of an action.
func AuditActionOf(action string) (AuditAction, bool) {
	for _, a := range AuditActions {
		if a.Action == action {
			return a, true
		}
	}

	return AuditAction{}, false
}

// AuditGroupActions returns the actions of a group, and whether the
// scheduler records any of them on its own.
func AuditGroupActions(group AuditGroup) (actions []string, scheduler bool) {
	actions = make([]string, 0, 8)

	for _, a := range AuditActions {
		if a.Group == group {
			actions = append(actions, a.Action)
			scheduler = scheduler || a.Scheduler
		}
	}

	return actions, scheduler
}

// AuditTargetActions returns the actions whose target is of a kind, and
// whether the scheduler records any of them on its own.
func AuditTargetActions(kind AuditTargetKind) (actions []string, scheduler bool) {
	actions = make([]string, 0, 16)

	for _, a := range AuditActions {
		if a.Target == kind {
			actions = append(actions, a.Action)
			scheduler = scheduler || a.Scheduler
		}
	}

	return actions, scheduler
}
