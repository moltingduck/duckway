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
		"export NO_PROXY=localhost,127.0.0.1",
		"export no_proxy=localhost,127.0.0.1",
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
