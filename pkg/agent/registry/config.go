package registry

// defaultAuthFile is the root user's Docker config.json.
const defaultAuthFile = "/root/.docker/config.json"

// Config is the registry section of agent.yaml.
type Config struct {
	// AuthFile is a Docker config.json with the credentials for pulls; a
	// missing file means anonymous.
	AuthFile string `yaml:"authFile"`
}

// ApplyDefaults fills unset fields.
func (c *Config) ApplyDefaults() {
	if c.AuthFile == "" {
		c.AuthFile = defaultAuthFile
	}
}
