package client

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(bufio.NewReader(r))
	return string(out)
}

func TestPrintProxyEnv(t *testing.T) {
	out := captureStdout(t, func() { PrintProxyEnv(18080) })

	want := []string{
		"export HTTP_PROXY=http://localhost:18080",
		"export HTTPS_PROXY=http://localhost:18080",
		"export http_proxy=http://localhost:18080",
		"export https_proxy=http://localhost:18080",
		"export NO_PROXY=" + noProxyBaseline,
		"export no_proxy=" + noProxyBaseline,
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q\n--- got ---\n%s", line, out)
		}
	}

	// Every non-comment line must be a valid `export` statement so the output
	// is safe to eval / append to a shell startup file.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "export ") {
			t.Errorf("non-comment line is not an export statement: %q", line)
		}
	}
}

func TestPrintProxyEnv_CustomPort(t *testing.T) {
	out := captureStdout(t, func() { PrintProxyEnv(9999) })
	if !strings.Contains(out, "export HTTPS_PROXY=http://localhost:9999") {
		t.Errorf("custom port not honored:\n%s", out)
	}
}

// PrintEnv must emit both the placeholder keys AND the HTTP(S)_PROXY exports so
// a single `eval "$(duckway env)"` configures keys and routes traffic through
// the local proxy.
func TestPrintEnv_IncludesProxyVars(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(KeysEnvPath(dir), []byte("OPENAI_API_KEY=sk-test\n# a comment\n\n"), 0600); err != nil {
		t.Fatalf("write keys: %v", err)
	}
	if err := SaveConfig(dir, &Config{ServerURL: "http://srv:8080", ProxyPort: 9999}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var err error
	out := captureStdout(t, func() { err = PrintEnv(dir) })
	if err != nil {
		t.Fatalf("PrintEnv: %v", err)
	}

	want := []string{
		"export OPENAI_API_KEY=sk-test",
		"export HTTP_PROXY=http://localhost:9999",
		"export HTTPS_PROXY=http://localhost:9999",
		"export http_proxy=http://localhost:9999",
		"export https_proxy=http://localhost:9999",
		"export NO_PROXY=" + noProxyBaseline,
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q\n--- got ---\n%s", line, out)
		}
	}

	// Every non-comment line must be a valid `export` statement.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "export ") {
			t.Errorf("non-comment line is not an export statement: %q", line)
		}
	}
}

// downloads.claude.ai must be in NO_PROXY so `claude --update` works even when
// the duckway proxy is not running. Regression guard.
func TestNoProxyBaselineContainsUpdateDomain(t *testing.T) {
	for _, domain := range []string{"localhost", "127.0.0.1", "downloads.claude.ai"} {
		found := false
		for _, entry := range strings.Split(noProxyBaseline, ",") {
			if strings.TrimSpace(entry) == domain {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("noProxyBaseline missing %q (needed so claude --update bypasses the duckway proxy)", domain)
		}
	}

	// ensureBypassInNoProxy must add downloads.claude.ai when it is absent.
	result := ensureBypassInNoProxy("myhost.internal")
	if !strings.Contains(result, "downloads.claude.ai") {
		t.Errorf("ensureBypassInNoProxy did not add downloads.claude.ai: %q", result)
	}
	// Must preserve the user's existing entry.
	if !strings.Contains(result, "myhost.internal") {
		t.Errorf("ensureBypassInNoProxy dropped user entry: %q", result)
	}
}

// When no config exists, PrintEnv falls back to the default proxy port rather
// than failing — the proxy exports are still useful.
func TestPrintEnv_DefaultPortWhenNoConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(KeysEnvPath(dir), []byte("OPENAI_API_KEY=sk-test\n"), 0600); err != nil {
		t.Fatalf("write keys: %v", err)
	}

	var err error
	out := captureStdout(t, func() { err = PrintEnv(dir) })
	if err != nil {
		t.Fatalf("PrintEnv: %v", err)
	}
	if !strings.Contains(out, "export HTTPS_PROXY=http://localhost:18080") {
		t.Errorf("expected default proxy port 18080:\n%s", out)
	}
}
