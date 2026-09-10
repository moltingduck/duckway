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
	Name                   string            `json:"name,omitempty" yaml:"name,omitempty"`
	RawOutputSubscriptions *int              `json:"raw_output_subscription_limit,omitempty" yaml:"raw_output_subscription_limit,omitempty"`
	SessionListWidth       *int              `json:"session_list_width,omitempty" yaml:"session_list_width,omitempty"`
	AutoHideSessionList    *bool             `json:"auto_hide_session_list,omitempty" yaml:"auto_hide_session_list,omitempty"`
	Shortcuts              map[string]string `json:"shortcuts,omitempty" yaml:"shortcuts,omitempty"`
	Clients                []Client          `json:"hosts" yaml:"hosts"`
}

const DefaultRawOutputSubscriptions = 10
const DefaultSessionListWidth = 36

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
	if err := cfg.normalize(); err != nil {
		return nil, err
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
	if cfg == nil {
		return fmt.Errorf("ducklord config is required")
	}
	if err := cfg.normalize(); err != nil {
		return err
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

func (c *Config) normalize() error {
	if c.RawOutputSubscriptions != nil && (*c.RawOutputSubscriptions < 1 || *c.RawOutputSubscriptions > 100) {
		return fmt.Errorf("raw_output_subscription_limit must be between 1 and 100")
	}
	if c.SessionListWidth != nil && (*c.SessionListWidth < 20 || *c.SessionListWidth > 80) {
		return fmt.Errorf("session_list_width must be between 20 and 80")
	}
	for action, binding := range c.Shortcuts {
		if _, ok := DefaultShortcuts[action]; !ok {
			return fmt.Errorf("unknown shortcut action %q", action)
		}
		if !validShortcutBinding(binding) {
			return fmt.Errorf("invalid shortcut binding %q for %s", binding, action)
		}
		c.Shortcuts[action] = canonicalShortcutBinding(binding)
	}
	seen := make(map[string]string)
	for action, fallback := range DefaultShortcuts {
		binding := fallback
		if c.Shortcuts[action] != "" {
			binding = c.Shortcuts[action]
		}
		if previous := seen[binding]; previous != "" {
			return fmt.Errorf("shortcut %q is assigned to both %s and %s", binding, previous, action)
		}
		seen[binding] = action
	}
	return nil
}

var DefaultShortcuts = map[string]string{
	"help": "?", "quit": "q", "host_actions": "h", "host_add": "a", "host_remove": "d",
	"session_create": "c", "session_actions": "m", "session_notifications": "n", "session_yield": "y", "session_yield_wait": "Y",
	"session_end": "E", "session_restart": "R", "session_destroy": "X", "list_search": "/", "list_organize": "o", "list_groups": "g",
	"list_reorder_up": "ctrl-k", "list_reorder_down": "ctrl-j", "pty_copy": "v", "pty_unfocus": "ctrl-]", "refresh": "r", "shortcut_settings": "S",
}

func validShortcutBinding(binding string) bool {
	if strings.HasPrefix(binding, "ctrl-") {
		runes := []rune(strings.TrimPrefix(binding, "ctrl-"))
		return len(runes) == 1 && (runes[0] >= 'a' && runes[0] <= 'z' || runes[0] >= 'A' && runes[0] <= 'Z' || runes[0] == ']')
	}
	runes := []rune(binding)
	return len(runes) == 1 && !unicode.IsControl(runes[0]) && !unicode.Is(unicode.Cf, runes[0])
}

func canonicalShortcutBinding(binding string) string {
	if strings.HasPrefix(binding, "ctrl-") {
		return "ctrl-" + strings.ToLower(strings.TrimPrefix(binding, "ctrl-"))
	}
	return binding
}

func (c *Config) Shortcut(action string) string {
	if c != nil && c.Shortcuts != nil && c.Shortcuts[action] != "" {
		return c.Shortcuts[action]
	}
	return DefaultShortcuts[action]
}

func (c *Config) RawOutputSubscriptionLimit() int {
	if c == nil || c.RawOutputSubscriptions == nil {
		return DefaultRawOutputSubscriptions
	}
	return *c.RawOutputSubscriptions
}

func (c *Config) SessionListPaneWidth() int {
	if c == nil || c.SessionListWidth == nil {
		return DefaultSessionListWidth
	}
	return *c.SessionListWidth
}

func (c *Config) SessionListAutoHide() bool {
	return c == nil || c.AutoHideSessionList == nil || *c.AutoHideSessionList
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
	clone.Shortcuts = make(map[string]string, len(c.Shortcuts))
	for action, binding := range c.Shortcuts {
		clone.Shortcuts[action] = binding
	}
	if c.RawOutputSubscriptions != nil {
		value := *c.RawOutputSubscriptions
		clone.RawOutputSubscriptions = &value
	}
	if c.SessionListWidth != nil {
		value := *c.SessionListWidth
		clone.SessionListWidth = &value
	}
	if c.AutoHideSessionList != nil {
		value := *c.AutoHideSessionList
		clone.AutoHideSessionList = &value
	}
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
