package client

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hackerduck/duckway/internal/duckwayconfig"
	"gopkg.in/yaml.v3"
)

type Config struct {
	ServerURL           string `yaml:"server_url"`
	ClientName          string `yaml:"client_name"`
	Token               string `yaml:"token"`
	ProxyPort           int    `yaml:"proxy_port"`
	PTYLogRetentionDays int    `yaml:"pty_log_retention_days,omitempty"`
}

func DefaultConfigDir() string {
	return duckwayconfig.DefaultConfigDir()
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
	if cfg.PTYLogRetentionDays == 0 {
		cfg.PTYLogRetentionDays = duckwayconfig.DefaultPTYLogRetentionDays
	}
	if cfg.PTYLogRetentionDays < 1 || cfg.PTYLogRetentionDays > duckwayconfig.MaxPTYLogRetentionDays {
		return nil, fmt.Errorf("pty_log_retention_days must be between 1 and %d", duckwayconfig.MaxPTYLogRetentionDays)
	}

	return &cfg, nil
}

func SaveConfig(configDir string, cfg *Config) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	dirInfo, err := os.Lstat(configDir)
	if err != nil {
		return fmt.Errorf("inspect config directory: %w", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config directory must be a real directory")
	}
	if stat, ok := dirInfo.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("config directory must be owned by the current user")
	}
	if err := os.Chmod(configDir, 0700); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	path := filepath.Join(configDir, "config.yaml")
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("config.yaml must be a regular file, not a symlink")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("config.yaml must be owned by the current user")
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	tmp, err := os.CreateTemp(configDir, ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if chmodErr := tmp.Chmod(0600); chmodErr != nil {
		err = chmodErr
	} else {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(configDir)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func KeysEnvPath(configDir string) string {
	return filepath.Join(configDir, "keys.env")
}
