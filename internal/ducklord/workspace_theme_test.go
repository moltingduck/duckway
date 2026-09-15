package ducklord

import (
	"path/filepath"
	"testing"
)

func TestWorkspaceThemeValidationAndPersistence(t *testing.T) {
	if err := DefaultWorkspaceTheme().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"red", "#abc", "#ffffff\x1b[2J", "#-00001", "#gg0000"} {
		cfg := Config{WorkspaceTheme: WorkspaceTheme{Separator: invalid}}
		if err := cfg.normalize(); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
		if got := cfg.WorkspaceTheme.resolved().Separator; got != DefaultWorkspaceTheme().Separator {
			t.Fatalf("unsafe renderer fallback %q", got)
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Config{WorkspaceTheme: WorkspaceTheme{FocusBackground: "#123ABC"}}
	if err := SaveConfig(path, &cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkspaceTheme.FocusBackground != "#123ABC" {
		t.Fatalf("theme did not round-trip: %+v", loaded.WorkspaceTheme)
	}
	if loaded.WorkspaceTheme.resolved().Foreground != DefaultWorkspaceTheme().Foreground {
		t.Fatal("partial theme failed to inherit defaults")
	}
}
