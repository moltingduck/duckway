package ducklord

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type Config struct {
	Clients []Client `json:"clients"`
}

type Client struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	User     string `json:"user,omitempty"`
	Group    string `json:"group,omitempty"`
	Ducklion string `json:"ducklion,omitempty"`
	SSH      string `json:"ssh,omitempty"`
}

func DefaultConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("DUCKLORD_CONFIG")); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ducklord", "config.json")
}

func LoadConfig(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ducklord config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse ducklord config: %w", err)
	}
	if len(cfg.Clients) == 0 {
		return nil, fmt.Errorf("ducklord config has no clients")
	}
	seen := map[string]bool{}
	for i := range cfg.Clients {
		if err := cfg.Clients[i].Normalize(); err != nil {
			return nil, fmt.Errorf("client %d: %w", i+1, err)
		}
		if seen[cfg.Clients[i].Name] {
			return nil, fmt.Errorf("duplicate client name %q", cfg.Clients[i].Name)
		}
		seen[cfg.Clients[i].Name] = true
	}
	return &cfg, nil
}

func (c *Config) Client(name string) (Client, bool) {
	for _, client := range c.Clients {
		if client.Name == name {
			return client, true
		}
	}
	return Client{}, false
}

func (c *Client) Normalize() error {
	c.Name = strings.TrimSpace(c.Name)
	c.Host = strings.TrimSpace(c.Host)
	c.User = strings.TrimSpace(c.User)
	c.Group = strings.TrimSpace(c.Group)
	c.Ducklion = strings.TrimSpace(c.Ducklion)
	c.SSH = strings.TrimSpace(c.SSH)
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !SafeIdentifier(c.Name) {
		return fmt.Errorf("invalid client name %q", c.Name)
	}
	if c.Host == "" {
		return fmt.Errorf("host is required")
	}
	if !safeSSHToken(c.Host) {
		return fmt.Errorf("invalid host %q", c.Host)
	}
	if c.User != "" && !SafeIdentifier(c.User) {
		return fmt.Errorf("invalid user %q", c.User)
	}
	if c.Group != "" && !SafeGroup(c.Group) {
		return fmt.Errorf("invalid group %q", c.Group)
	}
	if c.Ducklion == "" {
		c.Ducklion = "ducklion"
	}
	if !safeRemoteCommand(c.Ducklion) {
		return fmt.Errorf("invalid ducklion command %q", c.Ducklion)
	}
	if c.SSH == "" {
		c.SSH = "ssh"
	}
	if !safeRemoteCommand(c.SSH) {
		return fmt.Errorf("invalid ssh command %q", c.SSH)
	}
	return nil
}

func (c Client) Target() string {
	if c.User == "" {
		return c.Host
	}
	return c.User + "@" + c.Host
}

func SafeIdentifier(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func SafeGroup(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '/' {
			continue
		}
		return false
	}
	return true
}

func safeSSHToken(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return !strings.ContainsAny(s, "\"'`;$&|()<>")
}

func safeRemoteCommand(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return !strings.ContainsAny(s, "\"'`;$&|()<>")
}
