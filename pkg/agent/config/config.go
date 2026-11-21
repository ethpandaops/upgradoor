// Package config is the agent's configuration file, agent.yaml: what the
// host manages and which verbs it allows, decided on the host and never by
// the controller.
package config

import (
	"bytes"
	"crypto/mldsa"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Default file locations.
const (
	DefaultFile       = "/etc/upgradoor/agent.yaml"
	DefaultKeyFile    = "/etc/upgradoor/host.key"
	DefaultCertFile   = "/etc/upgradoor/host.crt"
	DefaultStateDir   = "/var/lib/upgradoor"
	DefaultSocketFile = "/run/upgradoor/agent.sock"

	// DefaultEntryUser is the user whose home directory holds the overrides
	// file and whose group may use the local socket.
	DefaultEntryUser = "devops"

	// OverridesFileName is the name of the overrides file in the entry
	// user's home directory.
	OverridesFileName = "overrides.yaml"
)

// Config is /etc/upgradoor/agent.yaml. It is written by Ansible and never
// changed by the controller: what the agent manages and which verbs it
// allows is decided on the host.
type Config struct {
	Controller Controller `yaml:"controller"`

	KeyFile  string `yaml:"keyFile"`
	CertFile string `yaml:"certFile"`
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
	Socket Socket `yaml:"socket"`
	// AuditLog is the host's own audit log of what users and the local
	// commands asked for (default audit.log in StateDir).
	AuditLog auditlog.Config `yaml:"auditLog"`

	// Verbs are the allowed verbs: any of roll, packages, reboot.
	Verbs []protocol.Verb `yaml:"verbs"`
	// Groups say, per roll group, whose containers they are and what may be
	// overridden. The managed containers themselves are found by their
	// labels (protocol.LabelGroup) or by Containers; a group without an entry
	// here is admin-only: no client, no owners, nothing may be overridden.
	Groups map[string]Group `yaml:"groups"`
	// Containers puts containers in their group by name: for containers
	// deployed before the labels. A label wins over the entry; the next
	// recreate of the container adds the labels from it.
	Containers map[string]MappedContainer `yaml:"containers"`
	// DefaultGroup is what becomes of the containers that neither a label
	// nor Containers puts in a group (protocol.DefaultGroup).
	DefaultGroup DefaultGroup `yaml:"defaultGroup"`
	// Validators says where the host's validator range is read from.
	Validators Validators `yaml:"validators"`

	Docker   Docker   `yaml:"docker"`
	Registry Registry `yaml:"registry"`
	Packages Packages `yaml:"packages"`
	Probes   Probes   `yaml:"probes"`
	Verify   Verify   `yaml:"verify"`
}

// Controller says where and how often to sync.
type Controller struct {
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

// Socket is the local Unix socket of the agent.
type Socket struct {
	// Path is the socket file (default /run/upgradoor/agent.sock).
	Path string `yaml:"path"`
	// Group is the group that may use the socket besides root (default: the
	// primary group of EntryUser). The socket has mode 0660.
	Group string `yaml:"group"`
}

// MaxControllerKeys bounds controller.publicKeys: the keys of a rotation and
// some room. Every key on the list is one more key the host obeys.
const MaxControllerKeys = 4

// ControllerKeys decodes controller.publicKeys. It is empty only with
// controller.allowUnsignedAnswers.
func (c *Controller) ControllerKeys() ([]*mldsa.PublicKey, error) {
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

// Group is the agent.yaml entry of one roll group.
type Group struct {
	// Client is the client software the group's containers run ("geth",
	// "lighthouse"); the consensus group of a beacon node and its validator
	// client names the consensus client. The controller groups hosts by it
	// and takes the image policy of the group's rollouts from it.
	Client string `yaml:"client"`
	// Owners are the teams that own the group's containers: at most
	// protocol.MaxOwners team names. The controller lets a member of an
	// owning team act on them.
	Owners []string `yaml:"owners"`
	// Overrides lists what may be overridden on the group's containers:
	// image, flags or both. It bounds everybody: the controller, the local
	// commands and a hand edit of the overrides file. Empty: the group is
	// tooling and always runs what Ansible deployed.
	Overrides []string `yaml:"overrides"`
	// Impact is the group's impact class: node, slot or independent. Empty:
	// node when the group has an el, cl or vc container on the host,
	// independent otherwise.
	Impact protocol.Impact `yaml:"impact"`
	// StopTimeout is the grace period when stopping a container of the
	// group. Zero: the stop timeout the container was created with, or
	// 300s.
	StopTimeout time.Duration `yaml:"stopTimeout"`
}

// MayOverride reports whether the group lets anybody override a part
// (protocol.OverrideImage, protocol.OverrideFlags).
func (g *Group) MayOverride(part string) bool {
	return slices.Contains(g.Overrides, part)
}

// Group returns the configuration of a roll group; the zero value for a
// group that agent.yaml does not list (admin-only, nothing overridable).
func (c *Config) Group(name string) Group {
	return c.Groups[name]
}

// MappedContainer is the group and kind of a container that carries no
// labels, by its name.
type MappedContainer struct {
	Group string `yaml:"group"`
	// Kind is el, cl, vc or other (default other).
	Kind protocol.ContainerKind `yaml:"kind"`
}

// DefaultGroup says what happens to the containers of protocol.DefaultGroup.
type DefaultGroup struct {
	// Roll lets them follow new digests of their tag (independent impact, no
	// gate). Without it their updates are reported and never rolled.
	Roll bool `yaml:"roll"`
	// Exclude leaves containers out of the default group, and so out of
	// everything: names or path.Match patterns ("tmp-*").
	Exclude []string `yaml:"exclude"`
}

// Excludes reports whether a container of that name is left out of the
// default group.
func (d *DefaultGroup) Excludes(name string) bool {
	return slices.ContainsFunc(d.Exclude, func(pattern string) bool {
		ok, _ := pathpkg.Match(pattern, name)

		return ok
	})
}

// Docker locates the Docker daemon.
type Docker struct {
	// Host is the daemon address (default unix:///var/run/docker.sock).
	Host string `yaml:"host"`
}

// Registry controls registry checks.
type Registry struct {
	// PollInterval is the agent's own check interval per tag (default 30m):
	// used while the controller has not answered for a while (it hands out
	// the lookups otherwise) and for the image of an override. Every check
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

// Packages controls the package layer.
type Packages struct {
	// Blocked are the packages unattended-upgrades must not touch and the
	// agent upgrades under a slot (docker-ce, containerd.io, ...).
	Blocked []string `yaml:"blocked"`
}

// Probes are the local node endpoints. An empty URL disables a probe.
type Probes struct {
	// EL is the execution client JSON-RPC URL, e.g. http://127.0.0.1:8545.
	EL string `yaml:"el"`
	// CL is the beacon API URL, e.g. http://127.0.0.1:5052.
	CL string `yaml:"cl"`
}

// Verify controls the verifying phase.
type Verify struct {
	// Window is how long every touched container must stay running without
	// a restart before the operation is done (default 45s).
	Window time.Duration `yaml:"window"`
	// Timeout bounds the starting and verifying phases (default 10m).
	Timeout time.Duration `yaml:"timeout"`
}

// Load reads, defaults and validates the configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config

	// A key the agent does not know is an error: a misspelt or outdated key
	// (the containers list of earlier builds) must not be ignored.
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
	str(&c.StateDir, DefaultStateDir)
	str(&c.EntryUser, DefaultEntryUser)
	str(&c.OverridesFile, filepath.Join(homeOf(c.EntryUser), OverridesFileName))
	str(&c.Socket.Path, DefaultSocketFile)
	str(&c.AuditLog.File, filepath.Join(c.StateDir, auditlog.FileName))

	if c.AuditLog.MaxSizeBytes <= 0 {
		c.AuditLog.MaxSizeBytes = auditlog.DefaultMaxSize
	}

	if c.AuditLog.MaxFiles <= 0 {
		c.AuditLog.MaxFiles = auditlog.DefaultMaxFiles
	}

	str(&c.Docker.Host, "unix:///var/run/docker.sock")
	def(&c.Registry.PollInterval, 30*time.Minute)
	str(&c.Registry.AuthFile, "/root/.docker/config.json")
	def(&c.Verify.Window, 45*time.Second)
	def(&c.Verify.Timeout, 10*time.Minute)

	for name, m := range c.Containers {
		if m.Kind == "" {
			m.Kind = protocol.KindOther
			c.Containers[name] = m
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

	for name, g := range c.Groups {
		if err := g.validate(name); err != nil {
			return fmt.Errorf("group %q: %w", name, err)
		}
	}

	for name, m := range c.Containers {
		if err := m.validate(name); err != nil {
			return fmt.Errorf("containers: %q: %w", name, err)
		}
	}

	for _, pattern := range c.DefaultGroup.Exclude {
		if _, err := pathpkg.Match(pattern, ""); err != nil || pattern == "" {
			return fmt.Errorf("defaultGroup.exclude: %q is not a name or pattern", pattern)
		}
	}

	return nil
}

// validate checks the name map entry of one container.
func (m *MappedContainer) validate(name string) error {
	if !containerName.MatchString(name) {
		return errors.New("not a container name")
	}

	if !protocol.ValidGroup(m.Group) {
		return fmt.Errorf("group %q is not a group name ([a-z0-9][a-z0-9._-]{0,62})", m.Group)
	}

	if protocol.ReservedGroup(m.Group) {
		return fmt.Errorf("group %q is reserved", m.Group)
	}

	if protocol.ParseKind(string(m.Kind)) != m.Kind {
		return fmt.Errorf("unknown kind %q (el, cl, vc or other)", m.Kind)
	}

	return nil
}

// containerName is the form Docker gives a container name.
var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// validate checks the entry of one group.
func (g *Group) validate(name string) error {
	if !protocol.ValidGroup(name) {
		return errors.New("not a group name ([a-z0-9][a-z0-9._-]{0,62})")
	}

	if protocol.ReservedGroup(name) {
		return errors.New("the name is reserved: the default group cannot be configured")
	}

	if g.Impact != "" && !g.Impact.Valid() {
		return fmt.Errorf("unknown impact %q (node, slot or independent)", g.Impact)
	}

	if g.Client != "" && !protocol.ValidClient(g.Client) {
		return fmt.Errorf("client %q is not a client name ([a-z0-9][a-z0-9._-]{0,62})", g.Client)
	}

	for i, part := range g.Overrides {
		if part != protocol.OverrideImage && part != protocol.OverrideFlags {
			return fmt.Errorf("overrides: unknown part %q (image or flags)", part)
		}

		if slices.Contains(g.Overrides[:i], part) {
			return fmt.Errorf("overrides: %q listed twice", part)
		}
	}

	if g.StopTimeout < 0 {
		return errors.New("stopTimeout is negative")
	}

	return validateOwners(g.Owners)
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

// Overridable is what the group lets anybody override, for the report; nil
// when nothing.
func (g *Group) Overridable() *protocol.Overridable {
	image, flags := g.MayOverride(protocol.OverrideImage), g.MayOverride(protocol.OverrideFlags)
	if !image && !flags {
		return nil
	}

	return &protocol.Overridable{Image: image, Flags: flags}
}

// Allows reports whether the configuration allows a verb.
func (c *Config) Allows(v protocol.Verb) bool { return slices.Contains(c.Verbs, v) }
