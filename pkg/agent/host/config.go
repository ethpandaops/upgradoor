package host

import (
	"errors"
	"fmt"

	"github.com/ethpandaops/upgradoor/pkg/agent/host/docker"
	"github.com/ethpandaops/upgradoor/pkg/agent/host/probe"
	"github.com/ethpandaops/upgradoor/pkg/agent/host/system"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Config is host's part of agent.yaml.
type Config struct {
	Docker     docker.Config `yaml:"docker"`
	Probes     probe.Config  `yaml:"probes"`
	Validators Validators    `yaml:"validators"`
	// Commands are run for the package upgrade and the reboot when set.
	Commands system.Commands `yaml:"commands"`
}

// Validators says where the host's validator range is read from.
type Validators struct {
	// MarkerFile holds the half-open range "<start>-<end>"; when set it alone decides.
	MarkerFile string `yaml:"markerFile"`
	// Start and End are a static half-open range, used without a MarkerFile.
	Start uint64 `yaml:"start"`
	End   uint64 `yaml:"end"`
}

// ApplyDefaults fills unset fields of the docker section.
func (c *Config) ApplyDefaults() {
	c.Docker.ApplyDefaults()
}

// Validate checks the commands and the static validator range.
func (c *Config) Validate() error {
	if err := c.Commands.Validate(); err != nil {
		return err
	}

	v := &c.Validators

	if err := protocol.CheckValidatorRange(v.Start, v.End); err != nil {
		return fmt.Errorf("validators: %w", err)
	}

	if v.MarkerFile == "" && v.Start == v.End && v.End != 0 {
		return errors.New("validators: start and end are equal: leave both out for no validators")
	}

	return nil
}
