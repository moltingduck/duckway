package ducklion

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("DUCKLION_SUPERVISE") == "1" && len(os.Args) > 1 && os.Args[1] == "__supervise" {
		opts, err := ParseSupervisorArgs(os.Args[2:])
		if err != nil {
			panic(err)
		}
		if err := RunSupervisor(opts); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestValidateNameRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"", "../x", "a.b", "a b", "a:b"} {
		if _, err := ValidateName(name); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
}

func TestValidateAgentTypeRejectsUnsafeValues(t *testing.T) {
	for _, agentType := range []string{"codex", "shell", "claude_code", "openclaw-1"} {
		if _, err := ValidateAgentType(agentType); err != nil {
			t.Fatalf("agent type %q rejected: %v", agentType, err)
		}
	}
	for _, agentType := range []string{"bad value", "bad/type", "\x1b[2J"} {
		if _, err := ValidateAgentType(agentType); err == nil {
			t.Fatalf("agent type %q accepted", agentType)
		}
	}
}

func TestScrubSessionEnvDropsSSHAuthSock(t *testing.T) {
	got := scrubSessionEnv([]string{"PATH=/bin", "SSH_AUTH_SOCK=/tmp/agent.sock", "HOME=/home/duck"})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "SSH_AUTH_SOCK=") {
		t.Fatalf("SSH_AUTH_SOCK survived: %#v", got)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/home/duck") {
		t.Fatalf("env was over-scrubbed: %#v", got)
	}
}

func TestDuckwayAgentProxyEnvReadsClientConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DUCKWAY_CONFIG_DIR", dir)
	t.Setenv("NO_PROXY", "example.internal")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("proxy_port: 19090\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if env := duckwayAgentProxyEnv("claude_code"); len(env) != 0 {
		t.Fatalf("proxy env without live proxy = %#v, want empty", env)
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("test ca\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(duckwayAgentProxyEnv("claude_code"), "\n")
	for _, want := range []string{
		"HTTPS_PROXY=http://127.0.0.1:19090",
		"HTTP_PROXY=http://127.0.0.1:19090",
		"NO_PROXY=example.internal,localhost,127.0.0.1",
		"SSL_CERT_FILE=" + filepath.Join(dir, "ca.pem"),
		"NODE_EXTRA_CA_CERTS=" + filepath.Join(dir, "ca.pem"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("proxy env missing %q in:\n%s", want, got)
		}
	}
	if env := duckwayAgentProxyEnv("shell"); len(env) != 0 {
		t.Fatalf("shell proxy env = %#v, want empty", env)
	}
}

func TestMergeEnvLetsExplicitEnvOverrideProxyDefaults(t *testing.T) {
	got := mergeEnv(
		[]string{"PATH=/bin", "HTTPS_PROXY=http://old"},
		[]string{"HTTPS_PROXY=http://default", "NO_PROXY=localhost"},
		[]string{"HTTPS_PROXY=http://explicit"},
	)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "HTTPS_PROXY=http://old") || strings.Contains(joined, "HTTPS_PROXY=http://default") {
		t.Fatalf("old proxy env survived:\n%s", joined)
	}
	if !strings.Contains(joined, "HTTPS_PROXY=http://explicit") || !strings.Contains(joined, "NO_PROXY=localhost") {
		t.Fatalf("merged env missing expected values:\n%s", joined)
	}
}

func TestManagerStartInjectsDuckwayProxyEnvForAgent(t *testing.T) {
	root := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("DUCKWAY_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("proxy_port: 19191\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "proxy.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root, "")
	rec, err := m.Start(StartOptions{
		Name:      "proxyenv",
		AgentType: "claude_code",
		Cwd:       root,
		Command:   []string{"sh", "-lc", "printf \"proxy:%s\" \"$HTTPS_PROXY\"; sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(rec.Name) })
	waitForText(t, m, "proxyenv", "proxy:http://127.0.0.1:19191")
}

func TestManagerStartReadSendStopWithPTY(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "")
	rec, err := m.Start(StartOptions{
		Name:      "alpha",
		AgentType: "shell",
		Cwd:       root,
		Command:   []string{"sh", "-lc", "printf ready; while IFS= read -r line; do echo got:$line; done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Socket == "" || rec.Log == "" || rec.PID == 0 {
		t.Fatalf("record = %+v", rec)
	}
	waitForText(t, m, "alpha", "ready")
	if err := m.Send("alpha", "hello"); err != nil {
		t.Fatal(err)
	}
	waitForText(t, m, "alpha", "got:hello")
	if err := m.Stop("alpha"); err != nil {
		t.Fatal(err)
	}
	records, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != StatusStopped {
		t.Fatalf("records = %+v", records)
	}
}

func TestManagerRejectsDuplicateLiveSession(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "")
	_, err := m.Start(StartOptions{Name: "alpha", Cwd: root, Command: []string{"sh", "-lc", "sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop("alpha") })
	if _, err := m.Start(StartOptions{Name: "alpha", Cwd: root, Command: []string{"sh", "-lc", "sleep 30"}}); err == nil {
		t.Fatal("duplicate session accepted")
	}
}

func TestListMarksExitedSupervisorStale(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "")
	if _, err := m.Start(StartOptions{Name: "alpha", Cwd: root, Command: []string{"sh", "-lc", "sleep 0.2; printf done"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		records, err := m.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 && records[0].Status == StatusStale {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	records, _ := m.List()
	t.Fatalf("session did not become stale after command exit: %+v", records)
}

func TestParseSupervisorArgs(t *testing.T) {
	opts, err := ParseSupervisorArgs([]string{"--name", "alpha", "--agent", "shell", "--cwd", "/tmp", "--socket", "/tmp/a.sock", "--log", "/tmp/a.log", "--", "sh", "-lc", "echo ok"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "alpha" || strings.Join(opts.Command, " ") != "sh -lc echo ok" {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestRespondToTerminalQueries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "da1", in: "\x1b[c", want: "\x1b[?6c"},
		{name: "da2", in: "\x1b[>c", want: "\x1b[>0;0;0c"},
		{name: "dsr-status", in: "\x1b[5n", want: "\x1b[0n"},
		{name: "cursor-position", in: "\x1b[6n", want: "\x1b[1;1R"},
		{name: "window-size", in: "\x1b[18t", want: "\x1b[8;40;120t"},
		{name: "xtversion", in: "\x1b[>q", want: "\x1bP>|duckway 0\x1b\\"},
		{name: "unknown", in: "\x1b[?25h", want: ""},
		{name: "partial", in: "\x1b[>", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(respondToTerminalQueries([]byte(tc.in))); got != tc.want {
				t.Fatalf("response = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadMissingLogReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "")
	state := &State{Version: stateVersion, Sessions: []Record{{
		Name: "alpha", Status: StatusRunning, PID: os.Getpid(), Log: filepath.Join(root, "missing.log"), Socket: filepath.Join(root, "missing.sock"),
	}}}
	unlock, err := m.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.saveLocked(state); err != nil {
		t.Fatal(err)
	}
	unlock()
	text, err := m.Read("alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Fatalf("text = %q", text)
	}
}

func waitForText(t *testing.T, m *Manager, name, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		text, err := m.Read(name, 20)
		if err == nil && strings.Contains(text, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	text, _ := m.Read(name, 20)
	t.Fatalf("timed out waiting for %q in %q", want, text)
}
