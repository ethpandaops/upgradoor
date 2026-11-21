package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/lease"
	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/agent/registry"
)

// Default file locations.
const (
	DefaultConfigFile = "/etc/upgradoor/agent.yaml"
	DefaultKeyFile    = "/etc/upgradoor/host.key"
	DefaultCertFile   = "/etc/upgradoor/host.crt"
	DefaultStateDir   = "/var/lib/upgradoor"
)

// Config is agent.yaml. Each section is defined by the package that uses it;
// Local, Policy and Host are inline, so their keys are top-level keys.
type Config struct {
	Controller link.Config `yaml:"controller"`

	KeyFile  string `yaml:"keyFile"`
	CertFile string `yaml:"certFile"`
	// StateDir holds state.json and, by default, the audit log.
	StateDir string `yaml:"stateDir"`

	Local  local.Config `yaml:",inline"`
	Policy plan.Policy  `yaml:",inline"`
	Host   host.Config  `yaml:",inline"`

	Registry registry.Config `yaml:"registry"`
	Verify   lease.Config    `yaml:"verify"`
}

// LoadConfig reads agent.yaml (unknown keys are an error), applies the
// defaults and validates it.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg.ApplyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	return &cfg, nil
}

// ApplyDefaults fills unset fields of every section; a duration counts as
// unset when it is not positive.
func (c *Config) ApplyDefaults() {
	str := func(field *string, def string) {
		if *field == "" {
			*field = def
		}
	}

	str(&c.KeyFile, DefaultKeyFile)
	str(&c.CertFile, DefaultCertFile)
	str(&c.StateDir, DefaultStateDir)

	c.Controller.ApplyDefaults()
	c.Local.ApplyDefaults(c.StateDir)
	c.Policy.ApplyDefaults()
	c.Host.ApplyDefaults()
	c.Registry.ApplyDefaults()
	c.Verify.ApplyDefaults()
}

// Validate checks the controller section, the validator range and the
// policy, in that order.
func (c *Config) Validate() error {
	if err := c.Controller.Validate(); err != nil {
		return err
	}

	if err := c.Host.Validate(); err != nil {
		return err
	}

	return c.Policy.Validate()
}

// defaulted reports whether ApplyDefaults ran.
func (c *Config) defaulted() bool {
	return c.StateDir != "" && c.Local.OverridesFile != "" && c.Local.Socket.Path != "" &&
		c.Controller.SyncInterval > 0 && c.Controller.LeaseSyncInterval > 0 &&
		c.Controller.MinSyncInterval > 0 && c.Controller.MaxSyncInterval >= c.Controller.MinSyncInterval &&
		c.Registry.AuthFile != "" && c.Verify.Window > 0 && c.Verify.Timeout > 0
}
