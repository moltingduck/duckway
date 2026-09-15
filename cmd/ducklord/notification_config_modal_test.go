package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func notificationConfigTestState(t *testing.T) (*tuiState, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &ducklord.Config{Name: "desk", Clients: []ducklord.Client{{Name: "host", Host: "host"}}}
	if err := ducklord.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return &tuiState{cfg: cfg, cfgPath: path}, path
}

func TestNotificationConfigGlobalThenHostPreservesPendingDiskEdit(t *testing.T) {
	state, path := notificationConfigTestState(t)
	state.beginNotificationConfig("global", "")
	if !state.notificationConfigMode {
		t.Fatal(state.outputErr)
	}
	state.handleNotificationConfigInput([]byte("\r")) // completed level
	state.handleNotificationConfigInput([]byte("j"))  // indicator -> sound
	state.handleNotificationConfigInput([]byte("\r"))
	if got := state.notificationConfigDraft.NotificationLevels[ducklord.NotificationCompleted]; got != ducklord.NotificationSound {
		t.Fatalf("global level was not staged: %s", got)
	}
	state.handleNotificationConfigInput([]byte("s"))
	if state.notificationConfigStep != "restart" {
		t.Fatal(state.notificationConfigErr)
	}
	state.handleNotificationConfigInput([]byte("\x1b")) // defer restart
	if state.cfg.NotificationLevels[ducklord.NotificationCompleted] != "" {
		t.Fatal("live config changed without restart")
	}
	state.beginNotificationConfig("host", "host")
	state.handleNotificationConfigInput([]byte("\r"))
	for range 4 { // inherit -> system
		state.handleNotificationConfigInput([]byte("j"))
	}
	state.handleNotificationConfigInput([]byte("\r"))
	state.handleNotificationConfigInput([]byte("s"))
	if state.notificationConfigStep != "restart" {
		t.Fatal(state.notificationConfigErr)
	}
	saved, err := ducklord.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := saved.Client("host")
	if saved.NotificationLevels[ducklord.NotificationCompleted] != ducklord.NotificationSound ||
		host.NotificationLevels[ducklord.NotificationCompleted] != ducklord.NotificationSystem {
		t.Fatalf("sequential settings save lost a change: global=%v host=%v", saved.NotificationLevels, host.NotificationLevels)
	}
}

func TestNotificationConfigRejectsExternalEditAndInvalidSoundPath(t *testing.T) {
	state, path := notificationConfigTestState(t)
	state.beginNotificationConfig("global", "")
	for range 4 {
		state.handleNotificationConfigInput([]byte("j"))
	}
	state.handleNotificationConfigInput([]byte("\r"))
	state.handleNotificationConfigInput([]byte("relative.mp3"))
	state.handleNotificationConfigInput([]byte("\r"))
	if state.notificationConfigStep != "path" || !strings.Contains(state.notificationConfigErr, "absolute") {
		t.Fatal("relative local sound path was accepted")
	}
	state.handleNotificationConfigInput([]byte("\x1b"))
	state.notificationConfigErr = ""
	if err := ducklord.SaveConfig(path, &ducklord.Config{Name: "other", Clients: []ducklord.Client{{Name: "host", Host: "host"}}}); err != nil {
		t.Fatal(err)
	}
	state.handleNotificationConfigInput([]byte("s"))
	if state.notificationConfigStep != "list" || !strings.Contains(state.notificationConfigErr, "changed on disk") {
		t.Fatal("external config edit was overwritten")
	}
	var out bytes.Buffer
	state.renderNotificationConfigModal(&out, 100, 30)
	if !strings.Contains(out.String(), "Global notification settings") {
		t.Fatal("notification editor did not render a centered modal")
	}
}

func TestNotificationConfigRejectsMissingAndLinkedSound(t *testing.T) {
	state, _ := notificationConfigTestState(t)
	state.beginNotificationConfig("global", "")
	for range 4 {
		state.handleNotificationConfigInput([]byte("j"))
	}
	state.handleNotificationConfigInput([]byte("\r"))
	missing := filepath.Join(t.TempDir(), "missing.wav")
	state.handleNotificationConfigInput([]byte(missing))
	state.handleNotificationConfigInput([]byte("\r"))
	if state.notificationConfigStep != "path" || !strings.Contains(state.notificationConfigErr, "cannot read") {
		t.Fatalf("missing sound accepted: step=%s err=%s", state.notificationConfigStep, state.notificationConfigErr)
	}
	real := filepath.Join(t.TempDir(), "real.wav")
	if err := os.WriteFile(real, []byte("sound"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.wav")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	state.notificationConfigPath = link
	state.handleNotificationConfigInput([]byte("\r"))
	if state.notificationConfigStep != "path" || !strings.Contains(state.notificationConfigErr, "regular file") {
		t.Fatalf("symlinked sound accepted: step=%s err=%s", state.notificationConfigStep, state.notificationConfigErr)
	}
}

func TestNotificationSettingsShortcutOpensGlobalEditor(t *testing.T) {
	state, _ := notificationConfigTestState(t)
	if action := state.handleInput([]byte("\x0f")); action != "notification-settings" {
		t.Fatalf("global settings shortcut returned %q", action)
	}
	state.beginNotificationConfig("global", "")
	if !state.notificationConfigMode {
		t.Fatal(state.outputErr)
	}
}
