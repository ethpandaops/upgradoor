package agent

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Default file locations.
const (
	DefaultConfigFile = "/etc/upgradoor/agent.yaml"
	DefaultKeyFile    = "/etc/upgradoor/host.key"
	DefaultCertFile   = "/etc/upgradoor/host.crt"
	DefaultHoldFile   = "/etc/upgradoor/hold"
	DefaultStateDir   = "/var/lib/upgradoor"
)

// Config is /etc/upgradoor/agent.yaml. It is written by Ansible and never
// changed by the controller: what the agent manages and which verbs it
// allows is decided on the host.
type Config struct {
	Controller ControllerConfig `yaml:"controller"`

	KeyFile  string `yaml:"keyFile"`
	CertFile string `yaml:"certFile"`
	// HoldFile: while this file exists the agent reports no needs and
	// refuses all work.
	HoldFile string `yaml:"holdFile"`
	// StateDir holds state.json (lease, operation, handled directives).
	StateDir string `yaml:"stateDir"`

	// Verbs are the allowed verbs: any of roll, packages, reboot.
	Verbs []protocol.Verb `yaml:"verbs"`
	// Containers are the managed containers.
	Containers []ManagedContainer `yaml:"containers"`

	Docker   DockerConfig   `yaml:"docker"`
	Registry RegistryConfig `yaml:"registry"`
	Packages PackagesConfig `yaml:"packages"`
	Probes   ProbesConfig   `yaml:"probes"`
	Verify   VerifyConfig   `yaml:"verify"`
}

// ControllerConfig says where and how often to sync.
type ControllerConfig struct {
	// URL is the controller base URL, e.g. https://upgradoor.<network>.
	URL string `yaml:"url"`
	// SyncInterval is the idle sync interval (default 30s).
	SyncInterval time.Duration `yaml:"syncInterval"`
	// LeaseSyncInterval is the interval while holding a lease (default 5s).
	LeaseSyncInterval time.Duration `yaml:"leaseSyncInterval"`
	// AllowInsecure permits a plain http controller URL (local development
	// and tests only).
	AllowInsecure bool `yaml:"allowInsecure"`
}

// ManagedContainer is one container the agent may roll.
type ManagedContainer struct {
	Name string                 `yaml:"name"`
	Kind protocol.ContainerKind `yaml:"kind"`
	// Image is the image reference Ansible deployed. A container running
	// another reference is "custom" and is never rolled.
	Image string `yaml:"image"`
	// StopTimeout is the grace period when stopping (default 300s).
	StopTimeout time.Duration `yaml:"stopTimeout"`
	// Owners are the teams that own the container: at most
	// protocol.MaxOwners team names. The agent reports them; the controller
	// lets a member of an owning team act on the container.
	Owners []string `yaml:"owners"`
}

// DockerConfig locates the Docker daemon.
type DockerConfig struct {
	// Host is the daemon address (default unix:///var/run/docker.sock).
	Host string `yaml:"host"`
}

// RegistryConfig controls registry checks.
type RegistryConfig struct {
	// PollInterval is the check interval per tag (default 5m); every check
	// is jittered by up to 20%.
	PollInterval time.Duration `yaml:"pollInterval"`
	// AuthFile is a Docker config.json with registry credentials (default
	// /root/.docker/config.json; a missing file means anonymous).
	AuthFile string `yaml:"authFile"`
	// PlainHTTP lists registry hosts to talk to without TLS.
	PlainHTTP []string `yaml:"plainHttp"`
	// CAFile is an extra CA bundle for private registries.
	CAFile string `yaml:"caFile"`
}

// PackagesConfig controls the package layer.
type PackagesConfig struct {
	// Blocked are the packages unattended-upgrades must not touch and the
	// agent upgrades under a slot (docker-ce, containerd.io, ...).
	Blocked []string `yaml:"blocked"`
	// InventoryInterval is how often the installed and upgradable lists
	// are re-read (default 15m). The agent never runs "apt-get update" by
	// itself outside a packages slot; the system's apt timers do.
	InventoryInterval time.Duration `yaml:"inventoryInterval"`
}

