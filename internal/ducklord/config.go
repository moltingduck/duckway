package ducklord

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"gopkg.in/yaml.v3"
)

type Config struct {
	WorkspaceTheme         WorkspaceTheme                          `json:"workspace_theme,omitempty" yaml:"workspace_theme,omitempty"`
	Name                   string                                  `json:"name,omitempty" yaml:"name,omitempty"`
	RawOutputSubscriptions *int                                    `json:"raw_output_subscription_limit,omitempty" yaml:"raw_output_subscription_limit,omitempty"`
	SessionListWidth       *int                                    `json:"session_list_width,omitempty" yaml:"session_list_width,omitempty"`
	AutoHideSessionList    *bool                                   `json:"auto_hide_session_list,omitempty" yaml:"auto_hide_session_list,omitempty"`
	PromoteUnreadSessions  *bool                                   `json:"promote_unread_sessions,omitempty" yaml:"promote_unread_sessions,omitempty"`
	QuickSort              string                                  `json:"quick_sort,omitempty" yaml:"quick_sort,omitempty"`
	QuickOldestFirst       bool                                    `json:"quick_oldest_first,omitempty" yaml:"quick_oldest_first,omitempty"`
	NotificationLevels     map[NotificationClass]NotificationLevel `json:"notification_levels,omitempty" yaml:"notification_levels,omitempty"`
	NotificationSounds     map[NotificationClass]string            `json:"notification_sounds,omitempty" yaml:"notification_sounds,omitempty"`
	OtherProjectThreshold  NotificationLevel                       `json:"other_project_threshold,omitempty" yaml:"other_project_threshold,omitempty"`
	Shortcuts              map[string]string                       `json:"shortcuts,omitempty" yaml:"shortcuts,omitempty"`
	SkillSources           []SkillTrackingSource                   `json:"skill_sources,omitempty" yaml:"skill_sources,omitempty"`
	Clients                []Client                                `json:"hosts" yaml:"hosts"`
}

const DefaultRawOutputSubscriptions = 10
const DefaultSessionListWidth = 36

type Client struct {
	Name               string                                  `json:"name" yaml:"name"`
	Host               string                                  `json:"host" yaml:"host"`
	User               string                                  `json:"user,omitempty" yaml:"user,omitempty"`
	Group              string                                  `json:"group,omitempty" yaml:"group,omitempty"`
	Ducklion           string                                  `json:"ducklion,omitempty" yaml:"ducklion,omitempty"`
	SSH                string                                  `json:"ssh,omitempty" yaml:"ssh,omitempty"`
	SkillTargets       []SkillInstallTarget                    `json:"skill_targets,omitempty" yaml:"skill_targets,omitempty"`
	SelectedSkills     []string                                `json:"selected_skills,omitempty" yaml:"selected_skills,omitempty"`
	NotificationLevels map[NotificationClass]NotificationLevel `json:"notification_levels,omitempty" yaml:"notification_levels,omitempty"`
}

// SkillTrackingSource is a public source used to refresh a locally managed
// skill. TLS verification remains enabled unless explicitly opted out.
type SkillTrackingSource struct {
	SkillID       string `json:"skill_id,omitempty" yaml:"skill_id,omitempty"`
	URL           string `json:"url" yaml:"url"`
	InsecureHTTPS bool   `json:"insecure_https,omitempty" yaml:"insecure_https,omitempty"`
}

// SkillInstallTarget identifies a remote directory into which selected skills
// are installed for a host.
type SkillInstallTarget struct {
	ID     string             `json:"id" yaml:"id"`
	Path   string             `json:"path" yaml:"path"`
	Skills []ManagedHostSkill `json:"skills,omitempty" yaml:"skills,omitempty"`
}

// ManagedHostSkill records Ducklord's relationship with one skill for one
// agent target. An absent record is legacy/unmanaged; an explicit "none"
// record prevents legacy selected_skills from being applied to this target.
type ManagedHostSkill struct {
	ID         string `json:"id" yaml:"id"`
	Management string `json:"management" yaml:"management"` // none, push, or pull
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
	return saveConfig(path, cfg, nil)
}

