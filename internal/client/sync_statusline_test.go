package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallStatuslineIntoClaudeSettings_FreshFile(t *testing.T) {
	home := t.TempDir()
	scriptPath := "/home/agent/.duckway/statusline.sh"

	if err := installStatuslineIntoClaudeSettings(home, scriptPath); err != nil {
		t.Fatalf("install: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse settings: %v\nbody: %s", err, body)
	}
	sl, _ := got["statusLine"].(map[string]interface{})
	if sl["type"] != "command" {
		t.Errorf("statusLine.type = %v, want command", sl["type"])
	}
	if sl["command"] != scriptPath {
		t.Errorf("statusLine.command = %v, want %v", sl["command"], scriptPath)
	}

	// Fresh-file path must NOT create a backup (there was nothing to back up).
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json.duckway-backup")); !os.IsNotExist(err) {
		t.Errorf("unexpected backup file created on fresh write")
	}
}

func TestInstallStatuslineIntoClaudeSettings_MergesAndBacksUp(t *testing.T) {
	home := t.TempDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		t.Fatal(err)
	}

	// Pre-existing settings with user prefs that MUST survive the merge.
	existing := map[string]interface{}{
		"theme":                          "dark",
		"skipDangerousModePermissionPrompt": true,
		"env": map[string]interface{}{
			"HTTPS_PROXY": "http://localhost:18080",
		},
		// A pre-existing statusLine we should overwrite.
		"statusLine": map[string]interface{}{
			"type":    "command",
			"command": "/old/path.sh",
		},
	}
	existingBody, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(settingsPath, existingBody, 0644); err != nil {
		t.Fatal(err)
	}

	newScript := "/home/agent/.duckway/statusline.sh"
	if err := installStatuslineIntoClaudeSettings(home, newScript); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Backup must be byte-identical to what was on disk before.
	backup, err := os.ReadFile(settingsPath + ".duckway-backup")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(backup) != string(existingBody) {
		t.Errorf("backup content drift\nwant:\n%s\ngot:\n%s", existingBody, backup)
	}

	// Merged file should keep theme + env + the new statusLine.
	merged, _ := os.ReadFile(settingsPath)
	var got map[string]interface{}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("parse merged: %v\nbody: %s", err, merged)
	}
	if got["theme"] != "dark" {
		t.Errorf("theme lost: %v", got["theme"])
	}
	if got["skipDangerousModePermissionPrompt"] != true {
		t.Errorf("skipDangerousModePermissionPrompt lost: %v", got["skipDangerousModePermissionPrompt"])
	}
	env, _ := got["env"].(map[string]interface{})
	if env["HTTPS_PROXY"] != "http://localhost:18080" {
		t.Errorf("env.HTTPS_PROXY lost: %v", env["HTTPS_PROXY"])
	}
	sl, _ := got["statusLine"].(map[string]interface{})
	if sl["command"] != newScript {
		t.Errorf("statusLine.command = %v, want %v (old value should have been overwritten)", sl["command"], newScript)
	}
}

func TestInstallStatuslineIntoClaudeSettings_GarbledExistingStillBacksUp(t *testing.T) {
	home := t.TempDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("not valid json {{{")
	if err := os.WriteFile(settingsPath, garbage, 0644); err != nil {
		t.Fatal(err)
	}

	if err := installStatuslineIntoClaudeSettings(home, "/x/statusline.sh"); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Backup must exactly mirror the garbage — that's the whole point
	// of the backup (one-step undo even when we can't parse the file).
	backup, _ := os.ReadFile(settingsPath + ".duckway-backup")
	if string(backup) != string(garbage) {
		t.Errorf("backup not byte-identical to original garbage")
	}
	// The settings file should now be valid JSON with our statusLine.
	body, _ := os.ReadFile(settingsPath)
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("post-install settings still not valid JSON: %v\nbody: %s", err, body)
	}
	sl, _ := got["statusLine"].(map[string]interface{})
	if sl["command"] != "/x/statusline.sh" {
		t.Errorf("statusLine.command = %v, want /x/statusline.sh", sl["command"])
	}
}
