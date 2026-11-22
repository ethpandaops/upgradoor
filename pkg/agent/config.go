package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/lease"
	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
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
	// Roles are the host's roles, reported to the controller.
	Roles []string `yaml:"roles"`
	// StateDir holds state.json and, by default, the audit log.
	StateDir string `yaml:"stateDir"`

	Local  local.Config `yaml:",inline"`
	Policy plan.Policy  `yaml:",inline"`
	Host   host.Config  `yaml:",inline"`

	Verify lease.Config `yaml:"verify"`
}

// removedKeys are keys of earlier agent.yaml files, with what took their
// place; the strict parser would only call them unknown.
var removedKeys = map[string]string{
	"groups":       "containers are configured by their labels " + labelNames,
	"containers":   "containers are configured by their labels " + labelNames,
	"defaultGroup": "defaultPolicy (ignore or roll) handles unlabelled containers",
	"validators":   "the controller reads the validators from the inventory",
	"registry":     "the pull credentials are docker.authFile",
}

const labelNames = "(" + protocol.LabelKind + ", " + protocol.LabelTeam + ")"

// rejectRemoved fails on a key that agent.yaml no longer has.
func rejectRemoved(raw []byte) error {
	var doc map[string]yaml.Node

	if err := yaml.Unmarshal(raw, &doc); err != nil {
		// The strict decode names the error.
		return nil //nolint:nilerr // reported by the decode that follows
	}

	for _, key := range slices.Sorted(maps.Keys(removedKeys)) {
		if _, ok := doc[key]; ok {
			return fmt.Errorf("%s: no longer supported: %s", key, removedKeys[key])
		}
	}

	var fallback map[string]yaml.Node

	if node, ok := doc["fallback"]; ok && node.Decode(&fallback) == nil {
		if _, ok := fallback["containers"]; ok {
			return errors.New("fallback.containers: no longer supported: " +
				"kindPolicies and defaultPolicy say what the fallback rolls")
		}
	}

	return nil
}

// LoadConfig reads agent.yaml (unknown keys are an error), applies the
// defaults and validates it.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := rejectRemoved(raw); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
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
	c.Verify.ApplyDefaults()
}

// Validate checks the controller section, the roles, the host section and
// the policy, in that order.
func (c *Config) Validate() error {
	if err := c.Controller.Validate(); err != nil {
		return err
	}

	if len(c.Roles) > protocol.MaxRoles {
		return fmt.Errorf("roles: %d, at most %d", len(c.Roles), protocol.MaxRoles)
	}

	for _, role := range c.Roles {
		if err := identity.ValidateName(role); err != nil {
			return fmt.Errorf("roles: %w", err)
		}
	}

	if err := c.Host.Validate(); err != nil {
		return err
	}

	return c.Policy.Validate()
}

// defaulted reports whether ApplyDefaults ran.
func (c *Config) defaulted() bool {
	return c.StateDir != "" && c.Local.ManagedFile != "" && c.Local.Socket.Path != "" &&
		c.Controller.SyncInterval > 0 && c.Controller.LeaseSyncInterval > 0 &&
		c.Controller.MinSyncInterval > 0 && c.Controller.MaxSyncInterval >= c.Controller.MinSyncInterval &&
		c.Host.Docker.AuthFile != "" && c.Verify.Window > 0 && c.Verify.Timeout > 0
}
