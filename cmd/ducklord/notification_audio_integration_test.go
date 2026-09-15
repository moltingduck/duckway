package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This exercises the installed encoders and player through SDL's dummy audio
// device. It verifies local decoding/playback integration, not audible hardware.
func TestNotificationAudioIntegration(t *testing.T) {
	if os.Getenv("DUCKLORD_AUDIO_INTEGRATION") != "1" {
		t.Skip("set DUCKLORD_AUDIO_INTEGRATION=1 with ffmpeg and ffplay installed")
	}
	programs := make(map[string]string)
	for _, name := range []string{"ffmpeg", "ffplay"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("audio integration explicitly enabled but %s is unavailable: %v", name, err)
		}
		programs[name] = path
	}
	t.Setenv("SDL_AUDIODRIVER", "dummy")
	t.Setenv("SDL_VIDEODRIVER", "dummy")
	for _, tc := range []struct{ extension, codec string }{
		{"wav", "pcm_s16le"}, {"mp3", "libmp3lame"}, {"ogg", "libvorbis"},
	} {
		t.Run(tc.extension, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "notification."+tc.extension)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, programs["ffmpeg"], "-nostdin", "-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=0.2", "-c:a", tc.codec, path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generate %s fixture: %v\n%s", tc.extension, err, output)
			}
			if info, err := os.Stat(path); err != nil || info.Size() == 0 {
				t.Fatalf("generated %s fixture is missing or empty: %v", tc.extension, err)
			}
			if err := playLocalSound(ctx, path); err != nil {
				t.Fatalf("play real %s fixture with SDL dummy audio: %v", tc.extension, err)
			}
		})
	}
	t.Run("corrupt_audio", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corrupt.ogg")
		if err := os.WriteFile(path, []byte("this is not an Ogg audio stream\n"), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		playErr := playLocalSound(ctx, path)
		if ctx.Err() != nil {
			t.Fatalf("corrupt input stalled notification playback: %v", ctx.Err())
		}
		if playErr == nil {
			t.Fatal("notification playback swallowed the player's corrupt-audio failure")
		}
		if errors.Is(playErr, context.DeadlineExceeded) || errors.Is(playErr, context.Canceled) {
			t.Fatalf("corrupt input timed out instead of reporting a decoder error: %v", playErr)
		}
		sink := &localNotificationSink{}
		sink.recordBackendResult("sound", playErr, path)
		if warnings := sink.Warnings(); len(warnings) != 1 {
			t.Fatalf("corrupt-audio failure did not produce a backend warning: %v", warnings)
		}
	})
}