// ProbesConfig are the local node endpoints. An empty URL disables a probe.
type ProbesConfig struct {
	// EL is the execution client JSON-RPC URL, e.g. http://127.0.0.1:8545.
	EL string `yaml:"el"`
	// CL is the beacon API URL, e.g. http://127.0.0.1:5052.
	CL string `yaml:"cl"`
}

// VerifyConfig controls the verifying phase.
type VerifyConfig struct {
	// Window is how long every touched container must stay running without
	// a restart before the operation is done (default 45s).
	Window time.Duration `yaml:"window"`
	// Timeout bounds the starting and verifying phases (default 10m).
	Timeout time.Duration `yaml:"timeout"`
}

// LoadConfig reads, defaults and validates the configuration file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config

	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg.ApplyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	return &cfg, nil
}

// ApplyDefaults fills unset fields.
func (c *Config) ApplyDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	str := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}

	def(&c.Controller.SyncInterval, 30*time.Second)
	def(&c.Controller.LeaseSyncInterval, 5*time.Second)
	str(&c.KeyFile, DefaultKeyFile)
	str(&c.CertFile, DefaultCertFile)
	str(&c.HoldFile, DefaultHoldFile)
	str(&c.StateDir, DefaultStateDir)
	str(&c.Docker.Host, "unix:///var/run/docker.sock")
	def(&c.Registry.PollInterval, 5*time.Minute)
	str(&c.Registry.AuthFile, "/root/.docker/config.json")
	def(&c.Packages.InventoryInterval, 15*time.Minute)
	def(&c.Verify.Window, 45*time.Second)
	def(&c.Verify.Timeout, 10*time.Minute)

	for i := range c.Containers {
		def(&c.Containers[i].StopTimeout, 300*time.Second)

		if c.Containers[i].Kind == "" {
			c.Containers[i].Kind = protocol.KindOther
		}
	}
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	u, err := url.Parse(c.Controller.URL)
	if err != nil || u.Host == "" {
		return errors.New("controller.url must be an absolute URL")
	}

	if u.Scheme != "https" && (u.Scheme != "http" || !c.Controller.AllowInsecure) {
		return errors.New("controller.url must be https (or http with controller.allowInsecure)")
	}

	for _, v := range c.Verbs {
		if v != protocol.VerbRoll && v != protocol.VerbPackages && v != protocol.VerbReboot {
			return fmt.Errorf("unknown verb %q", v)
		}
	}

	seen := make(map[string]struct{}, len(c.Containers))

	for _, m := range c.Containers {
		if m.Name == "" || m.Image == "" {
			return errors.New("containers need a name and an image")
		}

		if _, dup := seen[m.Name]; dup {
			return fmt.Errorf("container %q listed twice", m.Name)
		}

		seen[m.Name] = struct{}{}

		kinds := []protocol.ContainerKind{
			protocol.KindEL, protocol.KindCL, protocol.KindVC, protocol.KindOther,
		}
		if !slices.Contains(kinds, m.Kind) {
			return fmt.Errorf("container %q: unknown kind %q", m.Name, m.Kind)
		}

		if err := validateOwners(m.Owners); err != nil {
			return fmt.Errorf("container %q: %w", m.Name, err)
		}
	}

	return nil
}

// validateOwners checks the owners of one managed container.
func validateOwners(owners []string) error {
	if len(owners) > protocol.MaxOwners {
		return fmt.Errorf("more than %d owners", protocol.MaxOwners)
	}

	for i, name := range owners {
		if !protocol.ValidOwner(name) {
			return fmt.Errorf("owner %q is not a team name ([a-z0-9][a-z0-9._-]{0,62})", name)
		}

		if slices.Contains(owners[:i], name) {
			return fmt.Errorf("owner %q listed twice", name)
		}
	}

	return nil
}

// Allows reports whether the configuration allows a verb.
func (c *Config) Allows(v protocol.Verb) bool { return slices.Contains(c.Verbs, v) }

// Managed returns the configuration of a managed container, or nil.
func (c *Config) Managed(name string) *ManagedContainer {
	for i := range c.Containers {
		if c.Containers[i].Name == name {
			return &c.Containers[i]
		}
	}

	return nil
}
