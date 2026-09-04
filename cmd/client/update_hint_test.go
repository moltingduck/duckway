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

const testDuckwayCAPEM = "-----BEGIN CERTIFICATE-----\nduckway\n-----END CERTIFICATE-----\n"

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
	got := sudoUpdateCommand("/usr/local/bin/duckway", "https://srv:8080", false)
	want := "sudo /usr/local/bin/duckway update --server https://srv:8080"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Empty exe (os.Executable failed) falls back to a bare "duckway".
	if got := sudoUpdateCommand("", "https://srv:8080", false); got != "sudo duckway update --server https://srv:8080" {
		t.Fatalf("empty-exe fallback wrong: %q", got)
	}

	// Spaces in the path are quoted so the command stays one pasteable token.
	if got := sudoUpdateCommand("/opt/my apps/duckway", "https://srv:8080", false); got != "sudo '/opt/my apps/duckway' update --server https://srv:8080" {
		t.Fatalf("spaced path not quoted: %q", got)
	}
}

func TestSudoUpdateCommandWithRestartKeepsRestartUserLevel(t *testing.T) {
	got := sudoUpdateCommand("/usr/local/bin/duckway", "https://srv:8080", true)
	want := "sudo /usr/local/bin/duckway update --server https://srv:8080 && /usr/local/bin/duckway restart"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "update --restart") {
		t.Fatalf("restart must not run under sudo/root: %q", got)
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

func TestParseManagedUpdateArtifactOptions(t *testing.T) {
	opts := parseUpdateOptions([]string{
		"--server", "https://srv", "--restart",
		"--expected-version", "v2",
		"--expected-binary", "duckway-client-linux-amd64",
		"--expected-sha256", strings.Repeat("a", 64),
		"--expected-size", "2097152",
		"--expected-ducklion-binary", "ducklion-linux-amd64",
		"--expected-ducklion-sha256", strings.Repeat("b", 64),
		"--expected-ducklion-size", "3145728",
	}, "")
	if opts.serverURL != "https://srv" || !opts.restartAfter || opts.expectedVersion != "v2" || opts.expectedBinary != "duckway-client-linux-amd64" || opts.expectedSHA256 != strings.Repeat("a", 64) || opts.expectedSize != 2097152 ||
		opts.expectedDucklionBinary != "ducklion-linux-amd64" || opts.expectedDucklionSHA256 != strings.Repeat("b", 64) || opts.expectedDucklionSize != 3145728 {
		t.Fatalf("options=%+v", opts)
	}
}

func TestUpdateDoesNotRestartWhenAlreadyUpToDate(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	start := strings.Index(source, "if !updateInfo.UpdateRequired && !updateInfo.UpdateRecommended {")
	if start < 0 {
		t.Fatal("up-to-date branch not found")
	}
	end := strings.Index(source[start:], "\n\tif updateInfo.UpdateRequired {")
	if end < 0 {
		t.Fatal("end of up-to-date branch not found")
	}
	branch := source[start : start+end]
	if strings.Contains(branch, "restartDaemonsRunningBeforeUpdate") {
		t.Fatalf("up-to-date branch must not restart daemons:\n%s", branch)
	}
}

func TestProcessEnvValueReadsProcEnv(t *testing.T) {
	t.Setenv("DUCKWAY_TEST_PROCESS_ENV", "present")
	got := processEnvValue(os.Getpid(), "DUCKWAY_TEST_PROCESS_ENV")
	if got != "" && got != "present" {
		t.Fatalf("process env value = %q", got)
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

	t.Setenv("DUCKWAY_CC_USE_TMUX", "")
	if got := tmuxUnavailableWarning(false, missing); got != "" {
		t.Fatalf("tmux not requested warning = %q", got)
	}
	t.Setenv("DUCKWAY_CC_USE_TMUX", "1")
	if got := tmuxUnavailableWarning(false, missing); !strings.Contains(got, "tmux was requested") {
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
	if got := logTargets(dir, "ducklion"); len(got) != 1 || got[0].Name != "ducklion" || got[0].Path != filepath.Join(dir, "ducklion", "daemon.log") {
		t.Fatalf("ducklion target = %+v", got)
	}
	if got := logTargets(dir, "proxy"); len(got) != 1 || got[0].Name != "proxy" || got[0].Path != filepath.Join(dir, "proxy.log") {
		t.Fatalf("proxy target = %+v", got)
	}
	if got := logTargets(dir, "cc"); len(got) != 1 || got[0].Name != "cc-watch" || got[0].Path != filepath.Join(dir, "cc-watch.log") {
		t.Fatalf("cc target = %+v", got)
	}
	if got := logTargets(dir, "all"); len(got) != 3 {
		t.Fatalf("all targets = %+v", got)
	}
}

func TestProxyExecEnvOverridesProxyVariables(t *testing.T) {
	configDir := t.TempDir()
	got := proxyExecEnv([]string{
		"PATH=/bin",
		"HTTP_PROXY=http://old-proxy",
		"https_proxy=http://old-lower",
		"NO_PROXY=chatgpt.com,github.com",
		"no_proxy=api.openai.com",
	}, 19090, configDir)
	env := map[string]string{}
	for _, kv := range got {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry missing '=': %q", kv)
		}
		env[key] = value
	}

	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if env[key] != "http://127.0.0.1:19090" {
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

func TestProxyExecEnvInjectsCAForNodeClients(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "ca.pem"), []byte(testDuckwayCAPEM), 0600); err != nil {
		t.Fatal(err)
	}

	got := proxyExecEnv([]string{
		"NODE_EXTRA_CA_CERTS=/old/node-ca.pem",
		"SSL_CERT_FILE=/old/ssl-ca.pem",
	}, 19090, configDir)
	env := map[string]string{}
	for _, kv := range got {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			env[key] = value
		}
	}

	wantBundle := filepath.Join(configDir, "agent-ca-bundle.pem")
	for _, key := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"} {
		if env[key] != wantBundle {
			t.Fatalf("%s = %q, want %q", key, env[key], wantBundle)
		}
	}
	if _, err := os.Stat(wantBundle); err != nil {
		t.Fatalf("CA bundle was not written: %v", err)
	}
}

