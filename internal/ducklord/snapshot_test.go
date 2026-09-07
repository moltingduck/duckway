package ducklord

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestSnapshotStoreRoundTripAndAtomicReplacement(t *testing.T) {
	instance := string(model.NewInstanceID())
	session, err := model.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	store := SnapshotStore{Root: filepath.Join(t.TempDir(), "sessions")}
	want := TerminalSnapshot{InstanceID: instance, SessionID: string(session), SavedAt: time.UnixMilli(1234), Payload: []byte("screen one")}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	want.Payload = []byte("screen two")
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(instance, string(session))
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != want.InstanceID || got.SessionID != want.SessionID || !got.SavedAt.Equal(want.SavedAt) || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("snapshot=%+v want=%+v", got, want)
	}
	path, _ := store.path(instance, string(session))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot mode=%o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != string(session)+".snapshot" {
		t.Fatalf("snapshot entries=%v", entries)
	}
}

func TestSnapshotStoreRejectsCorruptionIdentityAndUnsafeFiles(t *testing.T) {
	instance := string(model.NewInstanceID())
	otherInstance := string(model.NewInstanceID())
	store := SnapshotStore{Root: filepath.Join(t.TempDir(), "sessions")}
	if err := store.Save(TerminalSnapshot{InstanceID: instance, SessionID: "ABC123", Payload: []byte("secret")}); err != nil {
		t.Fatal(err)
	}
	path, _ := store.path(instance, "ABC123")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(instance, "ABC123"); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
	if err := store.Save(TerminalSnapshot{InstanceID: instance, SessionID: "ABC123", Payload: []byte("safe")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(otherInstance, "ABC123"); !os.IsNotExist(err) {
		t.Fatalf("wrong instance error=%v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(instance, "ABC123"); err == nil {
		t.Fatal("world-readable snapshot accepted")
	}
}

func TestSnapshotStoreEnforcesPayloadAndIdentifierBounds(t *testing.T) {
	store := SnapshotStore{Root: filepath.Join(t.TempDir(), "sessions")}
	instance := string(model.NewInstanceID())
	if err := store.Save(TerminalSnapshot{InstanceID: instance, SessionID: "bad/path", Payload: nil}); err == nil {
		t.Fatal("unsafe session id accepted")
	}
	if err := store.Save(TerminalSnapshot{InstanceID: instance, SessionID: "ABC123", Payload: make([]byte, MaxSnapshotPayload+1)}); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
}

func TestTerminalRenderStateRejectsControlsAndTruncatesWholeLines(t *testing.T) {
	for _, text := range []string{"safe\x1b[2J", "bell\a", "c1\u009b"} {
		if _, err := EncodeTerminalRenderState(TerminalRenderState{Text: text}); err == nil {
			t.Fatalf("control text accepted: %q", text)
		}
	}
	line := bytes.Repeat([]byte("x"), 1<<20)
	text := string(bytes.Join([][]byte{line, line, line, line, []byte("newest")}, []byte("\n")))
	payload, err := EncodeTerminalRenderState(TerminalRenderState{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > MaxSnapshotPayload {
		t.Fatalf("payload length=%d", len(payload))
	}
	state, err := DecodeTerminalRenderState(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Truncated || !strings.HasSuffix(state.Text, "newest") || strings.HasPrefix(state.Text, string(line)) && len(state.Text) == len(text) {
		t.Fatalf("truncated=%v length=%d suffix=%q", state.Truncated, len(state.Text), state.Text[len(state.Text)-6:])
	}
}
