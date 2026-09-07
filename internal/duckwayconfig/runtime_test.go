package duckwayconfig

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRuntimeSettingsDefaultsWhenConfigMissing(t *testing.T) {
	settings, err := LoadRuntimeSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if settings.PTYLogRetentionDays != 7 || settings.PTYLogRetention() != 7*24*time.Hour {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestLoadRuntimeSettingsReadsSharedDuckwayConfig(t *testing.T) {
	dir := t.TempDir()
	data := []byte("server_url: https://duckway.example\npty_log_retention_days: 14\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadRuntimeSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if settings.PTYLogRetentionDays != 14 || settings.PTYLogRetention() != 14*24*time.Hour {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestLoadRuntimeSettingsRejectsUnsafeRetention(t *testing.T) {
	for _, value := range []string{"-1", "3651"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("pty_log_retention_days: "+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRuntimeSettings(dir); err == nil {
			t.Fatalf("retention %s accepted", value)
		}
	}
}
