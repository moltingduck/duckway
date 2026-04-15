package client

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ServerURL  string `yaml:"server_url"`
	ClientName string `yaml:"client_name"`
	Token      string `yaml:"token"`
	ProxyPort  int    `yaml:"proxy_port"`
}

func DefaultConfigDir() string {
	if d := os.Getenv("DUCKWAY_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".duckway")
}

func LoadConfig(configDir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("config not found — run 'duckway init' first: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = 18080
	}

	return &cfg, nil
}

func SaveConfig(configDir string, cfg *Config) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(configDir, "config.yaml"), data, 0600)
}

func KeysEnvPath(configDir string) string {
	return filepath.Join(configDir, "keys.env")
}
