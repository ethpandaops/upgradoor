package host

import (
	"context"
	"errors"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host/docker"
	"github.com/ethpandaops/upgradoor/pkg/agent/host/probe"
	"github.com/ethpandaops/upgradoor/pkg/agent/host/system"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// ErrDigestMismatch is returned by Host.Recreate, before anything is stopped,
// when the local image does not have the granted digest.
var ErrDigestMismatch = docker.ErrDigestMismatch

// ErrImageMoved is returned by Host.Recreate when the tag moved after the old
// container was stopped; the rollback has run.
var ErrImageMoved = docker.ErrImageMoved

// Host is the machine; the contracts are on docker.Runtime, system.System
// and probe.Prober.
type Host interface {
	// Containers lists all containers.
	Containers(ctx context.Context) ([]protocol.Container, error)
	// LocalImage returns the digest and revision of a local image.
	LocalImage(ctx context.Context, ref string) (digest, revision string, err error)
	// Pull pulls an image by its tag.
	Pull(ctx context.Context, ref string) (digest, revision string, err error)
	// Recreate replaces a container, with change applied.
	Recreate(
		ctx context.Context, name, digest string, stopTimeout time.Duration, start bool, change *Change,
	) error
	// Stop stops a container.
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Start starts a container.
	Start(ctx context.Context, name string) error

	// System returns the cheap, frequently read part of the OS state.
	System(ctx context.Context) (System, error)
	// Inventory returns the full package inventory.
	Inventory(ctx context.Context) (*protocol.Inventory, error)
	// UpgradePackages upgrades exactly the named blocked packages.
	UpgradePackages(ctx context.Context, names []string) error
	// Reboot reboots the host.
	Reboot(ctx context.Context) error

	// Probe checks the local EL and CL.
	Probe(ctx context.Context) protocol.NodeHealth
	// Validators reads the host's validator range.
	Validators(ctx context.Context) *protocol.Validators

	// Close releases the Docker client.
	Close() error
}

// Change is what a recreate changes about a container.
type Change = docker.Change

// System is the cheap, frequently read part of the OS state.
type System = system.Status

// Login returns the credentials for a registry host, for pulls.
type Login = docker.Login

// New returns the machine. blocked is the configured blocked package set; a
// name that is not a plain package name is dropped with a warning.
func New(log logrus.FieldLogger, cfg *Config, blocked []string, login Login) (Host, error) {
	if log == nil {
		return nil, errors.New("host: logger is required")
	}

	runtime, err := docker.New(log, cfg.Docker.Host, login)
	if err != nil {
		return nil, err
	}

	return &machine{
		Runtime:    runtime,
		system:     system.New(log, blocked, cfg.Commands),
		Prober:     probe.New(log, &cfg.Probes),
		validators: cfg.Validators,
	}, nil
}

// machine joins Docker, the operating system and the probes into one Host.
type machine struct {
	docker.Runtime
	probe.Prober

	// system is a named field: its Status is Host's System.
	system system.System

	validators Validators
}

var _ Host = (*machine)(nil)

// System implements Host.
func (m *machine) System(ctx context.Context) (System, error) {
	return m.system.Status(ctx)
}

// Inventory implements Host.
func (m *machine) Inventory(ctx context.Context) (*protocol.Inventory, error) {
	return m.system.Inventory(ctx)
}

// UpgradePackages implements Host.
func (m *machine) UpgradePackages(ctx context.Context, names []string) error {
	return m.system.UpgradePackages(ctx, names)
}

// Reboot implements Host.
func (m *machine) Reboot(ctx context.Context) error {
	return m.system.Reboot(ctx)
}

// Validators implements Host.
func (m *machine) Validators(context.Context) *protocol.Validators {
	return ReadValidators(&m.validators)
}
