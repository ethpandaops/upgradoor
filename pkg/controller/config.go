package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/controller/api"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/upstream"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// Config is the controller configuration, read from a YAML file
// (LoadConfig). The controller holds one secret, the controller seed, which
// is only ever read from a file: inline or in the environment it would show
// in the pod specification.
type Config struct {
	// Server is the HTTP server.
	Server ServerConfig `yaml:"server"`
	// Network is the devnet the controller watches.
	Network NetworkConfig `yaml:"network"`
	// Agents is how agents are verified and answered.
	Agents AgentsConfig `yaml:"agents"`
	// Auth is how users log in.
	Auth AuthConfig `yaml:"auth"`
	// UI configures the web UI and what is served to its logins.
	UI UIConfig `yaml:"ui"`
	// Images is how the image tags and source branches are looked up.
	Images ImagesConfig `yaml:"images"`
	// Store is the database.
	Store StoreConfig `yaml:"store"`
	// DeployWindow is the length of the initial deployment window from the
	// first start on a fresh store; 0 is no window.
	DeployWindow Duration `yaml:"deployWindow"`
	// SettingDefaults override the built-in setting defaults; keys are the
	// JSON names of fleet.Settings.
	SettingDefaults map[string]any `yaml:"settingDefaults"`

	// Version is the build version.
	Version string `yaml:"-"`
	// WebFS is the built web UI.
	WebFS fs.FS `yaml:"-"`
}

// ServerConfig is the HTTP server.
type ServerConfig struct {
	// Listen is the listen address (API, UI, /metrics, /healthz).
	Listen string `yaml:"listen"`
	// PublicURLs are the base URLs agents reach the controller under.
	PublicURLs []string `yaml:"publicUrls"`
	// TrustForwardedHeaders takes the client address from X-Forwarded-For.
	TrustForwardedHeaders bool `yaml:"trustForwardedHeaders"`
}

// NetworkConfig is the devnet the controller watches.
type NetworkConfig struct {
	// Name is the network name certificates must be issued for.
	Name string `yaml:"name"`
	// BeaconEndpoints are beacon API base URLs, tried in order.
	BeaconEndpoints []string `yaml:"beaconEndpoints"`
	// ValidatorRangesURL is the devnet's validator ranges inventory, from
	// which hosts get their validators; empty gives no host validators.
	ValidatorRangesURL string `yaml:"validatorRangesUrl"`
}

// AgentsConfig is how agents are verified and answered.
type AgentsConfig struct {
	// SignerPublicKey is the unpadded base64url ML-DSA-44 signer public key
	// (keygen signer-pubkey).
	SignerPublicKey string `yaml:"signerPublicKey"`
	// ControllerSeedFile holds the controller seed (not the devnet seed),
	// from which the key that signs answers to agents is derived.
	ControllerSeedFile string `yaml:"controllerSeedFile"`
	// AllowUnsignedAnswers runs without a controller seed: answers to agents
	// are not signed (development only).
	AllowUnsignedAnswers bool `yaml:"allowUnsignedAnswers"`
	// AllowInsecureHTTP accepts agent requests over plain http when no
	// public URL is set (development only).
	AllowInsecureHTTP bool `yaml:"allowInsecureHttp"`
}

// AuthConfig is how users log in.
type AuthConfig struct {
	// ProviderURL is the authenticatoor base URL; empty needs AllowOpenAPI.
	ProviderURL string `yaml:"providerUrl"`
	// Service is the name of this service in the token's "services" claim.
	Service string `yaml:"service"`
	// AllowOpenAPI runs without an auth provider: every action is anonymous
	// and allowed (closed test networks only).
	AllowOpenAPI bool `yaml:"allowOpenApi"`
}

// UIConfig configures the web UI and what is served to its logins.
type UIConfig struct {
	// Origins are extra origins the web UI may connect to.
	Origins []string `yaml:"origins"`
	// Script is an https URL of a script the web UI includes, e.g. a site
	// menu; ScriptIntegrity is its subresource integrity.
	Script          string `yaml:"script"`
	ScriptIntegrity string `yaml:"scriptIntegrity"`
	// Pprof serves the Go profiles under /debug/pprof/ to admin logins; it
	// needs auth.providerUrl.
	Pprof bool `yaml:"pprof"`
}

