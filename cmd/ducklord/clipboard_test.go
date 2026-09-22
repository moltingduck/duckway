package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClipboardCommandsByHost(t *testing.T) {
	tests := map[string][]string{
		"darwin":  {"pbcopy"},
		"linux":   {"wl-copy", "xclip", "xsel"},
		"windows": {"clip.exe", "powershell.exe"},
	}
	for goos, want := range tests {
		commands := clipboardCommands(goos)
		if len(commands) != len(want) {
			t.Fatalf("%s commands=%+v", goos, commands)
		}
		for i := range want {
			if commands[i].name != want[i] {
				t.Errorf("%s command %d=%q, want %q", goos, i, commands[i].name, want[i])
			}
		}
	}
	if got := clipboardCommands("plan9"); got != nil {
		t.Fatalf("unsupported host commands=%+v", got)
	}
}

func TestWriteNativeClipboardFallsBackAcrossLinuxHelpers(t *testing.T) {
	originalOS, originalRun := clipboardGOOS, clipboardRun
	t.Cleanup(func() { clipboardGOOS, clipboardRun = originalOS, originalRun })
	clipboardGOOS = "linux"
	var calls []string
	clipboardRun = func(_ context.Context, name string, _ []string, body string) error {
		calls = append(calls, name+":"+body)
		if name == "xclip" {
			return nil
		}
		return errors.New("unavailable")
	}
	name, err := writeNativeClipboard("body")
	if err != nil || name != "xclip" {
		t.Fatalf("native clipboard=%q err=%v", name, err)
	}
	if strings.Join(calls, ",") != "wl-copy:body,xclip:body" {
		t.Fatalf("calls=%v", calls)
	}
}

func TestEmitNotesClipboardLinuxKeepsOSC52Fallback(t *testing.T) {
	originalOS, originalRun := clipboardGOOS, clipboardRun
	t.Cleanup(func() { clipboardGOOS, clipboardRun = originalOS, originalRun })
	clipboardGOOS = "linux"
	clipboardRun = func(_ context.Context, name string, _ []string, _ string) error {
		if name == "wl-copy" {
			return nil
		}
		return errors.New("unavailable")
	}
	var out strings.Builder
	status := emitNotesClipboard(&out, "body")
	if !strings.Contains(status, "wl-copy + OSC 52 terminal copy") || out.String() != "\x1b]52;c;Ym9keQ==\x07" {
		t.Fatalf("status=%q output=%q", status, out.String())
	}
}

func TestEmitNotesClipboardNativeFailureUsesOSC52(t *testing.T) {
	originalOS, originalRun := clipboardGOOS, clipboardRun
	t.Cleanup(func() { clipboardGOOS, clipboardRun = originalOS, originalRun })
	clipboardGOOS = "darwin"
	clipboardRun = func(_ context.Context, _ string, _ []string, _ string) error { return errors.New("pbcopy failed") }
	var out strings.Builder
	status := emitNotesClipboard(&out, "body")
	if !strings.Contains(status, "native clipboard unavailable") || out.String() != "\x1b]52;c;Ym9keQ==\x07" {
		t.Fatalf("status=%q output=%q", status, out.String())
	}
}

func TestEmitNotesClipboardDarwinUsesPbcopyAndOSC52(t *testing.T) {
	originalOS, originalRun := clipboardGOOS, clipboardRun
	t.Cleanup(func() { clipboardGOOS, clipboardRun = originalOS, originalRun })
	clipboardGOOS = "darwin"
	var command, copiedBody string
	clipboardRun = func(_ context.Context, name string, _ []string, body string) error {
		command, copiedBody = name, body
		return nil
	}
	var out strings.Builder
	status := emitNotesClipboard(&out, "mac body")
	if command != "pbcopy" || copiedBody != "mac body" {
		t.Fatalf("native command=%q body=%q", command, copiedBody)
	}
	if !strings.Contains(status, "system clipboard (pbcopy + OSC 52 terminal copy)") {
		t.Fatalf("status=%q", status)
	}
	if out.String() != "\x1b]52;c;bWFjIGJvZHk=\x07" {
		t.Fatalf("OSC52 output=%q", out.String())
	}
}
