package duckwayconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPTYLogRetentionDays = 7
	MaxPTYLogRetentionDays     = 3650
)

type RuntimeSettings struct {
	PTYLogRetentionDays int `yaml:"pty_log_retention_days"`
}

func LoadRuntimeSettings(configDir string) (RuntimeSettings, error) {
	settings := RuntimeSettings{PTYLogRetentionDays: DefaultPTYLogRetentionDays}
	data, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return RuntimeSettings{}, err
	}
	var raw RuntimeSettings
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return RuntimeSettings{}, fmt.Errorf("parse Duckway runtime settings: %w", err)
	}
	if raw.PTYLogRetentionDays == 0 {
		return settings, nil
	}
	if raw.PTYLogRetentionDays < 1 || raw.PTYLogRetentionDays > MaxPTYLogRetentionDays {
		return RuntimeSettings{}, fmt.Errorf("pty_log_retention_days must be between 1 and %d", MaxPTYLogRetentionDays)
	}
	return raw, nil
}

func (s RuntimeSettings) PTYLogRetention() time.Duration {
	days := s.PTYLogRetentionDays
	if days == 0 {
		days = DefaultPTYLogRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}