// ImagesConfig is how the image tags and source branches are looked up.
type ImagesConfig struct {
	// RegistryAuthFile is a Docker config.json with the credentials for the
	// lookups; empty or missing means anonymous.
	RegistryAuthFile string `yaml:"registryAuthFile"`
	// RegistryCAFile is an extra CA bundle for private registries.
	RegistryCAFile string `yaml:"registryCaFile"`
	// RegistryPlainHTTP lists registry hosts to look up without TLS.
	RegistryPlainHTTP []string `yaml:"registryPlainHttp"`
	// RegistryAllowPrivate also looks tags up at loopback, private and
	// link-local addresses.
	RegistryAllowPrivate bool `yaml:"registryAllowPrivate"`
	// SourceInterval is how often the source branches are read.
	SourceInterval Duration `yaml:"sourceInterval"`
	// Sources name the GitHub repository an image is built from, as
	// "<image>=<owner>/<name>[@<ref>]", for images whose labels do not.
	Sources []string `yaml:"sources"`
}

// StoreConfig is the database.
type StoreConfig struct {
	// DataDir holds the SQLite database.
	DataDir string `yaml:"dataDir"`
	// AuditRetention is how long audit entries stay; 0 keeps them.
	AuditRetention Duration `yaml:"auditRetention"`
}

// Duration is a time.Duration written as a Go duration string ("90s",
// "24h"). A bare number other than 0 is refused: it has no unit.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration must be a string such as 90s", n.Line)
	}

	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}

	*d = Duration(v)

	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std is the time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// DefaultConfig is the configuration of an empty file.
func DefaultConfig() Config {
	return Config{
		Server:       ServerConfig{Listen: ":8080"},
		Auth:         AuthConfig{Service: api.DefaultAuthService},
		Images:       ImagesConfig{SourceInterval: Duration(upstream.DefaultInterval)},
		Store:        StoreConfig{DataDir: "/data"},
		DeployWindow: Duration(24 * time.Hour),
	}
}

// LoadConfig reads the configuration file at path over the defaults and
// applies the "key=value" overrides of sets, whose keys are dotted paths
// ("auth.providerUrl", "settingDefaults.leaseTtl"). An empty path is the
// defaults. Unknown keys are errors. The result is not validated.
func LoadConfig(path string, sets []string) (*Config, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode}

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}

		if err := yaml.Unmarshal(raw, doc); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}

	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config %s: the document must be a mapping", path)
	}

	for _, set := range sets {
		if err := applySet(root, set); err != nil {
			return nil, fmt.Errorf("--set %s: %w", set, err)
		}
	}

	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	cfg := DefaultConfig()

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	return &cfg, nil
}

// applySet writes one "key=value" override into the document root.
func applySet(root *yaml.Node, set string) error {
	key, value, ok := strings.Cut(set, "=")
	if !ok || key == "" {
		return errors.New("not key=value")
	}

	path := strings.Split(key, ".")

	typ, err := keyType(reflect.TypeFor[Config](), path)
	if err != nil {
		return err
	}

	leaf, err := valueNode(typ, value)
	if err != nil {
		return err
	}

	node := root

	for i, name := range path {
		last := i == len(path)-1
		child := mappingValue(node, name)

		switch {
		case last && child != nil:
			*child = *leaf
		case last:
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: strTag, Value: name}, leaf)
		case child == nil:
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: strTag, Value: name}, child)
		case child.Kind != yaml.MappingNode:
			return fmt.Errorf("%s is not a mapping", strings.Join(path[:i+1], "."))
		}

		node = child
	}

	return nil
}

// mappingValue is the value of key in a mapping node; nil when absent.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}

	return nil
}

var durationType = reflect.TypeFor[Duration]()

// strTag is the YAML tag of a string.
const strTag = "!!str"

// anyType is the type of a key below settingDefaults: the settings decide it.
var anyType = reflect.TypeFor[any]()

// keyType is the type of the field a dotted path names; anyType for a key
// below a map.
func keyType(t reflect.Type, path []string) (reflect.Type, error) {
	for i, name := range path {
		switch t.Kind() {
		case reflect.Map:
			if i != len(path)-1 {
				return nil, fmt.Errorf("unknown config key %s", strings.Join(path, "."))
			}

			return anyType, nil
		case reflect.Struct:
		default:
			return nil, fmt.Errorf("unknown config key %s", strings.Join(path, "."))
		}

		found := false

		for f := range t.Fields() {
			if tag := strings.Split(f.Tag.Get("yaml"), ",")[0]; tag == name && tag != "-" {
				t, found = f.Type, true

				break
			}
		}

		if !found {
			return nil, fmt.Errorf("unknown config key %s", strings.Join(path[:i+1], "."))
		}
	}

	if t.Kind() == reflect.Struct || t.Kind() == reflect.Map {
		return nil, fmt.Errorf("%s is a mapping: set its keys", strings.Join(path, "."))
	}

	return t, nil
}

