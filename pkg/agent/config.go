package agent

import (
	"crypto/mldsa"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Default file locations.
const (
	DefaultConfigFile = "/etc/upgradoor/agent.yaml"
	DefaultKeyFile    = "/etc/upgradoor/host.key"
	DefaultCertFile   = "/etc/upgradoor/host.crt"
	DefaultHoldFile   = "/etc/upgradoor/hold"
	DefaultStateDir   = "/var/lib/upgradoor"
	DefaultSocketFile = "/run/upgradoor/agent.sock"
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

	// EntryUser is the user people log in as on the host (default devops).
	// The overrides file lives in its home directory and its group may use
	// the local socket.
	EntryUser string `yaml:"entryUser"`
	// OverridesFile is the host's overrides file (default overrides.yaml in
	// the home directory of EntryUser). It is the only source of overrides.
	OverridesFile string `yaml:"overridesFile"`
	// Socket is the local socket the 'upgradoor host' commands talk to.
	Socket SocketConfig `yaml:"socket"`
	// AuditLog is the host's own audit log of what users and the local
	// commands asked for (default audit.log in StateDir).
	AuditLog AuditLogConfig `yaml:"auditLog"`

	// Verbs are the allowed verbs: any of roll, packages, reboot.
	Verbs []protocol.Verb `yaml:"verbs"`
	// Containers are the managed containers.
	Containers []ManagedContainer `yaml:"containers"`
	// Validators says where the host's validator range is read from.
	Validators ValidatorsConfig `yaml:"validators"`

	Docker   DockerConfig   `yaml:"docker"`
	Registry RegistryConfig `yaml:"registry"`
	Packages PackagesConfig `yaml:"packages"`
	Probes   ProbesConfig   `yaml:"probes"`
	Verify   VerifyConfig   `yaml:"verify"`

	// path is the file the configuration was loaded from; bases the image
	// and command line of every container as last read from it (Reload).
	path  string
	bases *baseTable
}

// ControllerConfig says where and how often to sync.
type ControllerConfig struct {
	// URL is the controller base URL, e.g. https://upgradoor.<network>.
	URL string `yaml:"url"`
	// SyncInterval is the idle sync interval (default 30s).
	SyncInterval time.Duration `yaml:"syncInterval"`
	// LeaseSyncInterval is the interval while holding a lease (default 5s).
	// Both apply until the controller has answered: every answer says when
	// to sync next, which the agent keeps within MinSyncInterval and
	// MaxSyncInterval.
	LeaseSyncInterval time.Duration `yaml:"leaseSyncInterval"`
	// MinSyncInterval and MaxSyncInterval bound the interval the controller
	// asks for (defaults 1s and 10m).
	MinSyncInterval time.Duration `yaml:"minSyncInterval"`
	MaxSyncInterval time.Duration `yaml:"maxSyncInterval"`
	// AllowInsecure permits a plain http controller URL (local development
	// and tests only).
	AllowInsecure bool `yaml:"allowInsecure"`
	// PublicKeys are the controller public keys the agent accepts (unpadded
	// base64url, the output of 'keygen controller-pubkey'). The agent acts
	// only on answers signed by one of them. More than one while the
	// controller key is rotated: the old and the new one.
	PublicKeys []string `yaml:"publicKeys"`
	// AllowUnsignedAnswers runs the agent without controller public keys: it
	// then acts on whatever answers under the controller URL (local
	// development and tests only).
	AllowUnsignedAnswers bool `yaml:"allowUnsignedAnswers"`
}

// SocketConfig is the local Unix socket of the agent.
type SocketConfig struct {
	// Path is the socket file (default /run/upgradoor/agent.sock).
	Path string `yaml:"path"`
	// Group is the group that may use the socket besides root (default: the
	// primary group of EntryUser). The socket has mode 0660.
	Group string `yaml:"group"`
}

// AuditLogConfig is the host's audit log and its rotation.
type AuditLogConfig struct {
	// File is the log file (default audit.log in StateDir).
	File string `yaml:"file"`
	// MaxSizeBytes is the size at which the file is rotated (default 1 MiB).
	MaxSizeBytes int64 `yaml:"maxSizeBytes"`
	// MaxFiles is how many rotated files are kept next to it (default 5).
	MaxFiles int `yaml:"maxFiles"`
}

// MaxControllerKeys bounds controller.publicKeys: the keys of a rotation and
// some room. Every key on the list is one more key the host obeys.
const MaxControllerKeys = 4

// ControllerKeys decodes controller.publicKeys. It is empty only with
// controller.allowUnsignedAnswers.
func (c *ControllerConfig) ControllerKeys() ([]*mldsa.PublicKey, error) {
	switch {
	case len(c.PublicKeys) == 0 && c.AllowUnsignedAnswers:
		return nil, nil
	case len(c.PublicKeys) == 0:
		return nil, errors.New("controller.publicKeys is required: the agent acts only on " +
			"answers signed by the controller key (controller.allowUnsignedAnswers runs " +
			"without, for development)")
	case c.AllowUnsignedAnswers:
		return nil, errors.New(
			"controller.publicKeys and controller.allowUnsignedAnswers exclude each other")
	case len(c.PublicKeys) > MaxControllerKeys:
		return nil, fmt.Errorf("controller.publicKeys lists more than %d keys", MaxControllerKeys)
	}

	keys := make([]*mldsa.PublicKey, 0, len(c.PublicKeys))

	for i, raw := range c.PublicKeys {
		key, err := identity.DecodePublicKey(raw)
		if err != nil {
			return nil, fmt.Errorf("controller.publicKeys[%d]: %w", i, err)
		}

		keys = append(keys, key)
	}

	return keys, nil
}

// ManagedContainer is one container the agent may roll.
type ManagedContainer struct {
	Name string                 `yaml:"name"`
	Kind protocol.ContainerKind `yaml:"kind"`
	// Impact is the container's impact class: node (default for el, cl and
	// vc), slot or independent (default for other). It decides how the
	// controller schedules work on the container.
	Impact protocol.Impact `yaml:"impact"`
	// Image is the image reference Ansible deployed the container with. A
	// container that runs another one (and no override says so) is rolled
	// onto it under a lease: the soft deploy.
	Image string `yaml:"image"`
	// Command is the command line (the arguments after the entrypoint)
	// Ansible deployed the container with; absent keeps the one the
	// container runs. Like Image, a change is rolled out under a lease.
	Command []string `yaml:"command"`
	// StopTimeout is the grace period when stopping (default 300s).
	StopTimeout time.Duration `yaml:"stopTimeout"`
	// Owners are the teams that own the container: at most
	// protocol.MaxOwners team names. The agent reports them; the controller
	// lets a member of an owning team act on the container.
	Owners []string `yaml:"owners"`
	// Client is the client software the container runs ("geth",
	// "lighthouse"); a validator client names its consensus client. The
	// controller groups hosts by it. Containers of kind el, cl and vc only.
	Client string `yaml:"client"`
	// Overrides says what the controller may override on this container.
	// The zero value is nothing. What a person writes into the overrides
	// file by hand is not bounded by it: whoever can edit that file has
	// SSH.
	Overrides OverridePolicy `yaml:"overrides"`
}

// OverridePolicy is the host's own bound on overrides that arrive from the
// controller.
type OverridePolicy struct {
	// Images are the image repositories an image override may name
	// ("ethpandaops/geth", "ghcr.io/ethereum/*"; "*" stands for any run of
	// characters without a slash). Empty: no image overrides.
	Images []string `yaml:"images"`
	// Flags allows flag overrides.
	Flags bool `yaml:"flags"`
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

	cfg.path = path

	if info, err := os.Stat(path); err == nil {
		cfg.bases = newBaseTable(cfg.Containers, fileStamp{size: info.Size(), modTime: info.ModTime()})
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
	def(&c.Controller.MinSyncInterval, time.Second)
	def(&c.Controller.MaxSyncInterval, 10*time.Minute)
	str(&c.KeyFile, DefaultKeyFile)
	str(&c.CertFile, DefaultCertFile)
	str(&c.HoldFile, DefaultHoldFile)
	str(&c.StateDir, DefaultStateDir)
	str(&c.EntryUser, DefaultEntryUser)
	str(&c.OverridesFile, filepath.Join(homeOf(c.EntryUser), overridesFileName))
	str(&c.Socket.Path, DefaultSocketFile)
	str(&c.AuditLog.File, filepath.Join(c.StateDir, auditLogName))

	if c.AuditLog.MaxSizeBytes <= 0 {
		c.AuditLog.MaxSizeBytes = defaultAuditLogSize
	}

	if c.AuditLog.MaxFiles <= 0 {
		c.AuditLog.MaxFiles = defaultAuditLogFiles
	}

	str(&c.Docker.Host, "unix:///var/run/docker.sock")
	def(&c.Registry.PollInterval, 5*time.Minute)
	str(&c.Registry.AuthFile, "/root/.docker/config.json")
	def(&c.Verify.Window, 45*time.Second)
	def(&c.Verify.Timeout, 10*time.Minute)

	for i := range c.Containers {
		def(&c.Containers[i].StopTimeout, 300*time.Second)

		if c.Containers[i].Kind == "" {
			c.Containers[i].Kind = protocol.KindOther
		}

		if c.Containers[i].Impact == "" {
			c.Containers[i].Impact = protocol.DefaultImpact(c.Containers[i].Kind)
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

	if _, err := c.Controller.ControllerKeys(); err != nil {
		return err
	}

	if err := c.Validators.validate(); err != nil {
		return err
	}

	for _, v := range c.Verbs {
		if v != protocol.VerbRoll && v != protocol.VerbPackages && v != protocol.VerbReboot {
			return fmt.Errorf("unknown verb %q", v)
		}
	}

	seen := make(map[string]struct{}, len(c.Containers))

	for i := range c.Containers {
		m := c.Containers[i]

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

		if !m.Impact.Valid() {
			return fmt.Errorf("container %q: unknown impact %q (node, slot or independent)", m.Name, m.Impact)
		}

		if err := validateOwners(m.Owners); err != nil {
			return fmt.Errorf("container %q: %w", m.Name, err)
		}

		if err := validateClient(&m); err != nil {
			return fmt.Errorf("container %q: %w", m.Name, err)
		}

		for _, pattern := range m.Overrides.Images {
			if err := validateRepositoryPattern(pattern); err != nil {
				return fmt.Errorf("container %q: overrides.images: %w", m.Name, err)
			}
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

// homeOf returns the home directory of a user: what the system says, or
// /home/<name> for a user it does not know.
func homeOf(name string) string {
	if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}

	return "/home/" + name
}

// overridable is what the configuration lets the controller override on a
// container, for the report; nil when nothing.
func (m *ManagedContainer) overridable() *protocol.Overridable {
	if len(m.Overrides.Images) == 0 && !m.Overrides.Flags {
		return nil
	}

	return &protocol.Overridable{Images: slices.Clone(m.Overrides.Images), Flags: m.Overrides.Flags}
}

// validateClient checks the client of one managed container.
func validateClient(m *ManagedContainer) error {
	switch {
	case m.Client == "":
		return nil
	case !protocol.ValidClient(m.Client):
		return fmt.Errorf("client %q is not a client name ([a-z0-9][a-z0-9._-]{0,62})", m.Client)
	case m.Kind == protocol.KindOther:
		return errors.New("a client needs the kind el, cl or vc")
	default:
		return nil
	}
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
