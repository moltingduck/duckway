package client

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProxyExecKeyEnvMissingKeys(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	base := []string{"PATH=/bin", "OPENAI_API_KEY=own-key"}
	if got := ProxyExecKeyEnv(base, t.TempDir(), "codex"); !reflect.DeepEqual(got, base) {
		t.Fatalf("missing assignments changed environment: %v", got)
	}
}

func TestProxyExecKeyEnvAssignmentsAndNativeOAuth(t *testing.T) {
	for _, tc := range []struct {
		name, command   string
		native, wantKey bool
	}{
		{"api Codex", "/bin/codex", false, true},
		{"native Codex", "/bin/codex", true, false},
		{"other command", "sh", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			dir := t.TempDir()
			if err := os.WriteFile(KeysEnvPath(dir), []byte("# synced\nOPENAI_API_KEY=first\nINVALID-NAME=value\nOPENAI_API_KEY=assigned\nANTHROPIC_API_KEY=$(literal)\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.native {
				if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"id_token":"present"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			base := []string{"PATH=/bin", "OPENAI_API_KEY=old", "OPENAI_API_KEY=duplicate", "OTHER=preserved"}
			got := ProxyExecKeyEnv(base, dir, tc.command)
			counts := map[string]int{}
			values := map[string]string{}
			for _, kv := range got {
				key, value, _ := strings.Cut(kv, "=")
				counts[key]++
				values[key] = value
			}
			if tc.wantKey && (counts["OPENAI_API_KEY"] != 1 || values["OPENAI_API_KEY"] != "assigned") {
				t.Fatalf("assignment did not replace inherited keys: %v", got)
			}
			if !tc.wantKey && counts["OPENAI_API_KEY"] != 0 {
				t.Fatal("native OAuth inherited an API key")
			}
			if values["ANTHROPIC_API_KEY"] != "$(literal)" || counts["INVALID-NAME"] != 0 || values["OTHER"] != "preserved" || values["PATH"] != "/bin" {
				t.Fatalf("assignment parsing or environment preservation failed: %v", got)
			}
			if base[1] != "OPENAI_API_KEY=old" {
				t.Fatal("caller environment mutated")
			}
		})
	}
}
