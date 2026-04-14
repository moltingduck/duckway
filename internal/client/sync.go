package client

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// SyncKeys fetches placeholder keys from the server and writes them to keys.env.
func SyncKeys(configDir string, cfg *Config) (int, error) {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	keys, err := api.FetchKeys()
	if err != nil {
		return 0, err
	}

	var lines []string
	lines = append(lines, "# Duckway placeholder keys — auto-generated, do not edit")
	lines = append(lines, fmt.Sprintf("# Server: %s | Client: %s", cfg.ServerURL, cfg.ClientName))
	lines = append(lines, "")

	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("# Service: %s", k.ServiceName))
		lines = append(lines, fmt.Sprintf("%s=%s", k.EnvName, k.Placeholder))
		lines = append(lines, "")
	}

	envPath := KeysEnvPath(configDir)
	if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return 0, fmt.Errorf("write keys.env: %w", err)
	}

	// Sync canary tokens
	canaryCount, err := SyncCanaries(configDir, cfg)
	if err != nil {
		log.Printf("Warning: canary sync failed: %v", err)
	} else if canaryCount > 0 {
		log.Printf("Deployed %d canary tokens", canaryCount)
	}

	return len(keys), nil
}

// SyncCanaries fetches canary tokens and deploys them as decoy files.
// Canary tokens are placed in realistic paths under $HOME to look like
// real credentials, but NOT in keys.env so agents use the real placeholders.
func SyncCanaries(configDir string, cfg *Config) (int, error) {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	canaries, err := api.FetchCanaries()
	if err != nil {
		return 0, err
	}
	if len(canaries) == 0 {
		return 0, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return 0, fmt.Errorf("get home dir: %w", err)
	}

	deployed := 0
	for _, c := range canaries {
		// Deploy to home directory path (e.g., ~/.aws/credentials)
		deployPath := filepath.Join(home, c.DeployPath)

		// Don't overwrite existing real files
		if _, err := os.Stat(deployPath); err == nil {
			continue
		}

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(deployPath), 0700); err != nil {
			log.Printf("Warning: cannot create dir for canary %s: %v", c.DeployPath, err)
			continue
		}

		if err := os.WriteFile(deployPath, []byte(c.DeployContent), 0600); err != nil {
			log.Printf("Warning: cannot deploy canary %s: %v", c.DeployPath, err)
			continue
		}

		deployed++
	}

	// Write a manifest so we know what we deployed (for cleanup)
	var manifest []string
	for _, c := range canaries {
		manifest = append(manifest, c.DeployPath)
	}
	manifestPath := filepath.Join(configDir, "canaries.manifest")
	os.WriteFile(manifestPath, []byte(strings.Join(manifest, "\n")), 0600)

	return deployed, nil
}

// PrintEnv outputs keys in shell-eval format.
func PrintEnv(configDir string) error {
	data, err := os.ReadFile(KeysEnvPath(configDir))
	if err != nil {
		return fmt.Errorf("no keys found — run 'duckway sync' first: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fmt.Printf("export %s\n", line)
	}
	return nil
}