// SaveConfigIfUnchanged prevents a settings editor from overwriting another
// Ducklord process's changes between its read and write.
func SaveConfigIfUnchanged(path string, cfg *Config, expected []byte) error {
	if expected == nil {
		return fmt.Errorf("expected config contents are required")
	}
	return saveConfig(path, cfg, expected)
}

func saveConfig(path string, cfg *Config, expected []byte) error {
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
	if err := validateConfigSavePath(path); err != nil {
		return err
	}
	lockPath := filepath.Join(filepath.Dir(path), ".config.lock")
	lockFD, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(lockFD), lockPath)
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() {
		return fmt.Errorf("ducklord config lock must be a regular file")
	}
	if stat, ok := lockInfo.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("ducklord config lock must be owned by the current user")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := validateConfigSavePath(path); err != nil {
		return err
	}
	if expected != nil {
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, expected) {
			return fmt.Errorf("ducklord config changed on disk; close and reopen settings")
		}
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

func validateConfigSavePath(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("ducklord config directory must be a real directory")
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("ducklord config directory must not be writable by group or others")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("ducklord config directory must be owned by the current user")
	}
	info, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ducklord config must be a regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("ducklord config must be owned by the current user")
	}
	return nil
}

func (c *Config) normalize() error {
	seenSources := make(map[string]bool, len(c.SkillSources))
	seenSkillIDs := make(map[string]bool, len(c.SkillSources))
	for i := range c.SkillSources {
		if err := c.SkillSources[i].Normalize(); err != nil {
			return fmt.Errorf("skill source %d: %w", i+1, err)
		}
		if seenSources[c.SkillSources[i].URL] {
			return fmt.Errorf("duplicate skill source %q", c.SkillSources[i].URL)
		}
		seenSources[c.SkillSources[i].URL] = true
		if seenSkillIDs[c.SkillSources[i].SkillID] {
			return fmt.Errorf("duplicate skill source for %q", c.SkillSources[i].SkillID)
		}
		seenSkillIDs[c.SkillSources[i].SkillID] = true
	}
	if err := c.WorkspaceTheme.Validate(); err != nil {
		return err
	}
	if err := ValidateNotificationLevels(c.NotificationLevels); err != nil {
		return err
	}
	for class, path := range c.NotificationSounds {
		if err := class.Validate(); err != nil {
			return err
		}
		if strings.ContainsRune(path, 0) {
			return fmt.Errorf("notification sound path contains NUL")
		}
	}
	if c.OtherProjectThreshold != "" {
		if err := c.OtherProjectThreshold.Validate(); err != nil {
			return err
		}
	}
	if c.QuickSort != "" && c.QuickSort != "event_time" && c.QuickSort != "event_importance" && c.QuickSort != "host" && c.QuickSort != "type" {
		return fmt.Errorf("quick_sort must be event_time, event_importance, host, or type")
	}
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
		if action == "pane_prefix" && (!strings.HasPrefix(c.Shortcuts[action], "ctrl-") || c.Shortcuts[action] == "ctrl-i" || c.Shortcuts[action] == "ctrl-j" || c.Shortcuts[action] == "ctrl-c") {
			return fmt.Errorf("pane_prefix must be a control key other than Tab, Enter, or Ctrl-C")
		}
	}
	// Older configs may already use the new default key for another action.
	// Keep that user's binding and move only the newly introduced default.
	if c.Shortcuts["notification_settings"] == "" {
		used := make(map[string]bool, len(DefaultShortcuts))
		for action, fallback := range DefaultShortcuts {
			if action == "notification_settings" {
				continue
			}
			binding := c.Shortcuts[action]
			if binding == "" {
				binding = fallback
			}
			used[binding] = true
		}
		if used[DefaultShortcuts["notification_settings"]] {
			for _, alternate := range []string{"ctrl-o", "ctrl-p", "ctrl-u", "ctrl-b", "ctrl-g"} {
				if !used[alternate] {
					if c.Shortcuts == nil {
						c.Shortcuts = make(map[string]string)
					}
					c.Shortcuts["notification_settings"] = alternate
					break
				}
			}
			if c.Shortcuts["notification_settings"] == "" {
				return fmt.Errorf("no free shortcut for notification settings; assign notification_settings explicitly")
			}
		}
	}
	seen := make(map[string]string)
	for action, fallback := range DefaultShortcuts {
		binding := fallback
		if c.Shortcuts[action] != "" {
			binding = c.Shortcuts[action]
		}
		if previous := seen[binding]; previous != "" {
			if !contextualShortcutPair(previous, action) {
				return fmt.Errorf("shortcut %q is assigned to both %s and %s", binding, previous, action)
			}
		}
		seen[binding] = action
	}
	return nil
}

