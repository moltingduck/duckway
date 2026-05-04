package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCCSessionStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewCCSessionStore(dir)
	if got := s.Get("dwch_x"); got != "" {
		t.Errorf("empty store should return empty: %q", got)
	}
	if err := s.Set("dwch_x", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("dwch_x"); got != "sess-1" {
		t.Errorf("Set/Get round-trip: %q", got)
	}
	// Persistence: open a fresh instance
	s2 := NewCCSessionStore(dir)
	if got := s2.Get("dwch_x"); got != "sess-1" {
		t.Errorf("not persisted across instances: %q", got)
	}
}

func TestCCSessionStore_Drop(t *testing.T) {
	dir := t.TempDir()
	s := NewCCSessionStore(dir)
	_ = s.Set("h1", "s1")
	_ = s.Set("h2", "s2")
	_ = s.Drop("h1")
	if got := s.Get("h1"); got != "" {
		t.Errorf("Drop didn't remove: %q", got)
	}
	if got := s.Get("h2"); got != "s2" {
		t.Errorf("Drop affected sibling: %q", got)
	}
}

func TestCCSessionStore_DiskFormat(t *testing.T) {
	dir := t.TempDir()
	s := NewCCSessionStore(dir)
	_ = s.Set("dwch_a", "sess-aaa")
	raw, err := os.ReadFile(filepath.Join(dir, "cc-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["dwch_a"] != "sess-aaa" {
		t.Errorf("disk shape wrong: %v", got)
	}
}

func TestCCSessionStore_Snapshot(t *testing.T) {
	dir := t.TempDir()
	s := NewCCSessionStore(dir)
	_ = s.Set("h1", "s1")
	snap := s.Snapshot()
	snap["h1"] = "mutated"
	if s.Get("h1") != "s1" {
		t.Error("Snapshot returned a live reference instead of a copy")
	}
}
