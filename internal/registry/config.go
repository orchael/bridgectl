package registry

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the registry service configuration.
type Config struct {
	Listen  string        `yaml:"listen"`
	TLS     TLSConfig     `yaml:"tls"`
	Storage StorageConfig `yaml:"storage"`
	Servers []ServerEntry `yaml:"servers"`
}

// TLSConfig holds TLS certificate paths for mTLS.
type TLSConfig struct {
	CABundle string `yaml:"ca_bundle"`
	Cert     string `yaml:"cert"`
	Key      string `yaml:"key"`
}

// StorageConfig selects and configures the storage backend.
type StorageConfig struct {
	Type    string `yaml:"type"`     // "memory" or "file"
	BaseDir string `yaml:"base_dir"` // required when type=file
}

// ServerEntry maps a server ID to its allowed issuers.
type ServerEntry struct {
	ID             string   `yaml:"id"`
	AllowedIssuers []string `yaml:"allowed_issuers"`
}

// LoadConfig reads and parses a registry config YAML file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Listen: ":8443",
		Storage: StorageConfig{
			Type: "memory",
		},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return cfg, nil
}

// BuildServerConfig converts the config's server entries to a ServerConfig.
func (c *Config) BuildServerConfig() ServerConfig {
	sc := ServerConfig{
		AllowedIssuers: make(map[string][]string, len(c.Servers)),
	}
	for _, s := range c.Servers {
		sc.AllowedIssuers[s.ID] = s.AllowedIssuers
	}
	return sc
}