// These pairs live in mutually exclusive TUI regions. Sharing their initial
// keys does not make either action ambiguous to the operator.
func contextualShortcutPair(a, b string) bool {
	return a == "detail_jump" && b == "list_groups" || a == "list_groups" && b == "detail_jump" ||
		a == "detail_search" && b == "list_search" || a == "list_search" && b == "detail_search"
}

// Ordinary actions use lowercase letters or control keys; uppercase letters are
// reserved for important or destructive actions. Keep defaults conflict-free and
// preserve explicit user overrides. Pane prefix arrows follow spatial layout;
// PageUp/PageDown switch tabs.
var DefaultShortcuts = map[string]string{
	"help": "?", "quit": "q", "host_actions": "h", "host_add": "a", "host_remove": "A", "notification_settings": "ctrl-o",
	"session_create": "c", "session_actions": "m", "session_notifications": "n", "session_yield": "y", "session_yield_wait": "Y",
	"session_end": "E", "session_restart": "R", "session_destroy": "X", "list_search": "/", "list_organize": "o", "list_groups": "g",
	"list_reorder_up": "ctrl-k", "list_reorder_down": "ctrl-j", "pty_copy": "v", "pty_unfocus": "ctrl-]", "refresh": "r", "shortcut_settings": "s",
	"project_focus":    "b",
	"pane_prefix":      "ctrl-b",
	"project_hosts":    "w",
	"project_prev_tab": "pageup", "project_next_tab": "pagedown", "project_prev_pane": "(", "project_next_pane": ")",
	"project_add_pane":           "p",
	"project_create":             "e",
	"project_delete":             "Z",
	"project_notification_focus": "i",
	"project_move_pane":          "u", "project_detach_pane": "x",
	"detail_list": "l", "detail_search": "/", "detail_jump": "g", "detail_filter": "f",
	"detail_next": "j", "detail_previous": "k", "detail_focus": "enter",
	"list_sort": "t", "list_sort_direction": "ctrl-t",
}

func validShortcutBinding(binding string) bool {
	if binding == "enter" || binding == "pageup" || binding == "pagedown" {
		return true
	}
	if strings.HasPrefix(binding, "ctrl-") {
		runes := []rune(strings.TrimPrefix(binding, "ctrl-"))
		return len(runes) == 1 && (runes[0] >= 'a' && runes[0] <= 'z' || runes[0] >= 'A' && runes[0] <= 'Z' || runes[0] == ']')
	}
	runes := []rune(binding)
	return len(runes) == 1 && !unicode.IsControl(runes[0]) && !unicode.Is(unicode.Cf, runes[0])
}

