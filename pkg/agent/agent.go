// Package agent is the per-host upgradoor agent: it observes the host, syncs
// with the controller and serves leases with a closed set of verbs.
//
// The agent opens no port. It can do exactly three things, each only under a
// lease and only when the local configuration allows the verb and no hold is
// in effect:
//
//  1. roll: recreate an existing managed container (it never creates one) on
//     the digest its own tag points at, or with the image and command line
//     agent.yaml and the overrides file say, after verifying that the pulled digest equals the
//     digest in the lease;
//  2. packages: apt-get install --only-upgrade for the configured blocked
//     packages;
//  3. reboot: stop VC, CL, EL in that order, then systemctl reboot.
//
// # Operation order
//
// One lease serves all granted needs in one downtime:
//
//	draining   only with a reboot need: stop managed containers, VC then CL
//	           then EL (then other), each with its stop timeout
//	applying   upgrade packages (if granted), then for every image need
//	           recreate the container, EL then CL then VC then other; with a
//	           reboot need the new containers are created but left stopped
//	rebooting  only with a reboot need: persist the phase, then reboot. After
//	           the boot the agent finds the phase in its state file, checks
//	           that the boot id changed and continues with starting
//	starting   make sure every managed container that should be running is
//	           running again, EL then CL then VC then other. That is every
//	           container that ran before the operation and every one that had
//	           stopped by itself with an error (protocol.Container.ShouldRun):
//	           a newer image repairs a container that crashed on a broken one.
//	           A container somebody stopped stays down, also when it is rolled
//	verifying  every touched container stays running without a restart for
//	           verify.window; bounded by verify.timeout
//	done / failed
//
// A step whose need no longer applies is "superseded", one that local policy
// forbids is "refused"; neither disrupts anything nor fails the operation.
// If any step fails the agent still runs starting so the host comes back as
// far as it can, then reports failed. The agent never rolls back.
package agent

import (
	"context"
)

// Service is the agent's main loop.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
}
