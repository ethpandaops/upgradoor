package lease

import "time"

// The defaults of the verify section.
const (
	defaultWindow  = 45 * time.Second
	defaultTimeout = 10 * time.Minute
)

// Config is the verify section of agent.yaml.
type Config struct {
	// Window is how long verifying watches the touched containers; each must
	// then have run stableUp (at most the window) without a restart, and one
	// that restarted late extends it up to maxVerify (default 45s).
	Window time.Duration `yaml:"window"`
	// Timeout bounds the starting and verifying phases together (default 10m).
	Timeout time.Duration `yaml:"timeout"`
}

// ApplyDefaults fills the durations that are not positive (45s, 10m).
func (c *Config) ApplyDefaults() {
	if c.Window <= 0 {
		c.Window = defaultWindow
	}

	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
}
