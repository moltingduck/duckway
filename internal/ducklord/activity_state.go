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

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

const activityStateVersion = 1
const maxActivityStateBytes = 1 << 20

type ActivityState struct {
	Version       int                                 `json:"version"`
	Sessions      map[string]SessionNotificationState `json:"notifications"`
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

func DefaultActivityStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ducklord", "state.json")
}

func NewActivityState() *ActivityState {
	return &ActivityState{Version: activityStateVersion, Sessions: make(map[string]SessionNotificationState), ExtraSections: make(map[string]json.RawMessage)}
}

func (s *ActivityState) Clone() *ActivityState {
	if s == nil {
		return NewActivityState()
	}
	clone := &ActivityState{Version: s.Version, Sessions: make(map[string]SessionNotificationState, len(s.Sessions)), ExtraSections: make(map[string]json.RawMessage, len(s.ExtraSections))}
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
	s.ExtraSections = sections
	return nil
}

func (s ActivityState) MarshalJSON() ([]byte, error) {
	sections := make(map[string]json.RawMessage, len(s.ExtraSections)+2)
	for name, raw := range s.ExtraSections {
		if name == "version" || name == "notifications" || !json.Valid(raw) {
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
	if err := state.validate(); err != nil {
		return s.recoverCorrupt(path, err)
	}
	return &state, nil
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
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
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
