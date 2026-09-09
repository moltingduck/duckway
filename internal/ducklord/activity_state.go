package ducklord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/model"
)

const activityStateVersion = 1
const maxActivityStateBytes = 1 << 20

const (
	maxOrganizationSessions = 16384
	maxCustomGroups         = 4096
	maxGroupNameRunes       = 64
)

type OrganizationMode string

const (
	OrganizationCustom OrganizationMode = "custom"
	OrganizationHost   OrganizationMode = "host"
	OrganizationType   OrganizationMode = "type"
	UngroupedGroupID                    = "ungrouped"
)

// SessionIdentity is the stable, host-alias-independent identity used by all
// Ducklord-local organization state.
type SessionIdentity struct {
	InstanceID string `json:"instance_id"`
	SessionID  string `json:"session_id"`
}

func IdentityFromSession(session RemoteSession) (SessionIdentity, bool) {
	instanceID, err := model.ParseInstanceID(session.InstanceID)
	if err != nil {
		return SessionIdentity{}, false
	}
	sessionID, err := model.ParseSessionID(session.SessionID)
	if err != nil {
		return SessionIdentity{}, false
	}
	return SessionIdentity{InstanceID: string(instanceID), SessionID: string(sessionID)}, true
}

func (i SessionIdentity) Key() string {
	if err := i.validate(); err != nil {
		return ""
	}
	return i.InstanceID + "/" + i.SessionID
}

func (i SessionIdentity) MarshalText() ([]byte, error) {
	if err := i.validate(); err != nil {
		return nil, err
	}
	return []byte(i.InstanceID + "/" + i.SessionID), nil
}

func (i *SessionIdentity) UnmarshalText(text []byte) error {
	if i == nil {
		return fmt.Errorf("session identity is nil")
	}
	parts := strings.Split(string(text), "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid session identity")
	}
	instanceID, err := model.ParseInstanceID(parts[0])
	if err != nil {
		return err
	}
	sessionID, err := model.ParseSessionID(parts[1])
	if err != nil {
		return err
	}
	*i = SessionIdentity{InstanceID: string(instanceID), SessionID: string(sessionID)}
	return nil
}

type CustomGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type OrganizationState struct {
	Mode         OrganizationMode              `json:"mode,omitempty"`
	SessionOrder []SessionIdentity             `json:"session_order,omitempty"`
	Groups       []CustomGroup                 `json:"groups,omitempty"`
	Membership   map[SessionIdentity]string    `json:"membership,omitempty"`
	GroupOrders  map[OrganizationMode][]string `json:"group_orders,omitempty"`
}

func newOrganizationState() OrganizationState {
	return OrganizationState{Mode: OrganizationCustom, Membership: make(map[SessionIdentity]string), GroupOrders: make(map[OrganizationMode][]string)}
}

func (o OrganizationState) clone() OrganizationState {
	clone := OrganizationState{Mode: o.Mode, SessionOrder: append([]SessionIdentity(nil), o.SessionOrder...), Groups: append([]CustomGroup(nil), o.Groups...),
		Membership: make(map[SessionIdentity]string, len(o.Membership)), GroupOrders: make(map[OrganizationMode][]string, len(o.GroupOrders))}
	for identity, groupID := range o.Membership {
		clone.Membership[identity] = groupID
	}
	for mode, order := range o.GroupOrders {
		clone.GroupOrders[mode] = append([]string(nil), order...)
	}
	return clone
}

func (m OrganizationMode) valid() bool {
	return m == OrganizationCustom || m == OrganizationHost || m == OrganizationType
}

func (m OrganizationMode) Valid() bool { return m.valid() }

func (i SessionIdentity) validate() error {
	instanceID, err := model.ParseInstanceID(i.InstanceID)
	if err != nil || string(instanceID) != i.InstanceID {
		return fmt.Errorf("invalid canonical organization instance id %q", i.InstanceID)
	}
	sessionID, err := model.ParseSessionID(i.SessionID)
	if err != nil || string(sessionID) != i.SessionID {
		return fmt.Errorf("invalid canonical organization session id %q", i.SessionID)
	}
	return nil
}

func validateCustomGroupName(name string) error {
	if name != strings.TrimSpace(name) || !utf8.ValidString(name) {
		return fmt.Errorf("custom group name must be trimmed valid UTF-8")
	}
	count := 0
	for _, r := range name {
		count++
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("custom group name contains a control or format character")
		}
	}
	if count < 1 || count > maxGroupNameRunes {
		return fmt.Errorf("custom group name must contain 1 to %d Unicode code points", maxGroupNameRunes)
	}
	return nil
}

