package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalSoundDetectsPlayerErrorsWithoutLeakingDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wantError    bool
	}{
		{"success", "exit 0", false},
		{"stdout", "printf 'ordinary stdout'", false},
		{"decoder_error_with_zero_exit", "printf 'private decoder path /secret.ogg\\n' >&2; exit 0", true},
		{"nonzero_exit", "exit 1", true},
		{"large_diagnostic", "i=0; while [ $i -lt 2000 ]; do printf 'private decoder diagnostic padding padding padding padding\\n' >&2; i=$((i+1)); done", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ffplay"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			path := filepath.Join(dir, "tone.ogg")
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			err := playLocalSound(context.Background(), path)
			if (err != nil) != tc.wantError {
				t.Fatalf("playLocalSound error = %v, wantError=%v", err, tc.wantError)
			}
			if err != nil && err.Error() != "sound playback failed" {
				t.Fatalf("playback error was not sanitized: %v", err)
			}
		})
	}
}

func TestSoundErrorCaptureBoundsMemoryAndDrainsAllInput(t *testing.T) {
	var capture soundErrorCapture
	input := bytes.Repeat([]byte("x"), 1<<20)
	for range 2 {
		if n, err := capture.Write(input); n != len(input) || err != nil {
			t.Fatalf("diagnostic drain = %d, %v", n, err)
		}
	}
	if capture.n != len(capture.data) || string(capture.data[:]) != strings.Repeat("x", len(capture.data)) {
		t.Fatal("diagnostic capture did not retain only its bounded prefix")
	}
}

func TestLocalSoundPreservesContextErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffplay"), []byte("#!/bin/sh\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, "tone.wav")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := playLocalSound(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline error = %v", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := playLocalSound(ctx, path); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	})
}