func TestProxyExecCommandInjectsEnv(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("server_url: http://duckway.test\ntoken: test-token\nproxy_port: 19090\n"), 0600); err != nil {
		t.Fatal(err)
	}

	const sep = "\x1f"
	args := strings.Join([]string{
		"proxy",
		"exec",
		"--",
		"sh",
		"-c",
		`printf '%s\n%s\n%s\n' "$HTTPS_PROXY" "$NO_PROXY" "$ALL_PROXY"`,
	}, sep)
	cmd := exec.Command(os.Args[0], "-test.run=TestProxyExecCommandHelper")
	cmd.Env = append(os.Environ(),
		"DUCKWAY_PROXY_EXEC_HELPER=1",
		"DUCKWAY_PROXY_EXEC_ARGS="+args,
		"DUCKWAY_CONFIG_DIR="+configDir,
		"HTTPS_PROXY=http://old-proxy",
		"NO_PROXY=chatgpt.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy exec helper failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := strings.Join([]string{
		"http://127.0.0.1:19090",
		"localhost,127.0.0.1,::1",
		"http://127.0.0.1:19090",
	}, "\n")
	if got != want {
		t.Fatalf("proxy exec output = %q, want %q", got, want)
	}
}

func TestProxyExecCommandHelper(t *testing.T) {
	if os.Getenv("DUCKWAY_PROXY_EXEC_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"duckway"}, strings.Split(os.Getenv("DUCKWAY_PROXY_EXEC_ARGS"), "\x1f")...)
	main()
	os.Exit(0)
}

func TestDuckwayDucklionSubcommand(t *testing.T) {
	dir := t.TempDir()
	ducklion := filepath.Join(dir, "ducklion")
	if err := os.WriteFile(ducklion, []byte("#!/bin/sh\necho standalone-ducklion \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestDuckwayDucklionSubcommandHelper")
	cmd.Env = append(os.Environ(), "DUCKWAY_DUCKLION_HELPER=1", "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ducklion helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "standalone-ducklion version") {
		t.Fatalf("output = %q, want standalone ducklion wrapper", out)
	}
}

func TestDuckwayDucklionSubcommandDetectsOldRecursiveShim(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestDuckwayDucklionSubcommandHelper")
	cmd.Env = append(os.Environ(), "DUCKWAY_DUCKLION_HELPER=1", "DUCKWAY_DUCKLION_WRAPPER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("recursive wrapper succeeded unexpectedly: %s", out)
	}
	if !strings.Contains(string(out), "standalone ducklion") {
		t.Fatalf("output = %q, want standalone ducklion hint", out)
	}
}

func TestDuckwayDucklionSubcommandHelper(t *testing.T) {
	if os.Getenv("DUCKWAY_DUCKLION_HELPER") != "1" {
		return
	}
	os.Args = []string{"duckway", "ducklion", "version"}
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