func ValidateCustomGroupName(name string) error { return validateCustomGroupName(name) }

func (o *OrganizationState) validate() error {
	if o.Mode == "" {
		o.Mode = OrganizationCustom
	}
	if !o.Mode.valid() {
		return fmt.Errorf("invalid organization mode %q", o.Mode)
	}
	if len(o.SessionOrder) > maxOrganizationSessions || len(o.Membership) > maxOrganizationSessions {
		return fmt.Errorf("organization state has too many sessions")
	}
	seenSessions := make(map[SessionIdentity]bool, len(o.SessionOrder))
	for _, identity := range o.SessionOrder {
		if err := identity.validate(); err != nil {
			return err
		}
		if seenSessions[identity] {
			return fmt.Errorf("duplicate session identity in organization order")
		}
		seenSessions[identity] = true
	}
	if len(o.Groups) > maxCustomGroups {
		return fmt.Errorf("organization state has too many custom groups")
	}
	groupIDs := make(map[string]bool, len(o.Groups))
	for _, group := range o.Groups {
		parsed, err := uuid.Parse(group.ID)
		if err != nil || parsed.String() != group.ID || group.ID == UngroupedGroupID {
			return fmt.Errorf("invalid canonical custom group id %q", group.ID)
		}
		if groupIDs[group.ID] {
			return fmt.Errorf("duplicate custom group id %q", group.ID)
		}
		if err := validateCustomGroupName(group.Name); err != nil {
			return fmt.Errorf("custom group %s: %w", group.ID, err)
		}
		groupIDs[group.ID] = true
	}
	for identity, groupID := range o.Membership {
		if err := identity.validate(); err != nil {
			return err
		}
		if !groupIDs[groupID] {
			return fmt.Errorf("organization membership references unknown custom group %q", groupID)
		}
	}
	for mode, order := range o.GroupOrders {
		if !mode.valid() {
			return fmt.Errorf("invalid organization group-order mode %q", mode)
		}
		if len(order) > maxCustomGroups+1 {
			return fmt.Errorf("organization group order is too large")
		}
		seen := make(map[string]bool, len(order))
		for _, id := range order {
			if id == "" || seen[id] {
				return fmt.Errorf("invalid or duplicate %s group-order identity %q", mode, id)
			}
			switch mode {
			case OrganizationCustom:
				if id != UngroupedGroupID && !groupIDs[id] {
					return fmt.Errorf("custom group order references unknown group %q", id)
				}
			case OrganizationHost:
				if !SafeIdentifier(id) {
					return fmt.Errorf("invalid host group identity %q", id)
				}
			case OrganizationType:
				if id != string(model.KindShell) && id != string(model.KindAgent) {
					return fmt.Errorf("invalid session-kind group identity %q", id)
				}
			}
			seen[id] = true
		}
	}
	if o.Membership == nil {
		o.Membership = make(map[SessionIdentity]string)
	}
	if o.GroupOrders == nil {
		o.GroupOrders = make(map[OrganizationMode][]string)
	}
	return nil
}

type ActivityState struct {
	Version       int                                 `json:"version"`
	Sessions      map[string]SessionNotificationState `json:"notifications"`
	Organization  OrganizationState                   `json:"organization,omitempty"`
	ExtraSections map[string]json.RawMessage          `json:"-"`
}

type SessionNotificationState struct {
	Seen     map[model.NotificationCategory]uint64 `json:"seen,omitempty"`
	Disabled map[model.NotificationCategory]bool   `json:"disabled,omitempty"`
	Unread   map[model.NotificationCategory]bool   `json:"unread,omitempty"`
}

type ActivityStateStore struct{ Path string }

// CorruptStateRecoveredError is a non-fatal load warning. Load returns it
// together with a fresh state after preserving the bad file for diagnosis.
type CorruptStateRecoveredError struct {
	PreservedPath string
	Cause         error
}

func (e *CorruptStateRecoveredError) Error() string {
	return fmt.Sprintf("preserved corrupt Ducklord state as %s: %v", e.PreservedPath, e.Cause)
}

func (e *CorruptStateRecoveredError) Unwrap() error { return e.Cause }

// StateLoadWarning reports a repaired, non-fatal state value. Load returns the
// usable state together with this warning so the TUI can surface it locally.
type StateLoadWarning struct{ Message string }

