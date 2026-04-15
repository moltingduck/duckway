package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	ListenAddr    string
	DataDir       string
	EncryptionKey []byte // 32 bytes for AES-256
	SessionSecret []byte // for cookie signing
}

func DefaultConfig() *Config {
	dataDir := os.Getenv("DUCKWAY_DATA_DIR")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".duckway-server")
	}

	return &Config{
		ListenAddr: envOrDefault("DUCKWAY_LISTEN", ":8080"),
		DataDir:    dataDir,
	}
}

func (c *Config) Init() error {
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// Load or generate encryption key
	keyPath := filepath.Join(c.DataDir, "encryption.key")
	key, err := loadOrGenerateKey(keyPath, 32)
	if err != nil {
		return fmt.Errorf("encryption key: %w", err)
	}
	c.EncryptionKey = key

	// Load or generate session secret
	secretPath := filepath.Join(c.DataDir, "session.key")
	secret, err := loadOrGenerateKey(secretPath, 32)
	if err != nil {
		return fmt.Errorf("session secret: %w", err)
	}
	c.SessionSecret = secret

	return nil
}

func loadOrGenerateKey(path string, size int) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return hex.DecodeString(string(data))
	}

	key := make([]byte, size)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}

	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0600); err != nil {
		return nil, err
	}

	return key, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