func canonicalShortcutBinding(binding string) string {
	// Enter and Ctrl-M produce the same terminal byte.
	if strings.EqualFold(binding, "ctrl-m") {
		return "enter"
	}
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

func (c *Config) FocusThreshold() NotificationLevel {
	if c == nil || c.OtherProjectThreshold == "" {
		return NotificationSystem
	}
	return c.OtherProjectThreshold
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

func (c *Config) PromoteUnread() bool {
	return c == nil || c.PromoteUnreadSessions == nil || *c.PromoteUnreadSessions
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
	clone.SkillSources = append([]SkillTrackingSource(nil), c.SkillSources...)
	clone.Shortcuts = make(map[string]string, len(c.Shortcuts))
	for action, binding := range c.Shortcuts {
		clone.Shortcuts[action] = binding
	}
	clone.NotificationLevels = cloneNotificationLevels(c.NotificationLevels)
	clone.NotificationSounds = make(map[NotificationClass]string, len(c.NotificationSounds))
	for class, path := range c.NotificationSounds {
		clone.NotificationSounds[class] = path
	}
	for i := range clone.Clients {
		clone.Clients[i].NotificationLevels = cloneNotificationLevels(c.Clients[i].NotificationLevels)
		clone.Clients[i].SkillTargets = append([]SkillInstallTarget(nil), c.Clients[i].SkillTargets...)
		for j := range clone.Clients[i].SkillTargets {
			clone.Clients[i].SkillTargets[j].Skills = append([]ManagedHostSkill(nil), c.Clients[i].SkillTargets[j].Skills...)
		}
		clone.Clients[i].SelectedSkills = append([]string(nil), c.Clients[i].SelectedSkills...)
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
	if c.PromoteUnreadSessions != nil {
		value := *c.PromoteUnreadSessions
		clone.PromoteUnreadSessions = &value
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
	if err := ValidateNotificationLevels(c.NotificationLevels); err != nil {
		return err
	}
	c.Name = strings.TrimSpace(c.Name)
	c.Host = strings.TrimSpace(c.Host)
	c.User = strings.TrimSpace(c.User)
	c.Group = strings.TrimSpace(c.Group)
	c.Ducklion = strings.TrimSpace(c.Ducklion)
	c.SSH = strings.TrimSpace(c.SSH)
	for i := range c.SkillTargets {
		if err := c.SkillTargets[i].Normalize(); err != nil {
			return fmt.Errorf("skill target %d: %w", i+1, err)
		}
	}
	seenTargets := make(map[string]bool, len(c.SkillTargets))
	for _, target := range c.SkillTargets {
		if seenTargets[target.ID] {
			return fmt.Errorf("duplicate skill target %q", target.ID)
		}
		seenTargets[target.ID] = true
	}
	seenSkills := make(map[string]bool, len(c.SelectedSkills))
	for i, skill := range c.SelectedSkills {
		c.SelectedSkills[i] = strings.TrimSpace(skill)
		if !SafeIdentifier(c.SelectedSkills[i]) {
			return fmt.Errorf("invalid selected skill %q", skill)
		}
		if seenSkills[c.SelectedSkills[i]] {
			return fmt.Errorf("duplicate selected skill %q", c.SelectedSkills[i])
		}
		seenSkills[c.SelectedSkills[i]] = true
	}
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

func (s *SkillTrackingSource) Normalize() error {
	s.SkillID = strings.TrimSpace(s.SkillID)
	if !SafeIdentifier(s.SkillID) {
		return fmt.Errorf("invalid skill ID %q", s.SkillID)
	}
	s.URL = strings.TrimSpace(s.URL)
	u, err := url.Parse(s.URL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("skill source URL must be an HTTPS URL")
	}
	return nil
}

func (t *SkillInstallTarget) Normalize() error {
	t.ID = strings.TrimSpace(t.ID)
	t.Path = strings.TrimSpace(t.Path)
	if !SafeIdentifier(t.ID) {
		return fmt.Errorf("invalid skill target identifier %q", t.ID)
	}
	if t.Path == "" || !filepath.IsAbs(t.Path) || strings.ContainsRune(t.Path, 0) {
		return fmt.Errorf("skill target %q path must be absolute", t.ID)
	}
	seen := make(map[string]bool, len(t.Skills))
	for i := range t.Skills {
		skill := &t.Skills[i]
		skill.ID = strings.TrimSpace(skill.ID)
		skill.Management = strings.ToLower(strings.TrimSpace(skill.Management))
		if !SafeIdentifier(skill.ID) {
			return fmt.Errorf("invalid managed skill %q", skill.ID)
		}
		if skill.Management != "none" && skill.Management != "push" && skill.Management != "pull" {
			return fmt.Errorf("managed skill %q must use none, push, or pull", skill.ID)
		}
		if seen[skill.ID] {
			return fmt.Errorf("duplicate managed skill %q", skill.ID)
		}
		seen[skill.ID] = true
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
