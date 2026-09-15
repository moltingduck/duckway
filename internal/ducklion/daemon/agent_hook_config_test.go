package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAgentHookConfigInvalidatesBeforeWriteAndSkipsUnchangedInstall(t *testing.T) {
	home := t.TempDir()
	wantErr := errors.New("activation persistence failed")
	if _, err := configureAgentHookInHomeBeforeWrite(home, "/bin/ducklion", "codex", "install", func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("invalidation failure=%v", err)
	}
	if installed, err := agentHookInstalledInHome(home, "codex"); err != nil || installed {
		t.Fatalf("settings written despite failed invalidation: installed=%v err=%v", installed, err)
	}
	calls := 0
	beforeWrite := func() error { calls++; return nil }
	for _, action := range []string{"install", "install", "remove", "install"} {
		if _, err := configureAgentHookInHomeBeforeWrite(home, "/bin/ducklion", "codex", action, beforeWrite); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("invalidation calls=%d want 3 (unchanged install preserves verification)", calls)
	}
}

func TestAgentHookConfigInstallPreservesExistingAndRemovesOnlyOwned(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			if installed, err := agentHookInstalledInHome(home, agent); err != nil || installed {
				t.Fatalf("missing settings reported installed: %t %v", installed, err)
			}
			filename := "hooks.json"
			if agent == "claude" {
				filename = "settings.json"
			}
			path := filepath.Join(home, "."+agent, filename)
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte(`{"unrelated":{"secret":"unchanged"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"custom"}]}]}}`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			readBackups := func() map[string][]byte {
				t.Helper()
				paths, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".ducklion-hook-backup-*.json"))
				if err != nil {
					t.Fatal(err)
				}
				backups := make(map[string][]byte, len(paths))
				for _, backup := range paths {
					info, err := os.Stat(backup)
					if err != nil || info.Mode().Perm() != 0600 {
						t.Fatalf("insecure backup %s: %v", backup, err)
					}
					data, err := os.ReadFile(backup)
					if err != nil {
						t.Fatal(err)
					}
					backups[backup] = data
				}
				return backups
			}
			result, err := configureAgentHookInHome(home, "/opt/duck lion/bin/ducklion", agent, "install")
			if err != nil || !result.Installed || !result.Changed || !result.BackupCreated || result.Activation != "pending" {
				t.Fatalf("install: result=%+v err=%v", result, err)
			}
			if installed, err := agentHookInstalledInHome(home, agent); err != nil || !installed {
				t.Fatalf("installed settings not detected: %t %v", installed, err)
			}
			installed, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(installed, []byte("custom")) || !bytes.Contains(installed, []byte("unchanged")) || !bytes.Contains(installed, []byte("__ducklion_agent_hook_v1")) {
				t.Fatalf("settings not merged: %s", installed)
			}
			if agent == "claude" && !bytes.Contains(installed, []byte("StopFailure")) {
				t.Fatal("Claude StopFailure missing")
			}
			installBackups := readBackups()
			if len(installBackups) != 1 {
				t.Fatalf("install created %d backups, want exactly one", len(installBackups))
			}
			for _, data := range installBackups {
				if !bytes.Equal(data, original) {
					t.Fatal("install backup does not preserve exact original bytes")
				}
			}
			result, err = configureAgentHookInHome(home, "/opt/duck lion/bin/ducklion", agent, "install")
			if err != nil || result.Changed || result.BackupCreated {
				t.Fatalf("non-idempotent install: %+v %v", result, err)
			}
			reinstallBackups := readBackups()
			if len(reinstallBackups) != len(installBackups) {
				t.Fatalf("reinstall changed backup count: got %d want %d", len(reinstallBackups), len(installBackups))
			}
			for name, data := range installBackups {
				if !bytes.Equal(reinstallBackups[name], data) {
					t.Fatal("reinstall replaced or changed the original backup")
				}
			}
			result, err = configureAgentHookInHome(home, "/opt/duck lion/bin/ducklion", agent, "remove")
			if err != nil || result.Installed || !result.Changed {
				t.Fatalf("remove: %+v %v", result, err)
			}
			if installed, err := agentHookInstalledInHome(home, agent); err != nil || installed {
				t.Fatalf("removed settings still detected: %t %v", installed, err)
			}
			removed, _ := os.ReadFile(path)
			if bytes.Contains(removed, []byte("__ducklion_agent_hook_v1")) || !bytes.Contains(removed, []byte("custom")) {
				t.Fatalf("removed custom hook: %s", removed)
			}
			removeBackups := readBackups()
			if len(removeBackups) != 2 {
				t.Fatalf("removal left %d backups, want install and removal backups", len(removeBackups))
			}
			for name, data := range installBackups {
				if !bytes.Equal(removeBackups[name], data) {
					t.Fatal("removal replaced or changed the original backup")
				}
			}
			for name, data := range removeBackups {
				if _, exists := installBackups[name]; !exists && !bytes.Equal(data, installed) {
					t.Fatal("removal backup does not preserve exact installed bytes")
				}
			}
		})
	}
}

