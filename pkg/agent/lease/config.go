package lease

import "time"

// The defaults of the verify section.
const (
	defaultWindow  = 45 * time.Second
	defaultTimeout = 10 * time.Minute
)

// Config is the verify section of agent.yaml.
type Config struct {
	// Window is how long touched containers must run without a restart (default 45s).
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
