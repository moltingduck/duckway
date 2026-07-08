package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":                       "''",
		"duckway":                "duckway",
		"/usr/local/bin/duckway": "/usr/local/bin/duckway",
		"https://srv:8080":       "https://srv:8080",
		"/opt/my apps/duckway":   "'/opt/my apps/duckway'",
		"https://srv/?a=b&c=d":   "'https://srv/?a=b&c=d'",
		"a'b":                    `'a'\''b'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSudoUpdateCommand(t *testing.T) {
	got := sudoUpdateCommand("/usr/local/bin/duckway", "https://srv:8080")
	want := "sudo /usr/local/bin/duckway update --server https://srv:8080"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Empty exe (os.Executable failed) falls back to a bare "duckway".
	if got := sudoUpdateCommand("", "https://srv:8080"); got != "sudo duckway update --server https://srv:8080" {
		t.Fatalf("empty-exe fallback wrong: %q", got)
	}

	// Spaces in the path are quoted so the command stays one pasteable token.
	if got := sudoUpdateCommand("/opt/my apps/duckway", "https://srv:8080"); got != "sudo '/opt/my apps/duckway' update --server https://srv:8080" {
		t.Fatalf("spaced path not quoted: %q", got)
	}
}

func TestParseUpdateOptionsRestart(t *testing.T) {
	opts := parseUpdateOptions([]string{"--restart", "--server", "https://srv:8080"}, "")
	if !opts.restartAfter {
		t.Fatal("--restart was not parsed")
	}
	if opts.serverURL != "https://srv:8080" {
		t.Fatalf("serverURL = %q", opts.serverURL)
	}

	opts = parseUpdateOptions([]string{}, "https://env-server")
	if opts.restartAfter {
		t.Fatal("restartAfter should default false")
	}
	if opts.serverURL != "https://env-server" {
		t.Fatalf("env serverURL = %q", opts.serverURL)
	}
}

func TestStatusPrintsVersionContract(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `fmt.Printf("Version:     %s\n", version.Get())`) {
		t.Fatal("cmdStatus must print the duckway client version")
	}
}

func TestConfirmSudoUpdate(t *testing.T) {
	cases := map[string]bool{
		"y\n":      true,
		"Y\n":      true,
		"yes\n":    true,
		" YES \n":  true,
		"\n":       false,
		"n\n":      false,
		"anything": false,
	}
	for input, want := range cases {
		var out bytes.Buffer
		got := confirmSudoUpdate(strings.NewReader(input), &out)
		if got != want {
			t.Errorf("confirmSudoUpdate(%q) = %v, want %v", input, got, want)
		}
		if !strings.Contains(out.String(), "[y/N]") {
			t.Errorf("prompt missing default: %q", out.String())
		}
	}
}

func TestTmuxUnavailableWarning(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	found := func(string) (string, error) { return "/usr/bin/tmux", nil }

	if got := tmuxUnavailableWarning(false, missing); !strings.Contains(got, "tmux is not installed") {
		t.Fatalf("missing tmux warning = %q", got)
	}
	if got := tmuxUnavailableWarning(true, missing); got != "" {
		t.Fatalf("no-tmux should suppress warning, got %q", got)
	}
	if got := tmuxUnavailableWarning(false, found); got != "" {
		t.Fatalf("installed tmux should not warn, got %q", got)
	}
}

func TestLastLines(t *testing.T) {
	got := lastLines("one\ntwo\nthree\nfour\n", 2)
	if got != "three\nfour\n" {
		t.Fatalf("lastLines returned %q", got)
	}
	if got := lastLines("one\ntwo\n", 10); got != "one\ntwo\n" {
		t.Fatalf("lastLines should keep short input, got %q", got)
	}
}

func TestLogTargets(t *testing.T) {
	dir := t.TempDir()
	if got := logTargets(dir, "proxy"); len(got) != 1 || got[0].Name != "proxy" || got[0].Path != filepath.Join(dir, "proxy.log") {
		t.Fatalf("proxy target = %+v", got)
	}
	if got := logTargets(dir, "cc"); len(got) != 1 || got[0].Name != "cc-watch" || got[0].Path != filepath.Join(dir, "cc-watch.log") {
		t.Fatalf("cc target = %+v", got)
	}
	if got := logTargets(dir, "all"); len(got) != 2 {
		t.Fatalf("all targets = %+v", got)
	}
}

func TestProxyRunEnvOverridesProxyVariables(t *testing.T) {
	got := proxyRunEnv([]string{
		"PATH=/bin",
		"HTTP_PROXY=http://old-proxy",
		"https_proxy=http://old-lower",
		"NO_PROXY=chatgpt.com,github.com",
		"no_proxy=api.openai.com",
	}, 19090)
	env := map[string]string{}
	for _, kv := range got {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry missing '=': %q", kv)
		}
		env[key] = value
	}

	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if env[key] != "http://localhost:19090" {
			t.Fatalf("%s = %q, want local proxy", key, env[key])
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if env[key] != "localhost,127.0.0.1,::1" {
			t.Fatalf("%s = %q, want loopback-only no_proxy", key, env[key])
		}
	}
	if env["PATH"] != "/bin" {
		t.Fatalf("PATH was not preserved: %q", env["PATH"])
	}
	if strings.Contains(strings.Join(got, "\n"), "old-proxy") || strings.Contains(strings.Join(got, "\n"), "chatgpt.com") {
		t.Fatalf("old proxy bypass values leaked into env: %#v", got)
	}
}

func TestProxyRunCommandInjectsEnv(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("server_url: http://duckway.test\ntoken: test-token\nproxy_port: 19090\n"), 0600); err != nil {
		t.Fatal(err)
	}

	const sep = "\x1f"
	args := strings.Join([]string{
		"proxy",
		"run",
		"--",
		"sh",
		"-c",
		`printf '%s\n%s\n%s\n' "$HTTPS_PROXY" "$NO_PROXY" "$ALL_PROXY"`,
	}, sep)
	cmd := exec.Command(os.Args[0], "-test.run=TestProxyRunCommandHelper")
	cmd.Env = append(os.Environ(),
		"DUCKWAY_PROXY_RUN_HELPER=1",
		"DUCKWAY_PROXY_RUN_ARGS="+args,
		"DUCKWAY_CONFIG_DIR="+configDir,
		"HTTPS_PROXY=http://old-proxy",
		"NO_PROXY=chatgpt.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy run helper failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := strings.Join([]string{
		"http://localhost:19090",
		"localhost,127.0.0.1,::1",
		"http://localhost:19090",
	}, "\n")
	if got != want {
		t.Fatalf("proxy run output = %q, want %q", got, want)
	}
}

func TestProxyRunCommandHelper(t *testing.T) {
	if os.Getenv("DUCKWAY_PROXY_RUN_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"duckway"}, strings.Split(os.Getenv("DUCKWAY_PROXY_RUN_ARGS"), "\x1f")...)
	main()
	os.Exit(0)
}

func TestPrintLogSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proxy.log"), []byte("proxy one\nproxy two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cc-watch.log"), []byte("cc one\ncc two\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	printLogSnapshot(&out, logTargets(dir, "all"), 1)
	got := out.String()
	for _, want := range []string{
		"==> proxy (",
		"proxy two\n",
		"==> cc-watch (",
		"cc two\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "proxy one") || strings.Contains(got, "cc one") {
		t.Fatalf("snapshot did not respect line limit:\n%s", got)
	}
}
