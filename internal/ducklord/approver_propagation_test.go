package ducklord

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestApproverTokenIsNeverPropagated pins the rule that the approver token
// stays in ducklord's own approver.json. Ducklord copies things to Ducklion
// hosts, agent containers and session environments in a few places (remote
// runner and install over ssh, skills deploy, file exchange, project transfer,
// podman demo scripts). None of them may touch the credentials, and the Config
// that the rest of ducklord serializes must not carry them.
func TestApproverTokenIsNeverPropagated(t *testing.T) {
	root := filepath.Join("..", "..")
	// Only these files may mention the credentials store.
	allowed := map[string]bool{
		"internal/ducklord/approver_credentials.go": true,
		"internal/ducklord/approver_client.go":      true,
		"cmd/ducklord/approver_cli.go":              true,
		"cmd/ducklord/tui_approvals.go":             true,
		"cmd/ducklord/tui_loop_approvals.go":        true,
		"cmd/ducklord/tui_loop_keys_approvals.go":   true,
		"cmd/ducklord/tui_state.go":                 true,
		"cmd/ducklord/tui_run.go":                   true,
		"internal/ducklion/manager.go":              true, // scrubSessionEnv drops the token from session environments
	}
	needles := []string{"ApproverCredentials", "approver.json", "DUCKLORD_APPROVER", "ApproverClient", "LoadApproverCredentials"}
	var offenders []string
	for _, dir := range []string{"cmd", "internal", "scripts"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if ext := filepath.Ext(path); ext != ".go" && ext != ".sh" {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			data, rerr := os.ReadFile(path)
			if rerr != nil || allowed[rel] {
				return nil
			}
			text := string(data)
			for _, needle := range needles {
				// Server-side packages legitimately define the approver
				// token store; only ducklord-side code is checked.
				if strings.Contains(text, needle) && !strings.HasPrefix(rel, "internal/server/") {
					offenders = append(offenders, rel+": "+needle)
				}
			}
			return nil
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("files outside the approver allowlist reference the approver credentials: %v", offenders)
	}

	// The shared Config (written to config.yaml, copied by demo scripts) has no
	// approver or token field.
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name + " " + typ.Field(i).Tag.Get("yaml"))
		if strings.Contains(name, "approver") || strings.Contains(name, "token") {
			t.Fatalf("Config.%s could carry the approver token", typ.Field(i).Name)
		}
	}

	// The credentials file sits beside config.yaml but is a different file, so
	// copying or mounting config.yaml alone cannot include it.
	t.Setenv("DUCKLORD_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	if filepath.Base(ApproverCredentialsPath()) == filepath.Base(DefaultConfigPath()) {
		t.Fatal("approver credentials must not share config.yaml")
	}
}
