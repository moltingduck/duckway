package services

import (
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestDefaultStatuslineScriptEmbedded(t *testing.T) {
	// The embed must succeed at build time, but a stray rename of
	// statusline_default.sh would leave us with an empty string and
	// agents would get a useless statusline. Pin a few markers.
	if len(DefaultStatuslineScript) == 0 {
		t.Fatal("DefaultStatuslineScript is empty — embed broken")
	}
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"workspace.current_dir",
		"rate_limits.five_hour",
		"rate_limits.seven_day",
		"basename",
	} {
		if !strings.Contains(DefaultStatuslineScript, want) {
			t.Errorf("default script missing marker %q", want)
		}
	}
}

func TestSeedDefaultStatusline_InstallsOnFirstRun(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	q := queries.NewSettingsQueries(db)

	// Pre-seed state: empty. After seed: script populated + sentinel set.
	if got := q.Get(queries.SettingAgentStatuslineScript); got != "" {
		t.Fatalf("pre-seed script = %q, want empty", got)
	}
	SeedDefaultStatusline(q)
	if got := q.Get(queries.SettingAgentStatuslineScript); got != DefaultStatuslineScript {
		t.Errorf("after seed: script mismatch (len got=%d want=%d)", len(got), len(DefaultStatuslineScript))
	}
	if got := q.Get(settingStatuslineSeeded); got != "1" {
		t.Errorf("sentinel = %q, want 1", got)
	}
}

func TestSeedDefaultStatusline_RespectsAdminClear(t *testing.T) {
	// Once the sentinel is set, an admin who cleared the textarea
	// (script="") must see their empty value preserved across
	// SeedDefaultStatusline calls — otherwise the next server restart
	// would silently re-enable the feature.
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	q := queries.NewSettingsQueries(db)

	// First boot: seed.
	SeedDefaultStatusline(q)
	// Admin clears the script.
	_ = q.Set(queries.SettingAgentStatuslineScript, "")
	// Subsequent boot: seed must be a no-op.
	SeedDefaultStatusline(q)

	if got := q.Get(queries.SettingAgentStatuslineScript); got != "" {
		t.Errorf("admin-cleared script got re-seeded: %q", got)
	}
}

func TestSeedDefaultStatusline_RespectsAdminCustomScript(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	q := queries.NewSettingsQueries(db)

	// First boot: seed default.
	SeedDefaultStatusline(q)
	// Admin replaces with their own script.
	custom := "#!/bin/sh\necho hello\n"
	_ = q.Set(queries.SettingAgentStatuslineScript, custom)
	// Subsequent boot must not clobber the admin's script.
	SeedDefaultStatusline(q)

	if got := q.Get(queries.SettingAgentStatuslineScript); got != custom {
		t.Errorf("admin's custom script got clobbered: got=%q", got)
	}
}
