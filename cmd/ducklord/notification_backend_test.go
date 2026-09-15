package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotificationSettingsShowRuntimeBackendFailures(t *testing.T) {
	state, _ := notificationConfigTestState(t)
	sink := &localNotificationSink{}
	sink.recordBackendResult("sound", errors.New("private decoder details"))
	sink.recordBackendResult("desktop", errors.New("private bus details"))
	state.notificationSink = sink
	state.beginNotificationConfig("global", "")
	var out bytes.Buffer
	state.renderNotificationConfigModal(&out, 140, 12)
	for _, expected := range []string{"sound notification failed", "desktop notification failed"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("settings omitted %q", expected)
		}
	}
	if strings.Contains(out.String(), "private") {
		t.Fatal("settings leaked backend details")
	}
}

func TestSoundWarningSurvivesUnrelatedSuccessfulSound(t *testing.T) {
	sink := &localNotificationSink{}
	sink.recordBackendResult("sound", errors.New("decode failed"), "/bad.ogg")
	sink.recordBackendResult("sound", nil, "/good.ogg")
	if len(sink.Warnings()) != 1 {
		t.Fatal("unrelated sound cleared unresolved failure")
	}
	sink.recordBackendResult("sound", nil, "/bad.ogg")
	if len(sink.Warnings()) != 0 {
		t.Fatal("matching sound recovery did not clear failure")
	}
}

func TestNotificationBackendFailuresAreReportedWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ffplay", "notify-send"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho sensitive-backend-output >&2\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	sound := filepath.Join(dir, "invalid.ogg")
	if err := os.WriteFile(sound, []byte("invalid audio"), 0600); err != nil {
		t.Fatal(err)
	}
	sink := &localNotificationSink{}
	for backend, err := range map[string]error{
		"sound":   playLocalSound(context.Background(), sound),
		"desktop": showDesktopNotification(context.Background(), localNotification{}),
	} {
		if err == nil {
			t.Fatalf("%s failure was swallowed", backend)
		}
		sink.recordBackendResult(backend, err)
	}
	warnings := sink.Warnings()
	if len(warnings) != 2 || !strings.HasPrefix(warnings[0], "sound") || !strings.HasPrefix(warnings[1], "desktop") {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if strings.Contains(strings.Join(warnings, " "), "sensitive") {
		t.Fatal("backend output leaked")
	}
	sink.recordBackendResult("sound", nil)
	if len(sink.Warnings()) != 1 {
		t.Fatal("successful retry did not clear sound warning")
	}
	sink.recordBackendResult("desktop", nil)
	if len(sink.Warnings()) != 0 {
		t.Fatal("successful retry did not clear desktop warning")
	}
	sink.closed = true
	sink.recordBackendResult("sound", errors.New("shutdown"))
	if len(sink.Warnings()) != 0 {
		t.Fatal("shutdown produced a spurious warning")
	}
}