func TestAgentHookConfigRejectsUnsafeOrMalformedSettings(t *testing.T) {
	for _, scenario := range []string{"symlink", "hardlink", "permissive", "malformed", "directory-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".codex")
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte(`{"hooks":{}}`), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "directory-symlink" {
				if err := os.Symlink(filepath.Dir(outside), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "hooks.json")
				switch scenario {
				case "symlink":
					_ = os.Symlink(outside, path)
				case "hardlink":
					_ = os.Link(outside, path)
				case "permissive":
					_ = os.WriteFile(path, []byte(`{}`), 0644)
				case "malformed":
					_ = os.WriteFile(path, []byte(`{"hooks":{"Stop":{}}}`), 0600)
				}
			}
			if _, err := configureAgentHookInHome(home, "/bin/ducklion", "codex", "install"); err == nil {
				t.Fatal("unsafe settings accepted")
			}
			current, _ := os.ReadFile(outside)
			if !bytes.Equal(current, []byte(`{"hooks":{}}`)) {
				t.Fatal("external file changed")
			}
		})
	}
}

func TestAgentHookConfigRejectsInvalidInputWithoutWriting(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct{ agent, action, exe string }{
		{"other", "install", "/bin/ducklion"},
		{"codex", "toggle", "/bin/ducklion"},
		{"codex", "install", "relative"},
		{"codex", "install", "/bad\npath"},
	} {
		if _, err := configureAgentHookInHome(home, tc.exe, tc.agent, tc.action); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("invalid inputs wrote settings: %v", entries)
	}
}

func TestAgentHookConfigPreservesCustomJSON(t *testing.T) {
	home := t.TempDir()
	_, err := configureAgentHookInHome(home, "/bin/ducklion", "codex", "install")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil || !strings.Contains(string(data), "Stop") {
		t.Fatalf("invalid generated JSON: %s", data)
	}
}

func TestAgentHookConfigUpgradeReplacesOldExecutableAndKeepsUserHandlers(t *testing.T) {
	home := t.TempDir()
	oldExecutable := "/opt/old ducklion/bin/ducklion"
	newExecutable := "/opt/new ducklion/bin/ducklion"
	if _, err := configureAgentHookInHome(home, oldExecutable, "codex", "install"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "hooks.json")
	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	hooks := doc["hooks"].(map[string]any)
	groups := hooks["Stop"].([]any)
	hooks["Stop"] = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "custom"}}})
	data, _ = json.Marshal(doc)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := configureAgentHookInHome(home, newExecutable, "codex", "install")
	if err != nil || !result.Changed {
		t.Fatalf("upgrade failed: %+v %v", result, err)
	}
	data, _ = os.ReadFile(path)
	if bytes.Contains(data, []byte(oldExecutable)) || !bytes.Contains(data, []byte(newExecutable)) || !bytes.Contains(data, []byte("custom")) {
		t.Fatalf("upgrade lost or duplicated handlers: %s", data)
	}
	result, err = configureAgentHookInHome(home, newExecutable, "codex", "remove")
	if err != nil || !result.Changed {
		t.Fatalf("remove failed: %+v %v", result, err)
	}
	data, _ = os.ReadFile(path)
	if bytes.Contains(data, []byte("__ducklion_agent_hook_v1")) || !bytes.Contains(data, []byte("custom")) {
		t.Fatalf("remove touched user handlers: %s", data)
	}
}

func TestAgentHookConfigRemoveAbsentDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	result, err := configureAgentHookInHome(home, "/bin/ducklion", "codex", "remove")
	if err != nil || result.Changed {
		t.Fatalf("remove missing dir: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("remove created directory: %v", err)
	}
	dir := filepath.Join(home, ".codex")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hooks.json")
	old := []byte(`{"notify":"custom"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	result, err = configureAgentHookInHome(home, "/bin/ducklion", "codex", "remove")
	current, _ := os.ReadFile(path)
	if err != nil || result.Changed || !bytes.Equal(current, old) {
		t.Fatalf("remove absent hook changed file: %+v %v %s", result, err, current)
	}
}

func TestAgentHookConfigConcurrentInstallsAreIdempotent(t *testing.T) {
	home := t.TempDir()
	var workers sync.WaitGroup
	errors := make(chan error, 20)
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := configureAgentHookInHome(home, "/bin/ducklion", "codex", "install")
			errors <- err
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if bytes.Count(data, []byte("__ducklion_agent_hook_v1")) != 2 {
		t.Fatalf("duplicate hook after parallel calls: %s", data)
	}
}

func TestAgentHookConfigInstallsSupportedActionNeededEvents(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			_, err := configureAgentHookInHome(home, "/bin/ducklion", agent, "install")
			if err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(home, ".codex", "hooks.json")
			if agent == "claude" {
				filename = filepath.Join(home, ".claude", "settings.json")
			}
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Hooks map[string][]json.RawMessage `json:"hooks"`
			}
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			if len(document.Hooks["PermissionRequest"]) != 1 || !isOwnedHookGroup(document.Hooks["PermissionRequest"][0], agent, "PermissionRequest") {
				t.Fatal("approval hook missing")
			}
			if agent == "claude" {
				if len(document.Hooks["Notification"]) != 1 || !isOwnedHookGroup(document.Hooks["Notification"][0], agent, "Notification") || bytes.Contains(document.Hooks["Notification"][0], []byte("permission_prompt")) {
					t.Fatal("filtered Claude input hook missing or duplicates approval hook")
				}
			} else if len(document.Hooks["Notification"]) != 0 {
				t.Fatal("installed unsupported Codex notification hook")
			}
			if installed, err := agentHookInstalledInHome(home, agent); err != nil || !installed {
				t.Fatalf("installed=%v err=%v", installed, err)
			}
		})
	}
}
