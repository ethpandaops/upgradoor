package host

import (
	"github.com/ethpandaops/upgradoor/pkg/agent/host/docker"
	"github.com/ethpandaops/upgradoor/pkg/agent/host/probe"
	"github.com/ethpandaops/upgradoor/pkg/agent/host/system"
)

// Config is host's part of agent.yaml.
type Config struct {
	Docker docker.Config `yaml:"docker"`
	Probes probe.Config  `yaml:"probes"`
	// Commands are run for the package upgrade and the reboot when set.
	Commands system.Commands `yaml:"commands"`
}

// ApplyDefaults fills unset fields of the docker section.
func (c *Config) ApplyDefaults() {
	c.Docker.ApplyDefaults()
}

// Validate checks the probes and the commands.
func (c *Config) Validate() error {
	if err := c.Docker.Validate(); err != nil {
		return err
	}

	if err := c.Probes.Validate(); err != nil {
		return err
	}

	return c.Commands.Validate()
}
