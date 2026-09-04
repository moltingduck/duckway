package ducklord

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Name    string   `json:"name,omitempty" yaml:"name,omitempty"`
	Clients []Client `json:"hosts" yaml:"hosts"`
}

type Client struct {
	Name     string `json:"name" yaml:"name"`
	Host     string `json:"host" yaml:"host"`
	User     string `json:"user,omitempty" yaml:"user,omitempty"`
	Group    string `json:"group,omitempty" yaml:"group,omitempty"`
	Ducklion string `json:"ducklion,omitempty" yaml:"ducklion,omitempty"`
	SSH      string `json:"ssh,omitempty" yaml:"ssh,omitempty"`
}

func DefaultConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("DUCKLORD_CONFIG")); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ducklord", "config.yaml")
}

func LoadConfig(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath()
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read ducklord config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read ducklord config: %s is not a regular file", path)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("read ducklord config: file exceeds 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read ducklord config: %w", err)
	}
	defer file.Close()
	var cfg Config
	decoder := yaml.NewDecoder(io.LimitReader(file, (1<<20)+1))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse ducklord config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse ducklord config: multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("parse ducklord config: %w", err)
	}
	if cfg.Name != "" && !ValidOwnerName(cfg.Name) {
		return nil, fmt.Errorf("invalid owner name %q", cfg.Name)
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

func SaveConfig(path string, cfg *Config) error {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath()
	}
	seen := make(map[string]bool, len(cfg.Clients))
	for i := range cfg.Clients {
		if err := cfg.Clients[i].Normalize(); err != nil {
			return fmt.Errorf("client %d: %w", i+1, err)
		}
		if seen[cfg.Clients[i].Name] {
			return fmt.Errorf("duplicate client name %q", cfg.Clients[i].Name)
		}
		seen[cfg.Clients[i].Name] = true
	}
	if cfg.Name != "" && !ValidOwnerName(cfg.Name) {
		return fmt.Errorf("invalid owner name %q", cfg.Name)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ResolveOwnerName applies --name > config.name > local hostname precedence.
func ResolveOwnerName(explicit, configured string) (string, error) {
	name := explicit
	if name == "" {
		name = configured
	}
	if name == "" {
		var err error
		name, err = os.Hostname()
		if err != nil {
			return "", fmt.Errorf("resolve local hostname: %w", err)
		}
	}
	if !ValidOwnerName(name) {
		return "", fmt.Errorf("invalid owner name %q", name)
	}
	return name, nil
}

func ValidOwnerName(name string) bool {
	return protocol.ValidDucklordPrincipal(name)
}

func (c *Config) Client(name string) (Client, bool) {
	for _, client := range c.Clients {
		if client.Name == name {
			return client, true
		}
	}
	return Client{}, false
}

func (c *Config) Clone() *Config {
	clone := *c
	clone.Clients = append([]Client(nil), c.Clients...)
	return &clone
}

func (c *Config) AddClient(client Client) error {
	if err := client.Normalize(); err != nil {
		return err
	}
	if _, ok := c.Client(client.Name); ok {
		return fmt.Errorf("duplicate client name %q", client.Name)
	}
	c.Clients = append(c.Clients, client)
	sort.SliceStable(c.Clients, func(i, j int) bool {
		if c.Clients[i].Group != c.Clients[j].Group {
			return c.Clients[i].Group < c.Clients[j].Group
		}
		return c.Clients[i].Name < c.Clients[j].Name
	})
	return nil
}

func (c *Config) RemoveClient(name string) bool {
	for i := range c.Clients {
		if c.Clients[i].Name == name {
			c.Clients = append(c.Clients[:i], c.Clients[i+1:]...)
			return true
		}
	}
	return false
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
	if !safeRemoteCommandLine(c.Ducklion) {
		return fmt.Errorf("invalid ducklion command %q", c.Ducklion)
	}
	if c.SSH == "" {
		c.SSH = "ssh"
	}
	if !safeRemoteCommandLine(c.SSH) {
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

func (c Client) DucklionArgs(args ...string) []string {
	parts := strings.Fields(c.Ducklion)
	out := make([]string, 0, len(parts)+len(args))
	out = append(out, parts...)
	out = append(out, args...)
	return out
}

func (c Client) SSHCommandParts() []string {
	parts := strings.Fields(c.SSH)
	if len(parts) == 0 {
		return []string{"ssh"}
	}
	return parts
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
	if strings.HasPrefix(s, "-") {
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

func safeRemoteCommandLine(s string) bool {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !safeRemoteCommand(part) {
			return false
		}
	}
	return true
}
