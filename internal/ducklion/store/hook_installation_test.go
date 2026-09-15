package store

import (
	"context"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestHookActivationFencesInstallationAndSurvivesRestart(t *testing.T) {
	database, session := openActivityTestStore(t)
	ctx := context.Background()
	check := func(want string) {
		t.Helper()
		if got, err := database.HookActivation(ctx, "codex"); err != nil || got != want {
			t.Fatalf("activation=%q want=%q err=%v", got, want, err)
		}
	}
	record := func(source string, generation, id uint64) {
		t.Helper()
		if _, _, err := database.RecordAgentActivityWithSource(ctx, session.ID, model.NotificationTaskCompleted, generation, id, 0, source); err != nil {
			t.Fatal(err)
		}
	}
	set := func(installed bool) {
		t.Helper()
		if err := database.SetHookInstallation(ctx, "codex", installed); err != nil {
			t.Fatal(err)
		}
	}
	reset := func() {
		t.Helper()
		if err := database.InvalidateHookInstallation(ctx, "codex"); err != nil {
			t.Fatal(err)
		}
	}
	record("codex", 2, 1) // Historical callback predates installation.
	reset()
	set(true)
	check("pending")
	record("codex", 2, 1) // Replay cannot activate a fresh epoch.
	record("codex", 1, 9) // Neither can an older generation.
	record("claude", 2, 2)
	check("pending")
	record("codex", 2, 3)
	check("operational")
	set(true) // Idempotent installation retains verification.
	check("operational")
	path := database.path
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	database, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	check("operational")
	reset()               // Before writing new settings, old verification is invalidated.
	record("codex", 2, 4) // A callback during the write cannot activate it.
	set(true)
	check("pending")
	record("codex", 2, 5)
	check("operational")
	set(false)
	record("codex", 2, 6) // Callback while removed remains historical only.
	reset()
	set(true)
	check("pending")
	if callback, found, err := database.LastHookCallback(ctx, "codex"); err != nil || !found || callback.LastEventID != 6 {
		t.Fatalf("historical callback lost: %+v found=%v err=%v", callback, found, err)
	}
}