func (e *StateLoadWarning) Error() string { return e.Message }

func DefaultActivityStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ducklord", "state.json")
}

func NewActivityState() *ActivityState {
	return &ActivityState{Version: activityStateVersion, Sessions: make(map[string]SessionNotificationState), Organization: newOrganizationState(), ExtraSections: make(map[string]json.RawMessage)}
}

func (s *ActivityState) Clone() *ActivityState {
	if s == nil {
		return NewActivityState()
	}
	clone := &ActivityState{Version: s.Version, Sessions: make(map[string]SessionNotificationState, len(s.Sessions)), Organization: s.Organization.clone(), ExtraSections: make(map[string]json.RawMessage, len(s.ExtraSections))}
	for name, raw := range s.ExtraSections {
		clone.ExtraSections[name] = append(json.RawMessage(nil), raw...)
	}
	for key, entry := range s.Sessions {
		copied := SessionNotificationState{
			Seen:     make(map[model.NotificationCategory]uint64, len(entry.Seen)),
			Disabled: make(map[model.NotificationCategory]bool, len(entry.Disabled)),
			Unread:   make(map[model.NotificationCategory]bool, len(entry.Unread)),
		}
		for category, sequence := range entry.Seen {
			copied.Seen[category] = sequence
		}
		for category, disabled := range entry.Disabled {
			copied.Disabled[category] = disabled
		}
		for category, unread := range entry.Unread {
			copied.Unread[category] = unread
		}
		clone.Sessions[key] = copied
	}
	return clone
}

func (s *ActivityState) UnmarshalJSON(data []byte) error {
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(data, &sections); err != nil {
		return err
	}
	versionRaw, ok := sections["version"]
	if !ok || json.Unmarshal(versionRaw, &s.Version) != nil {
		return fmt.Errorf("ducklord state version is required")
	}
	delete(sections, "version")
	s.Sessions = make(map[string]SessionNotificationState)
	if raw, ok := sections["notifications"]; ok {
		if err := json.Unmarshal(raw, &s.Sessions); err != nil {
			return fmt.Errorf("decode notification state: %w", err)
		}
		delete(sections, "notifications")
	}
	s.Organization = newOrganizationState()
	if raw, ok := sections["organization"]; ok {
		if err := json.Unmarshal(raw, &s.Organization); err != nil {
			return fmt.Errorf("decode organization state: %w", err)
		}
		delete(sections, "organization")
	}
	s.ExtraSections = sections
	return nil
}

func (s ActivityState) MarshalJSON() ([]byte, error) {
	sections := make(map[string]json.RawMessage, len(s.ExtraSections)+3)
	for name, raw := range s.ExtraSections {
		if name == "version" || name == "notifications" || name == "organization" || !json.Valid(raw) {
			return nil, fmt.Errorf("invalid Ducklord state section %q", name)
		}
		sections[name] = append(json.RawMessage(nil), raw...)
	}
	version, _ := json.Marshal(s.Version)
	notifications, err := json.Marshal(s.Sessions)
	if err != nil {
		return nil, err
	}
	sections["version"] = version
	sections["notifications"] = notifications
	organization, err := json.Marshal(s.Organization)
	if err != nil {
		return nil, err
	}
	sections["organization"] = organization
	return json.Marshal(sections)
}

func (s ActivityStateStore) Load() (*ActivityState, error) {
	path := s.Path
	if path == "" {
		path = DefaultActivityStatePath()
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return NewActivityState(), nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxActivityStateBytes {
		return nil, fmt.Errorf("ducklord state has unsafe type, permissions, or size")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxActivityStateBytes+1))
	if err != nil {
		return nil, err
	}
	var state ActivityState
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&state); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return s.recoverCorrupt(path, fmt.Errorf("decode ducklord state"))
	}
	var warning error
	if state.Organization.Mode == "" {
		state.Organization.Mode = OrganizationCustom
	} else if !state.Organization.Mode.valid() {
		unknown := state.Organization.Mode
		state.Organization.Mode = OrganizationCustom
		warning = &StateLoadWarning{Message: fmt.Sprintf("unknown Ducklord organization mode %q; using custom", unknown)}
	}
	if err := state.validate(); err != nil {
		return s.recoverCorrupt(path, err)
	}
	return &state, warning
}

