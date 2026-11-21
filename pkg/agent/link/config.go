package link

import (
	"crypto/mldsa"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

const (
	defaultSyncInterval      = 30 * time.Second
	defaultLeaseSyncInterval = 5 * time.Second
	defaultMinSyncInterval   = time.Second
	defaultMaxSyncInterval   = 10 * time.Minute
)

// Config is the controller section of agent.yaml.
type Config struct {
	// URL is the controller base URL, e.g. https://upgradoor.<network>.
	URL string `yaml:"url"`
	// SyncInterval is the idle sync interval (default 30s).
	SyncInterval time.Duration `yaml:"syncInterval"`
	// LeaseSyncInterval is the interval while busy (default 5s). Both apply
	// only while no answer has said when to sync next.
	LeaseSyncInterval time.Duration `yaml:"leaseSyncInterval"`
	// MinSyncInterval and MaxSyncInterval bound the interval the controller
	// asks for (defaults 1s and 10m).
	MinSyncInterval time.Duration `yaml:"minSyncInterval"`
	MaxSyncInterval time.Duration `yaml:"maxSyncInterval"`
	// AllowInsecure permits a plain http controller URL (development only).
	AllowInsecure bool `yaml:"allowInsecure"`
	// PublicKey is the controller public key (unpadded base64url).
	PublicKey string `yaml:"publicKey"`
	// AllowUnsignedAnswers runs without PublicKey (development only).
	AllowUnsignedAnswers bool `yaml:"allowUnsignedAnswers"`
}

// ApplyDefaults fills the intervals that are not positive.
func (c *Config) ApplyDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}

	def(&c.SyncInterval, defaultSyncInterval)
	def(&c.LeaseSyncInterval, defaultLeaseSyncInterval)
	def(&c.MinSyncInterval, defaultMinSyncInterval)
	def(&c.MaxSyncInterval, defaultMaxSyncInterval)
}

// Validate checks the URL and the key.
func (c *Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" {
		return errors.New("controller.url must be an absolute URL")
	}

	if u.Scheme != "https" && (u.Scheme != "http" || !c.AllowInsecure) {
		return errors.New("controller.url must be https (or http with controller.allowInsecure)")
	}

	_, err = c.Key()

	return err
}

// Key decodes publicKey; it is nil only with allowUnsignedAnswers.
func (c *Config) Key() (*mldsa.PublicKey, error) {
	switch {
	case c.PublicKey == "" && c.AllowUnsignedAnswers:
		return nil, nil //nolint:nilnil // no key and no error is the opt-out
	case c.PublicKey == "":
		return nil, errors.New("controller.publicKey is required: the agent acts only on " +
			"answers signed by the controller key (controller.allowUnsignedAnswers runs " +
			"without, for development)")
	case c.AllowUnsignedAnswers:
		return nil, errors.New(
			"controller.publicKey and controller.allowUnsignedAnswers exclude each other")
	}

	key, err := identity.DecodePublicKey(c.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("controller.publicKey: %w", err)
	}

	return key, nil
}
