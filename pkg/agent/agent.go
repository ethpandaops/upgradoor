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
	"encoding/json"
	"errors"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// ErrDigestMismatch is returned by ContainerRuntime.Recreate when the local
// image for the container's tag is not the wanted digest.
var ErrDigestMismatch = errors.New("agent: local image digest differs from the lease digest")

// Service is the agent's main loop.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
}

// ContainerRuntime is the agent's view of Docker (pkg/agent/docker).
type ContainerRuntime interface {
	// Observe lists all containers (running or not). It fills every field
	// the daemon knows; Managed, Kind, ConfiguredImage and Registry
	// are left for the caller.
	Observe(ctx context.Context) ([]protocol.Container, error)
	// LocalImage describes the local image that ref (a tag reference)
	// currently names: its registry manifest digest, "" when the image is
	// absent or has no digest for that repository, and its revision label
	// (org.opencontainers.image.revision), "" when it carries none.
	LocalImage(ctx context.Context, ref string) (digest, revision string, err error)
	// Pull pulls ref and returns what LocalImage then would. Pulling does
	// not touch running containers.
	Pull(ctx context.Context, ref string) (digest, revision string, err error)
	// Recreate replaces the container by one with identical configuration
	// created from the local image its own image reference now names. It
	// fails with ErrDigestMismatch, before stopping anything, unless digest
	// is a registry digest of that image. With start false the new
	// container is created but not started.
	Recreate(ctx context.Context, name, digest string, stopTimeout time.Duration, start bool) error
	// RecreateAs is Recreate with a change: the replacement is created from
	// another image reference, with another command line, or with other
	// labels. The digest is then verified against the image the replacement
	// is created from. The zero Recreation changes nothing.
	RecreateAs(
		ctx context.Context, name, digest string, stopTimeout time.Duration, start bool,
		change Recreation,
	) error
	// Stop stops a container, waiting up to timeout before killing it. A
	// container that is not running is not an error.
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Start starts a container. A running container is not an error.
	Start(ctx context.Context, name string) error
}

// Recreation is what a recreate changes about a container. Everything it
// does not name stays as the existing recreate keeps it.
type Recreation struct {
	// Image is the image reference of the replacement; empty keeps the
	// container's own.
	Image string
	// Command replaces the command line (the arguments after the
	// entrypoint) when SetCommand is true.
	Command    []string
	SetCommand bool
	// Labels sets the labels with a value and removes those with nil.
	Labels map[string]*string
}

// Zero reports whether the recreation changes nothing.
func (r *Recreation) Zero() bool {
	return r.Image == "" && !r.SetCommand && len(r.Labels) == 0
}

// BaseFromLabels reads what a container was before an override from its
// labels; nil when it carries none of them.
func BaseFromLabels(labels map[string]string) *protocol.OverrideBase {
	image := labels[LabelOverrideImage]
	raw, hasCommand := labels[LabelOverrideCommand]

	if image == "" && !hasCommand {
		return nil
	}

	base := &protocol.OverrideBase{Image: image}

	if hasCommand {
		// A label that does not read as a list of strings is no record.
		if err := json.Unmarshal([]byte(raw), &base.Command); err == nil {
			base.HasCommand = true
		}
	}

	if base.Image == "" && !base.HasCommand {
		return nil
	}

	return base
}

// Registry resolves tags (pkg/agent/registry).
type Registry interface {
	// Resolve returns the manifest digest ref points at (the index digest
	// for multi-arch tags).
	Resolve(ctx context.Context, ref string) (string, error)
}

// System is the agent's view of the OS (pkg/agent/system).
type System interface {
	// Status returns the cheap, frequently read part of the OS state.
	Status(ctx context.Context) (*SystemStatus, error)
	// Inventory returns the full package inventory with its hash set.
	Inventory(ctx context.Context) (*protocol.Inventory, error)
	// UpgradePackages runs apt-get update and then
	// apt-get install --only-upgrade for exactly the named packages, each
	// of which must be in the configured blocked set.
	UpgradePackages(ctx context.Context, names []string) error
	// Reboot issues systemctl reboot and returns once it is accepted.
	Reboot(ctx context.Context) error
}

// SystemStatus is the result of System.Status.
type SystemStatus struct {
	BootID  string
	Uptime  time.Duration
	OS      protocol.OSSummary
	Pending protocol.Pending
	// Blocked lists the configured blocked packages that have a newer
	// candidate version.
	Blocked []protocol.PackageChange
	// InventoryHash is the hash of the inventory Inventory would return
	// now (cached; recomputed every 15 minutes).
	InventoryHash string
}

// Prober checks the local EL and CL (pkg/agent/probe).
type Prober interface {
	Probe(ctx context.Context) protocol.NodeHealth
}