// valueNode is the YAML of an override value for a field of type t: a list
// is comma separated (or a YAML flow list), a string and a duration are
// taken as they are, anything else is read as YAML.
func valueNode(t reflect.Type, value string) (*yaml.Node, error) {
	if t.Kind() == reflect.Slice && !strings.HasPrefix(strings.TrimSpace(value), "[") {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}

		for item := range strings.SplitSeq(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: strTag, Value: item})
			}
		}

		return seq, nil
	}

	if t.Kind() == reflect.String || t == durationType {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: strTag, Value: value}, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(value), &doc); err != nil {
		return nil, err
	}

	if len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}

	return doc.Content[0], nil
}

// Validate checks the configuration; an error names the key.
func (c *Config) Validate() error {
	var errs []error

	bad := func(key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
	}

	if c.Server.Listen == "" {
		bad("server.listen", "is required")
	}

	if c.Store.DataDir == "" {
		bad("store.dataDir", "is required")
	}

	if c.Network.Name == "" {
		bad("network.name", "is required")
	} else if err := identity.ValidateName(c.Network.Name); err != nil {
		bad("network.name", "%v", err)
	}

	if len(c.Network.BeaconEndpoints) == 0 {
		bad("network.beaconEndpoints", "needs at least one beacon API URL")
	}

	for i, u := range c.Network.BeaconEndpoints {
		if parsed, err := url.Parse(u); err != nil || parsed.Host == "" {
			bad(fmt.Sprintf("network.beaconEndpoints[%d]", i), "%q is not a URL", u)
		}
	}

	if c.Agents.SignerPublicKey == "" {
		bad("agents.signerPublicKey", "is required (keygen signer-pubkey)")
	} else if _, err := identity.DecodePublicKey(c.Agents.SignerPublicKey); err != nil {
		bad("agents.signerPublicKey", "%v", err)
	}

	switch {
	case c.Agents.ControllerSeedFile == "" && !c.Agents.AllowUnsignedAnswers:
		bad("agents.controllerSeedFile", "is required: agents act only on signed answers "+
			"(agents.allowUnsignedAnswers runs without, for development)")
	case c.Agents.ControllerSeedFile != "" && c.Agents.AllowUnsignedAnswers:
		bad("agents.controllerSeedFile", "excludes agents.allowUnsignedAnswers")
	}

	switch {
	case c.Auth.ProviderURL == "" && !c.Auth.AllowOpenAPI:
		bad("auth.providerUrl", "is required: without it every action is anonymous and allowed "+
			"(auth.allowOpenApi runs without, for closed test networks)")
	case c.Auth.ProviderURL != "" && c.Auth.AllowOpenAPI:
		bad("auth.providerUrl", "excludes auth.allowOpenApi")
	}

	if c.UI.Pprof && c.Auth.ProviderURL == "" {
		bad("ui.pprof", "needs auth.providerUrl: the profiles are only served to admin logins")
	}

	if c.Images.SourceInterval <= 0 {
		bad("images.sourceInterval", "must be positive")
	}

	for i, value := range c.Images.Sources {
		if _, _, err := upstream.ParseSource(value); err != nil {
			bad(fmt.Sprintf("images.sources[%d]", i), "%v", err)
		}
	}

	if c.Store.AuditRetention < 0 {
		bad("store.auditRetention", "must not be negative")
	} else if r := c.Store.AuditRetention.Std(); r > 0 && r < time.Hour {
		bad("store.auditRetention", "must be 0 (keep) or at least 1h")
	}

	if c.DeployWindow < 0 || c.DeployWindow.Std() > fleet.MaxDeployWindow {
		bad("deployWindow", "must be between 0 and 720h")
	}

	if _, err := c.Settings(); err != nil {
		bad("settingDefaults", "%v", err)
	}

	return errors.Join(errs...)
}

// Settings are the built-in setting defaults with SettingDefaults applied
// and validated.
func (c *Config) Settings() (fleet.Settings, error) {
	settings := fleet.DefaultSettings()

	raw, err := json.Marshal(settings)
	if err != nil {
		return settings, err
	}

	fields := make(map[string]json.RawMessage, 32)
	if uerr := json.Unmarshal(raw, &fields); uerr != nil {
		return settings, uerr
	}

	for key, value := range c.SettingDefaults {
		if _, known := fields[key]; !known {
			return settings, fmt.Errorf("unknown setting %q", key)
		}

		encoded, encErr := json.Marshal(value)
		if encErr != nil {
			return settings, fmt.Errorf("%s: %w", key, encErr)
		}

		fields[key] = encoded
	}

	merged, err := json.Marshal(fields)
	if err != nil {
		return settings, err
	}

	// Decoded strictly into the typed struct: a bare number for a duration
	// ("leaseTtl: 30") is refused.
	if uerr := json.Unmarshal(merged, &settings); uerr != nil {
		return settings, uerr
	}

	if verr := settings.Validate(); verr != nil {
		return settings, verr
	}

	return settings, nil
}
