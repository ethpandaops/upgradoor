// Package agent is the per-host upgradoor agent: it observes the host,
// syncs with the controller, applies the controller's directives, hands its
// leases to the lease runner and serves the CLI socket.
//
// It decides nothing and changes nothing on the machine: decisions are
// plan's, the machine is changed by lease only, the overrides file is
// written through local.Files after plan. Nothing of a refused answer is
// acted on, and an answer's directives are applied before its lease.
package agent
