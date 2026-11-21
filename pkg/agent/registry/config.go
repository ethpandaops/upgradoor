package registry

import "time"

// Defaults of the registry section.
const (
	defaultPollInterval = 30 * time.Minute
	defaultAuthFile     = "/root/.docker/config.json"
)

// Config is the registry section of agent.yaml.
type Config struct {
	// PollInterval is the tracker's own per-tag lookup interval, jittered by up to 20%.
	PollInterval time.Duration `yaml:"pollInterval"`
	// AuthFile is a Docker config.json with credentials; a missing file means anonymous.
	AuthFile string `yaml:"authFile"`
	// PlainHTTP lists registry hosts to talk to without TLS.
	PlainHTTP []string `yaml:"plainHttp"`
	// CAFile is an extra CA bundle for private registries.
	CAFile string `yaml:"caFile"`
}

// ApplyDefaults fills unset fields; a duration that is not positive is unset.
func (c *Config) ApplyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}

	if c.AuthFile == "" {
		c.AuthFile = defaultAuthFile
	}
}