func (s ActivityStateStore) recoverCorrupt(path string, cause error) (*ActivityState, error) {
	preserved := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UTC().UnixNano())
	if err := os.Rename(path, preserved); err != nil {
		return nil, fmt.Errorf("preserve corrupt Ducklord state: %w", err)
	}
	if directory, err := os.Open(filepath.Dir(path)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return NewActivityState(), &CorruptStateRecoveredError{PreservedPath: preserved, Cause: cause}
}

func (s ActivityStateStore) Save(state *ActivityState) error {
	if state == nil {
		return fmt.Errorf("ducklord activity state is required")
	}
	if err := state.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxActivityStateBytes {
		return fmt.Errorf("ducklord state exceeds 1 MiB")
	}
	path := s.Path
	if path == "" {
		path = DefaultActivityStatePath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("ducklord state directory must be a private directory")
	}
	if info.Mode().Perm()&0077 != 0 {
		if err := os.Chmod(dir, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("ducklord state directory must be private")
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *ActivityState) Reconcile(session RemoteSession, activeFresh bool) (unread, changed bool) {
	key := activitySessionKey(session.InstanceID, session.SessionID)
	if key == "" {
		return false, false
	}
	entry := s.Sessions[key]
	if entry.Seen == nil {
		entry.Seen = make(map[model.NotificationCategory]uint64)
	}
	if entry.Unread == nil {
		entry.Unread = make(map[model.NotificationCategory]bool)
	}
	for category, current := range session.ActivitySequences {
		if activeFresh {
			if current > entry.Seen[category] {
				entry.Seen[category] = current
				changed = true
			}
			if entry.Unread[category] {
				delete(entry.Unread, category)
				changed = true
			}
		} else if entry.Disabled[category] {
			if current > entry.Seen[category] {
				entry.Seen[category] = current
				changed = true
			}
		} else if current > entry.Seen[category] && !entry.Unread[category] {
			entry.Unread[category] = true
			changed = true
		}
	}
	for _, categoryUnread := range entry.Unread {
		unread = unread || categoryUnread
	}
	s.Sessions[key] = entry
	return unread, changed
}

func (s *ActivityState) MarkSeen(session RemoteSession) bool {
	_, changed := s.Reconcile(session, true)
	return changed
}

func (s *ActivityState) SetEnabled(instanceID, sessionID string, category model.NotificationCategory, enabled bool) error {
	key := activitySessionKey(instanceID, sessionID)
	if key == "" || category.Validate() != nil {
		return fmt.Errorf("invalid notification identity or category")
	}
	entry := s.Sessions[key]
	if entry.Disabled == nil {
		entry.Disabled = make(map[model.NotificationCategory]bool)
	}
	if enabled {
		delete(entry.Disabled, category)
	} else {
		entry.Disabled[category] = true
	}
	s.Sessions[key] = entry
	return nil
}

func (s *ActivityState) Enabled(instanceID, sessionID string, category model.NotificationCategory) bool {
	key := activitySessionKey(instanceID, sessionID)
	if key == "" || category.Validate() != nil {
		return false
	}
	return !s.Sessions[key].Disabled[category]
}

func (s *ActivityState) validate() error {
	if s.Version != activityStateVersion {
		return fmt.Errorf("unsupported ducklord state version %d", s.Version)
	}
	if s.Sessions == nil {
		s.Sessions = make(map[string]SessionNotificationState)
	}
	if err := s.Organization.validate(); err != nil {
		return err
	}
	for key, entry := range s.Sessions {
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			return fmt.Errorf("invalid activity session key")
		}
		if _, err := model.ParseInstanceID(parts[0]); err != nil {
			return err
		}
		if _, err := model.ParseSessionID(parts[1]); err != nil {
			return err
		}
		for category := range entry.Seen {
			if err := category.Validate(); err != nil {
				return err
			}
		}
		for category, disabled := range entry.Disabled {
			if err := category.Validate(); err != nil || !disabled {
				return fmt.Errorf("invalid disabled notification category")
			}
		}
		for category, unread := range entry.Unread {
			if err := category.Validate(); err != nil || !unread {
				return fmt.Errorf("invalid unread notification category")
			}
		}
	}
	return nil
}

func activitySessionKey(instanceID, sessionID string) string {
	if _, err := model.ParseInstanceID(instanceID); err != nil {
		return ""
	}
	if _, err := model.ParseSessionID(sessionID); err != nil {
		return ""
	}
	return instanceID + "/" + sessionID
}
