package client

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestProxyExecArgsForegroundCompatibility(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' 'Usage: codex --no-daemon'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--", "--no-daemon"}, {"--", "--help"}, {"-c", "--no-daemon"}, {}, {"hello"}, {"pair"}, {"start"}, {"stop"}, {"resume", "--last"}, {"fork", "--last"}, {"remote-control", "-c", "pair"}, {"remote-control"}, {"-c", "model=\"exec\"", "remote-control", "--json"}} {
		input := append([]string{binary}, args...)
		got, err := ProxyExecArgs(input, os.Environ())
		if err != nil || len(got) != len(input)+1 || got[1] != "--no-daemon" {
			t.Fatalf("%v -> %v %v", input, got, err)
		}
	}
	for _, args := range [][]string{{"exec", "hello"}, {"login"}, {"sync"}, {"--help"}, {"daemon", "status"}} {
		input := append([]string{binary}, args...)
		got, err := ProxyExecArgs(input, os.Environ())
		if err != nil || !reflect.DeepEqual(input, got) {
			t.Fatalf("unrelated command changed %v %v", got, err)
		}
	}
	for _, args := range [][]string{{"app-server", "daemon", "restart"}, {"app-server", "proxy"}, {"app-server", "-c", "model=foo", "daemon", "restart"}, {"app-server", "-c", "model=foo", "proxy"}, {"daemon", "restart"}, {"remote-control", "start"}, {"remote-control", "stop"}, {"remote-control", "pair"}} {
		if _, err := ProxyExecArgs(append([]string{binary}, args...), os.Environ()); err == nil {
			t.Fatalf("shared daemon operation accepted: %v", args)
		}
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' 'older codex'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ProxyExecArgs([]string{binary}, os.Environ()); err == nil {
		t.Fatal("unsupported CLI accepted")
	}
}

func TestProxyExecProbeDescendantCleanup(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex")
	pidFile := filepath.Join(dir, "descendant")
	script := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nprintf '%s\\n' --no-daemon\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := ProxyExecArgs([]string{binary}, os.Environ())
	if err == nil {
		t.Fatal("inherited open probe pipes accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("probe did not bound inherited pipes")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// A killed orphan can briefly remain a zombie until the system reaps it.
	deadline := time.Now().Add(time.Second)
	for {
		state, readErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if readErr != nil || strings.Contains(string(state), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe descendant still running: %s", state)
		}
		time.Sleep(time.Millisecond)
	}
}
