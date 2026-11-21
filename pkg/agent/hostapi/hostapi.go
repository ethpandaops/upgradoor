// Package hostapi is what the agent needs of the host it runs on: the
// container runtime (pkg/agent/docker), the OS (pkg/agent/system), the
// registry (pkg/agent/registry) and the local nodes (pkg/agent/probe).
// pkg/agent/fakehost stands in for runtime and OS in development and tests.
package hostapi

import (
	"context"
	"errors"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// ErrDigestMismatch is returned by ContainerRuntime.Recreate when the local
// image for the container's tag is not the wanted digest.
var ErrDigestMismatch = errors.New("agent: local image digest differs from the lease digest")

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
