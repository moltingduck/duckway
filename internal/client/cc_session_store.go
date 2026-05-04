package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// CCSessionStore persists the channel_handle ↔ claude session_id map to
// ~/.duckway/cc-sessions.json so the daemon can `claude -p --resume` after
// a restart.
//
// The format is a flat JSON object:
//
//	{"dwch_abc": "sess-...", "dwch_def": "sess-..."}
//
// Concurrent reads + writes from many per-channel workers are safe via
// a single RWMutex. Writes flush to disk synchronously — losing one
// session id on a crash is annoying but recoverable (claude just starts
// a fresh session for that channel).
type CCSessionStore struct {
	path string
	mu   sync.RWMutex
	data map[string]string
}

func NewCCSessionStore(configDir string) *CCSessionStore {
	s := &CCSessionStore{
		path: filepath.Join(configDir, "cc-sessions.json"),
		data: map[string]string{},
	}
	s.load()
	return s
}

func (s *CCSessionStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &s.data)
}

// Get returns the session id for a handle, or "" if none.
func (s *CCSessionStore) Get(handle string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[handle]
}

// Set replaces the session id for a handle and persists to disk.
func (s *CCSessionStore) Set(handle, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[handle] = sessionID
	return s.flush()
}

// Drop removes the mapping (called on channel_delete events).
func (s *CCSessionStore) Drop(handle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, handle)
	return s.flush()
}

// flush writes the current map. Caller holds mu.
func (s *CCSessionStore) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, out, 0600)
}

// Snapshot returns a copy of the map for diagnostics / `!status`-style.
func (s *CCSessionStore) Snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}
