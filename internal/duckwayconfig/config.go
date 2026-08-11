package duckwayconfig

import (
	"os"
	"path/filepath"
)

func DefaultConfigDir() string {
	if d := os.Getenv("DUCKWAY_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".duckway")
}
