package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestDucklordClientsReadsConfig(t *testing.T) {
	config := writeConfig(t)
	var out bytes.Buffer
	if err := run([]string{"clients", "--config", config}, &out, fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "client-a") || !strings.Contains(out.String(), "duck@client-a") {
		t.Fatalf("clients output = %q", out.String())
	}
}

func TestDucklordSSHHostsReadsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(path, []byte("Host vulns *.internal\nHost lab\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"ssh-hosts", "--config-file", path}, &out, fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "vulns") || !strings.Contains(got, "lab") || strings.Contains(got, "*.internal") {
		t.Fatalf("ssh-hosts output = %q", got)
	}
}

func TestDucklordImportSSHHostsCreatesConfigWithoutDuplicates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	config := filepath.Join(dir, "ducklord.yaml")
	sshConfig := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(sshConfig, []byte("Host client-a *.skip\nHost client-b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"import-ssh-hosts", "--config", config, "--ssh-config", sshConfig}, &out, fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Imported 2 SSH host") {
		t.Fatalf("import output = %q", out.String())
	}
	if err := run([]string{"import-ssh-hosts", "--config", config, "--ssh-config", sshConfig}, io.Discard, fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := ducklord.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 2 {
		t.Fatalf("clients = %+v", cfg.Clients)
	}
	for _, c := range cfg.Clients {
		if c.Group != "ssh" || c.Ducklion != "ducklion" || c.SSH != "ssh" {
			t.Fatalf("imported client = %+v", c)
		}
	}
}

func TestLoadOrEmptyConfigAllowsMissingConfig(t *testing.T) {
	cfg, err := loadOrEmptyConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || len(cfg.Clients) != 0 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestDucklordSessionsUsesRunner(t *testing.T) {
	config := writeConfig(t)
	var out bytes.Buffer
	runner := fakeRunner{sessions: []ducklord.RemoteSession{{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell", LastLine: "done"}}}
	if err := run([]string{"sessions", "client-a", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "alpha") || !strings.Contains(out.String(), "done") {
		t.Fatalf("sessions output = %q", out.String())
	}
}

func TestDucklordRestartDefaultsToWaitAndSupportsForce(t *testing.T) {
	config := writeConfig(t)
	runner := &recordingRunner{}
	var out bytes.Buffer
	if err := run([]string{"restart", "client-a", "ABC123", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	if runner.lifecycleClient != "client-a" || runner.lifecycleSession != "ABC123" || runner.lifecycleOp != protocol.SessionLifecycleRestart || runner.lifecycleMode != protocol.SessionLifecycleWait {
		t.Fatalf("restart call=%+v", runner)
	}
	if !strings.Contains(out.String(), "Restarted ABC123") {
		t.Fatalf("restart output=%q", out.String())
	}
	if err := run([]string{"restart", "client-a", "ABC123", "--force", "--config", config}, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	if runner.lifecycleMode != protocol.SessionLifecycleForce {
		t.Fatalf("forced restart mode=%q", runner.lifecycleMode)
	}
	if err := run([]string{"restart", "client-a", "ABC123", "--wait", "--config", config}, io.Discard, runner); err == nil {
		t.Fatal("restart accepted redundant wait flag")
	}
	if err := run([]string{"end", "client-a", "ABC123", "--force", "-f", "--config", config}, io.Discard, runner); err == nil {
		t.Fatal("end accepted duplicate lifecycle mode flags")
	}
}

func TestDucklordSessionsSanitizesRemoteLastLine(t *testing.T) {
	config := writeConfig(t)
	var out bytes.Buffer
	runner := fakeRunner{sessions: []ducklord.RemoteSession{{
		Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell",
		LastLine: "ok\x1b]52;c;pw\a\x1b[2J",
	}}}
	if err := run([]string{"sessions", "client-a", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, b := range []byte{0x1b, 0x07} {
		if strings.ContainsRune(got, rune(b)) {
			t.Fatalf("control byte 0x%x survived: %q", b, got)
		}
	}
	if !strings.Contains(got, "ok ]52;c;pw") || !strings.Contains(got, "[2J") {
		t.Fatalf("sanitized sessions output = %q", got)
	}
}

func TestDucklordSessionsDistinguishesNativeShellFromAgent(t *testing.T) {
	config := writeConfig(t)
	runner := fakeRunner{sessions: []ducklord.RemoteSession{
		{Client: "client-a", Name: "terminal", Kind: string(model.KindShell), Status: "running"},
		{Client: "client-a", Name: "worker", Kind: string(model.KindAgent), AgentType: "codex", Status: "running"},
	}}
	var out bytes.Buffer
	if err := run([]string{"sessions", "client-a", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "TYPE") || !strings.Contains(text, "terminal") || !strings.Contains(text, "shell") || !strings.Contains(text, "agent:codex") {
		t.Fatalf("sessions output=%q", text)
	}
}

func TestDucklordProjectsUsesRunner(t *testing.T) {
	config := writeConfig(t)
	var out bytes.Buffer
	runner := fakeRunner{projects: []ducklord.RemoteProject{{Name: "duckway", Path: "/home/duck/duckway", Source: "duckway-client"}}}
	if err := run([]string{"projects", "client-a", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "duckway") || !strings.Contains(out.String(), "/home/duck/duckway") {
		t.Fatalf("projects output = %q", out.String())
	}
}

func TestDucklordAgentsUsesRemoteProjectDiscovery(t *testing.T) {
	config := writeConfig(t)
	var out bytes.Buffer
	runner := fakeRunner{agents: []ducklord.RemoteAgent{{Type: "codex", Command: []string{"/usr/local/bin/codex"}}}}
	if err := run([]string{"agents", "client-a", "/work/專案", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "codex") || !strings.Contains(out.String(), "/usr/local/bin/codex") {
		t.Fatalf("agents output=%q", out.String())
	}
}

func TestDucklordProbeUsesRunner(t *testing.T) {
	config := writeConfig(t)
	var out bytes.Buffer
	runner := fakeRunner{probe: ducklord.DucklionProbe{Available: true, Command: "duckway ducklion", Version: "ducklion v1"}}
	if err := run([]string{"probe", "client-a", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "duckway ducklion") {
		t.Fatalf("probe output = %q", out.String())
	}
}

func TestDucklordInstallDucklionUpdatesConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	config := writeConfig(t)
	runner := &recordingRunner{installPath: "/home/duck/.local/bin/ducklion"}
	var out bytes.Buffer
	err := run([]string{
		"install-ducklion", "client-a",
		"--source", "/tmp/ducklion",
		"--dest", "~/.local/bin/ducklion",
		"--config", config,
	}, &out, runner)
	if err != nil {
		t.Fatal(err)
	}
	if runner.installClient != "client-a" || runner.installSource != "/tmp/ducklion" || runner.installDest != "~/.local/bin/ducklion" {
		t.Fatalf("install runner client=%q source=%q dest=%q", runner.installClient, runner.installSource, runner.installDest)
	}
	cfg, err := ducklord.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := cfg.Client("client-a")
	if !ok {
		t.Fatal("client-a missing")
	}
	if client.Ducklion != "/home/duck/.local/bin/ducklion" {
		t.Fatalf("ducklion path = %q", client.Ducklion)
	}
	if !strings.Contains(out.String(), "Installed ducklion on client-a") {
		t.Fatalf("install output = %q", out.String())
	}
}

func TestAttachHostConfigNarrowsToOneClient(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{
		{Name: "client-a", Host: "client-a", Group: "lab"},
		{Name: "client-b", Host: "client-b", Group: "lab"},
	}}
	got, err := attachHostConfig(cfg, "client-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Clients) != 1 || got.Clients[0].Name != "client-b" {
		t.Fatalf("host config clients = %+v", got.Clients)
	}
	if len(cfg.Clients) != 2 {
		t.Fatalf("source config was mutated: %+v", cfg.Clients)
	}
	if _, err := attachHostConfig(cfg, "missing"); err == nil {
		t.Fatal("missing client accepted")
	}
}

func TestDucklordReadParsesLines(t *testing.T) {
	config := writeConfig(t)
	var out bytes.Buffer
	runner := fakeRunner{readText: "pane\n"}
	if err := run([]string{"read", "client-a", "alpha", "--lines", "42", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	if out.String() != "pane\n" {
		t.Fatalf("read output = %q", out.String())
	}
}

func TestDucklordStartRejectsInvalidArgsBeforeRunner(t *testing.T) {
	config := writeConfig(t)
	runner := &recordingRunner{}
	for _, args := range [][]string{
		{"start", "client-a", "--name", "bad\nname", "--", "bash", "--config", config},
		{"start", "client-a", "--bad", "--", "bash", "--config", config},
		{"start", "client-a", "--name", "alpha", "--config", config},
	} {
		if err := run(args, io.Discard, runner); err == nil {
			t.Fatalf("args %#v accepted", args)
		}
	}
	if runner.startClient != "" {
		t.Fatalf("runner start called for invalid args: %s %#v", runner.startClient, runner.startArgs)
	}
}

func TestDucklordStartValidatesAndUsesRunner(t *testing.T) {
	config := writeConfig(t)
	runner := &recordingRunner{}
	err := run([]string{"start", "client-a", "--name", "alpha", "--agent", "shell", "--cwd", "/tmp", "--", "bash", "--config", config}, io.Discard, runner)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--name", "alpha", "--agent", "shell", "--cwd", "/tmp", "--", "bash"}
	if runner.startClient != "client-a" || strings.Join(runner.startArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("start client=%q args=%#v", runner.startClient, runner.startArgs)
	}
}

func TestDucklordStartCreatesNativeShellSession(t *testing.T) {
	config := writeConfig(t)
	runner := &recordingRunner{}
	if err := run([]string{"start", "client-a", "--name", "terminal", "--kind", "shell", "--cwd", "/tmp", "--", "bash", "--config", config}, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	want := []string{"--name", "terminal", "--kind", "shell", "--cwd", "/tmp", "--", "bash"}
	if strings.Join(runner.startArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("start args=%#v", runner.startArgs)
	}
	for _, args := range [][]string{
		{"start", "client-a", "--name", "bad-shell", "--kind", "shell", "--agent", "codex", "--cwd", "/tmp", "--", "bash", "--config", config},
		{"start", "client-a", "--name", "bad-shell", "--kind", "shell", "--cwd", "/tmp", "--", "bash", "-l", "--config", config},
		{"start", "client-a", "--name", "bad-kind", "--kind", "other", "--cwd", "/tmp", "--", "bash", "--config", config},
	} {
		if err := run(args, io.Discard, runner); err == nil {
			t.Fatalf("args %#v accepted", args)
		}
	}
}

func TestDucklordYieldUsesRunnerAndWaitFlag(t *testing.T) {
	config := writeConfig(t)
	runner := &recordingRunner{yieldResult: protocol.SessionYieldResult{Decision: "waiting", SessionID: "ABC123", OwnershipEpoch: 7}}
	var out bytes.Buffer
	if err := run([]string{"yield", "client-a", "alpha", "--wait", "--config", config}, &out, runner); err != nil {
		t.Fatal(err)
	}
	if runner.yieldClient != "client-a" || runner.yieldSession != "alpha" || !runner.yieldWait {
		t.Fatalf("yield client=%q session=%q wait=%v", runner.yieldClient, runner.yieldSession, runner.yieldWait)
	}
	if !strings.Contains(out.String(), "Yield ABC123: waiting (epoch 7)") {
		t.Fatalf("yield output=%q", out.String())
	}
}

func TestDucklordRejectsUnknownTUIFlag(t *testing.T) {
	if _, _, _, err := parseTUIFlags([]string{"--bad"}); err == nil {
		t.Fatal("unknown tui flag accepted")
	}
}

func TestWrapDisplayTextPreservesActionableError(t *testing.T) {
	got := wrapDisplayText("error: daemon socket is unavailable", 12)
	if strings.Join(got, "") != "error: daemon socket is unavailable" || len(got) < 2 {
		t.Fatalf("wrapped lines = %#v", got)
	}
}

func TestAppendOutputTextBoundsNewlineFreeOutput(t *testing.T) {
	text := appendOutputText("", strings.Repeat("x", 2<<20), 120)
	if len(text) > 1<<20 {
		t.Fatalf("output retained %d bytes", len(text))
	}
	text = appendOutputText(text, strings.Repeat("y", 64<<10), 120)
	if len(text) > 1<<20 || !strings.HasSuffix(text, "y") {
		t.Fatalf("bounded output length=%d suffix=%q", len(text), text[len(text)-1:])
	}
}

func TestParseCreateLineDefaultsToBash(t *testing.T) {
	name, args, err := parseCreateLine("bash")
	if err != nil {
		t.Fatal(err)
	}
	if name != "bash" || strings.Join(args, " ") != "--name bash -- bash" {
		t.Fatalf("name=%q args=%q", name, strings.Join(args, " "))
	}
}

func TestParseCreateLineAllowsOptionsAndQuotedCommand(t *testing.T) {
	name, args, err := parseCreateLine(`build --agent shell --cwd /repo -- sh -lc "make test"`)
	if err != nil {
		t.Fatal(err)
	}
	if name != "build" {
		t.Fatalf("name = %q", name)
	}
	want := []string{"--name", "build", "--agent", "shell", "--cwd", "/repo", "--", "sh", "-lc", "make test"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args=%#v want=%#v", args, want)
	}
}

func TestParseCreateLineRejectsUnknownOptionBeforeStart(t *testing.T) {
	for _, line := range []string{"bad\x00name", "alpha --bad", "alpha --agent bad/value", `alpha -- sh -lc "unterminated`} {
		if _, _, err := parseCreateLine(line); err == nil {
			t.Fatalf("line %q accepted", line)
		}
	}
}

func TestSplitCommandLineQuotesAndEscapes(t *testing.T) {
	got, err := splitCommandLine(`alpha --cwd "/path with spaces" -- sh -lc 'echo ok' escaped\ arg`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "--cwd", "/path with spaces", "--", "sh", "-lc", "echo ok", "escaped arg"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("fields=%#v want=%#v", got, want)
	}
	for _, line := range []string{`alpha "unterminated`, `alpha \`} {
		if _, err := splitCommandLine(line); err == nil {
			t.Fatalf("line %q accepted", line)
		}
	}
}

func TestTUIRenderShowsMenuAndContentPane(t *testing.T) {
	state := &tuiState{
		sessions:   []ducklord.RemoteSession{{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell", Group: "lab", LastLine: "latest"}},
		selected:   0,
		outputText: sanitizeTerminalText("line 1\n\x1b[2Jline 2\n"),
	}
	var out bytes.Buffer
	state.render(&out)
	got := out.String()
	for _, want := range []string{"sessions", "content", "client-a", "alpha", "line 1", " [2Jline 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b[2Jline") {
		t.Fatalf("remote escape sequence was rendered: %q", got)
	}
}

func TestHostScopedTUIDisablesAddAndNewShortcuts(t *testing.T) {
	state := &tuiState{hostScoped: true}
	if got := state.handleInput([]byte("a")); got != "" {
		t.Fatalf("host-scoped add shortcut action = %q", got)
	}
	if got := state.handleInput([]byte("c")); got != "" {
		t.Fatalf("host-scoped new shortcut action = %q", got)
	}
	if got := state.handleInput([]byte("n")); got != "notifications" {
		t.Fatalf("host-scoped notification shortcut action = %q", got)
	}
	if got := state.handleInput([]byte("d")); got != "" {
		t.Fatalf("host-scoped remove shortcut action = %q", got)
	}
	if got := state.handleInput([]byte("r")); got != "refresh" {
		t.Fatalf("host-scoped refresh action = %q", got)
	}
	for key, want := range map[string]string{"E": "end", "R": "restart", "X": "destroy"} {
		if got := state.handleInput([]byte(key)); got != want {
			t.Fatalf("lifecycle shortcut %q action = %q", key, got)
		}
	}
}

func TestTUIRenderExplainsDestructiveLifecycleConfirmation(t *testing.T) {
	state := &tuiState{ownerName: "desk", lifecycleConfirm: protocol.SessionLifecycleDestroy,
		sessions: []ducklord.RemoteSession{{Client: "host", Name: "agent", SessionID: "ABC123", Kind: string(model.KindAgent), Status: "running"}}}
	var out bytes.Buffer
	state.render(&out)
	got := out.String()
	for _, want := range []string{"destroy selected session?", "enter now", "w wait", "f force-cancel", "permanently removes", "ABC123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("confirmation missing %q in %q", want, got)
		}
	}
}

func TestTUIRenderExplainsImmediateShellLifecycle(t *testing.T) {
	state := &tuiState{ownerName: "desk", lifecycleConfirm: protocol.SessionLifecycleRestart,
		sessions: []ducklord.RemoteSession{{Client: "host", Name: "shell", SessionID: "ABC123", Kind: string(model.KindShell), Status: "running"}}}
	var out bytes.Buffer
	state.render(&out)
	got := out.String()
	for _, want := range []string{"enter terminate process immediately", "it never waits"} {
		if !strings.Contains(got, want) {
			t.Fatalf("shell confirmation missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "force-cancel") || strings.Contains(got, "w wait") {
		t.Fatalf("shell confirmation exposed agent lifecycle modes: %q", got)
	}
}

func TestTUIRenderShowsStoppedRetainedOutputWindow(t *testing.T) {
	until := time.Date(2030, time.January, 2, 15, 4, 0, 0, time.Local).UnixMilli()
	state := &tuiState{ownerName: "desk", sessions: []ducklord.RemoteSession{{Client: "host", Name: "agent", SessionID: "ABC123",
		Kind: string(model.KindAgent), Status: string(model.StatusStopped), RetainedOutputBytes: 1536, RetainedOutputUntilMS: until}}}
	var out bytes.Buffer
	state.render(&out)
	for _, want := range []string{"stopped", "retained:1.5 KiB", "until Jan 02 15:04"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("retention header missing %q in %q", want, out.String())
		}
	}
}

func TestTUILifecycleWaitDoesNotBlockNavigationOrQuit(t *testing.T) {
	state := &tuiState{lifecycleBusy: true, sessions: []ducklord.RemoteSession{{Name: "one"}, {Name: "two"}}}
	if got := state.handleInput([]byte("j")); got != "select" || state.selected != 1 {
		t.Fatalf("busy lifecycle navigation action=%q selected=%d", got, state.selected)
	}
	if got := state.handleInput([]byte("q")); got != "quit" {
		t.Fatalf("busy lifecycle quit action=%q", got)
	}
}

func TestTUISelectionSurvivesRefreshByKey(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}, {Name: "client-b", Host: "client-b"}}}
	state := &tuiState{
		cfg: cfg,
		runner: fakeRunner{sessionsByClient: map[string][]ducklord.RemoteSession{
			"client-a": {{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell"}},
			"client-b": {{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell"}},
		}},
		hashes: map[string]string{},
	}
	state.refreshSessions(context.Background())
	state.selected = 1
	if state.currentKey() != "/client-b/beta" {
		t.Fatalf("initial key = %q", state.currentKey())
	}
	state.runner = fakeRunner{sessionsByClient: map[string][]ducklord.RemoteSession{
		"client-a": {{Client: "client-a", Name: "aardvark", Status: "running", AgentType: "shell"}, {Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell"}},
		"client-b": {{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell"}},
	}}
	state.refreshSessions(context.Background())
	if state.currentKey() != "/client-b/beta" {
		t.Fatalf("selected key moved after refresh: %q sessions=%+v", state.currentKey(), state.sessions)
	}
}

func TestTUIOfflineRowCannotAttach(t *testing.T) {
	if canAttach(ducklord.RemoteSession{Name: "(offline)", Status: "error", Error: "ssh failed"}) {
		t.Fatal("offline row can attach")
	}
}

func TestTUIRefreshSelectedOutputUsesSelectedClient(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}, {Name: "client-b", Host: "client-b"}}}
	runner := &recordingRunner{readText: "pane\n"}
	state := &tuiState{
		cfg:    cfg,
		runner: runner,
		sessions: []ducklord.RemoteSession{
			{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell"},
			{Client: "client-b", Name: "alpha", Status: "running", AgentType: "shell"},
		},
		selected: 1,
	}
	state.refreshSelectedOutput(context.Background())
	if runner.readClient != "client-b" || runner.readSession != "alpha" {
		t.Fatalf("read target = %s/%s", runner.readClient, runner.readSession)
	}
}

func TestTUIShowsSavedSnapshotWhileRemoteReadFails(t *testing.T) {
	instance := string(model.NewInstanceID())
	store := ducklord.SnapshotStore{Root: filepath.Join(t.TempDir(), "sessions")}
	payload, err := ducklord.EncodeTerminalRenderState(ducklord.TerminalRenderState{Text: "previous agent result\n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ducklord.TerminalSnapshot{InstanceID: instance, SessionID: "ABC123", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{
		cfg:           &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}}},
		runner:        fakeRunner{readErr: errors.New("remote offline")},
		snapshotStore: store,
		sessions: []ducklord.RemoteSession{{Client: "client-a", InstanceID: instance, SessionID: "ABC123", Name: "agent", Status: "running",
			AgentType: "codex"}},
	}
	state.refreshSelectedOutput(context.Background())
	if !state.outputStale || state.outputText != "previous agent result\n" || !strings.Contains(state.outputErr, "remote offline") {
		t.Fatalf("stale=%v text=%q err=%q", state.outputStale, state.outputText, state.outputErr)
	}
	var rendered bytes.Buffer
	state.renderContent(&rendered, 1, 80, 20)
	if !strings.Contains(rendered.String(), "STALE SNAPSHOT") || !strings.Contains(rendered.String(), "previous agent result") {
		t.Fatalf("rendered stale snapshot=%q", rendered.String())
	}
}

func TestTUIRenderContentPreservesTerminalColorAndResetsBeforeErase(t *testing.T) {
	terminal := ducklord.NewTerminal(4, 80, 10)
	terminal.Write([]byte("\x1b[38;2;12;34;56mcolored\x1b[0m"))
	state := &tuiState{
		sessions: []ducklord.RemoteSession{{Client: "host", Name: "agent", Status: "running"}},
		selected: 0,
		terminal: terminal,
	}
	var rendered bytes.Buffer
	state.renderContent(&rendered, 1, 80, 20)
	got := rendered.String()
	if !strings.Contains(got, "38;2;12;34;56mcolored\x1b[0m") {
		t.Fatalf("rendered terminal lost truecolor style: %q", got)
	}
	if strings.Contains(got, "colored\x1b[K") {
		t.Fatalf("styled terminal row was not reset before erase-to-EOL: %q", got)
	}
}

func TestTUIStoppedSessionReplacesSameGenerationSnapshotWithRetainedOutput(t *testing.T) {
	instance := string(model.NewInstanceID())
	store := ducklord.SnapshotStore{Root: filepath.Join(t.TempDir(), "sessions")}
	terminal := ducklord.NewTerminal(2, 40, 10)
	terminal.Write([]byte("older snapshot\n"))
	framebuffer := terminal.SnapshotState()
	payload, err := ducklord.EncodeTerminalRenderState(ducklord.TerminalRenderState{Framebuffer: &framebuffer, RuntimeGeneration: 3, OutputOffset: 14, ResumeCursorValid: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ducklord.TerminalSnapshot{InstanceID: instance, SessionID: "ABC123", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{readText: "older snapshot\nfinal retained bytes\n"}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: runner, snapshotStore: store,
		sessions: []ducklord.RemoteSession{{Client: "host", InstanceID: instance, SessionID: "ABC123", Name: "agent", Status: string(model.StatusStopped),
			RuntimeGeneration: 3, RetainedOutputBytes: 35, RetainedOutputUntilMS: time.Now().Add(7 * 24 * time.Hour).UnixMilli()}}}
	state.refreshSelectedOutput(context.Background())
	if runner.readSession != "ABC123" || !strings.Contains(state.outputText, "final retained bytes") || state.outputStale || strings.Contains(state.outputErr, "press enter") {
		t.Fatalf("read=%q text=%q stale=%v err=%q", runner.readSession, state.outputText, state.outputStale, state.outputErr)
	}
}

func TestTUIKeepsSnapshotForOfflineSessionAndReplacesItOnFreshOutput(t *testing.T) {
	instance := string(model.NewInstanceID())
	store := ducklord.SnapshotStore{Root: filepath.Join(t.TempDir(), "sessions")}
	payload, err := ducklord.EncodeTerminalRenderState(ducklord.TerminalRenderState{Text: "stale only\n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ducklord.TerminalSnapshot{InstanceID: instance, SessionID: "ABC123", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{cfg: &ducklord.Config{}, runner: fakeRunner{}, snapshotStore: store,
		sessions: []ducklord.RemoteSession{{Client: "client-a", InstanceID: instance, SessionID: "ABC123", Name: "agent", Status: "error", Error: "host offline"}}}
	state.refreshSelectedOutput(context.Background())
	if !state.outputStale || state.outputText != "stale only\n" || state.outputErr != "host offline" {
		t.Fatalf("offline stale=%v text=%q err=%q", state.outputStale, state.outputText, state.outputErr)
	}
	state.applyAttachOutput("fresh only\n", 1, 11)
	if state.outputStale || state.terminal == nil || state.terminal.Text() != "fresh only\n" || strings.Contains(state.terminal.Text(), "stale only") {
		t.Fatalf("fresh stale=%v text=%q", state.outputStale, state.terminal.Text())
	}
}

func TestTUIRevisionSnapshotPreservesSelectionAndRowsWhileReconnecting(t *testing.T) {
	state := &tuiState{hostSync: make(map[string]ducklord.SessionUpdate), selected: 1, sessions: []ducklord.RemoteSession{
		{Client: "host-a", InstanceID: "instance", SessionID: "ABC123", Name: "old-name"},
		{Client: "host-b", InstanceID: "other", SessionID: "DEF456", Name: "other"},
	}}
	state.selected = 0
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: "instance", Revision: 10, State: "live",
		Sessions: []ducklord.RemoteSession{{Client: "host-a", InstanceID: "instance", SessionID: "ABC123", Name: "renamed"}}})
	if len(state.sessions) != 2 || state.currentSession().Name != "renamed" {
		t.Fatalf("live sessions=%+v selected=%+v", state.sessions, state.currentSession())
	}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", Revision: 11, State: "reconnecting", Err: errors.New("ssh down")})
	if len(state.sessions) != 2 || state.currentSession().Name != "renamed" {
		t.Fatalf("reconnecting discarded rows: %+v", state.sessions)
	}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", Revision: 9, State: "live", Sessions: nil})
	if len(state.sessions) != 2 {
		t.Fatalf("old revision replaced rows: %+v", state.sessions)
	}
}

func TestTUIRevisionMarksOnlyChangedBackgroundSession(t *testing.T) {
	state := &tuiState{hostSync: make(map[string]ducklord.SessionUpdate), sessions: []ducklord.RemoteSession{
		{Client: "host-a", InstanceID: "instance", SessionID: "ABC123", Name: "active"},
		{Client: "host-a", InstanceID: "instance", SessionID: "DEF456", Name: "background"},
	}}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: "instance", Revision: 2, State: "live", ChangedSessionID: "DEF456",
		Sessions: []ducklord.RemoteSession{{Client: "host-a", InstanceID: "instance", SessionID: "ABC123", Name: "active"},
			{Client: "host-a", InstanceID: "instance", SessionID: "DEF456", Name: "background"}}})
	if state.sessions[0].Updated || !state.sessions[1].Updated {
		t.Fatalf("updated markers=%+v", state.sessions)
	}
}

func TestTUIActivityUnreadRequiresFreshActiveOutputToClear(t *testing.T) {
	instance := string(model.NewInstanceID())
	stateDir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateDir, "state.json")
	state := &tuiState{hostSync: make(map[string]ducklord.SessionUpdate), activityState: ducklord.NewActivityState(),
		activityStore: ducklord.ActivityStateStore{Path: statePath}, outputForKey: instance + "/ABC123", outputFresh: true,
		sessions: []ducklord.RemoteSession{
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "ABC123", Name: "active"},
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "DEF456", Name: "background"},
		}}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: instance, Revision: 2, Generation: 1, State: "live", ChangedSessionID: "DEF456",
		Sessions: []ducklord.RemoteSession{
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "ABC123", Name: "active"},
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "DEF456", Name: "background",
				ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTerminalAttention: 1}},
		}})
	if state.sessions[0].Unread || !state.sessions[1].Unread || !state.groupHasUnread("work") {
		t.Fatalf("unread projection=%+v", state.sessions)
	}
	state.selected = 1
	state.outputForKey = instance + "/DEF456"
	state.outputFresh = false // a stale detach snapshot is not proof of seeing it
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: instance, Revision: 3, Generation: 1, State: "live",
		Sessions: append([]ducklord.RemoteSession(nil), state.sessions...)})
	if !state.currentSession().Unread {
		t.Fatal("selection or stale output cleared unread")
	}
	state.focused = true
	state.pendingAttachKey = instance + "/DEF456"
	state.applyAttachOutput("fresh\n", 1, 6)
	if state.currentSession().Unread || state.groupHasUnread("work") {
		t.Fatalf("fresh attach did not clear unread: %+v", state.sessions)
	}
	loaded, err := (ducklord.ActivityStateStore{Path: statePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sessions[instance+"/DEF456"].Seen[model.NotificationTerminalAttention] != 1 {
		t.Fatalf("seen state=%+v", loaded.Sessions)
	}
}

func TestTUISelectionDoesNotMoveActiveSessionOrClearUnread(t *testing.T) {
	instance := string(model.NewInstanceID())
	state := &tuiState{activityState: ducklord.NewActivityState(), activeAttachKey: instance + "/ABC123", activeAttachFresh: true,
		outputForKey: instance + "/ABC123", outputFresh: true, sessions: []ducklord.RemoteSession{
			{Client: "host-a", InstanceID: instance, SessionID: "ABC123", Name: "active"},
			{Client: "host-a", InstanceID: instance, SessionID: "DEF456", Name: "background", Unread: true,
				ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTerminalAttention: 1}},
		}}
	if action := state.handleInput([]byte("j")); action != "select" {
		t.Fatalf("action=%q", action)
	}
	if state.activeAttachKey != instance+"/ABC123" || !state.currentSession().Unread {
		t.Fatalf("selection moved active or cleared unread: active=%q current=%+v", state.activeAttachKey, state.currentSession())
	}
}

func TestTUIDetachedSelectionImmediatelyLoadsSelectedSessionPreview(t *testing.T) {
	instance := string(model.NewInstanceID())
	runner := &recordingRunner{readText: "session-b-screen\n"}
	state := &tuiState{
		cfg:               &ducklord.Config{Clients: []ducklord.Client{{Name: "host-a", Host: "host-a"}}},
		runner:            runner,
		activityState:     ducklord.NewActivityState(),
		activeAttachKey:   instance + "/ABC123",
		activeAttachFresh: true,
		pendingAttachKey:  instance + "/ABC123",
		outputForKey:      instance + "/ABC123",
		outputText:        "session-a-screen\n",
		sessions: []ducklord.RemoteSession{
			{Client: "host-a", InstanceID: instance, SessionID: "ABC123", Name: "a", Status: "running"},
			{Client: "host-a", InstanceID: instance, SessionID: "DEF456", Name: "b", Status: "running"},
		},
	}
	if action := state.handleInput([]byte("j")); action != "select" {
		t.Fatalf("selection action=%q", action)
	}
	state.followSelectedPreview(context.Background())
	if state.effectiveAttachKey() != "" {
		t.Fatalf("preview switch retained attach identity: %q", state.effectiveAttachKey())
	}
	if runner.readSession != "DEF456" || strings.Contains(state.outputText, "session-a") || !strings.Contains(state.outputText, "session-b") {
		t.Fatalf("read=%q output=%q", runner.readSession, state.outputText)
	}
}

func TestInitialReplayCatchUpAllowsIdleAttachResize(t *testing.T) {
	for _, test := range []struct {
		name         string
		current, end uint64
		want         bool
	}{
		{"idle exact resume", 42, 42, true},
		{"empty fresh session", 0, 0, true},
		{"replay pending", 10, 20, false},
		{"replay consumed", 20, 20, true},
		{"live bytes passed boundary", 24, 20, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := initialReplayCaughtUp(test.current, test.end); got != test.want {
				t.Fatalf("initialReplayCaughtUp(%d, %d)=%v want %v", test.current, test.end, got, test.want)
			}
		})
	}
}

func TestPreviewResultCannotOverwriteNewSelectionOrFocusedAttach(t *testing.T) {
	instance := string(model.NewInstanceID())
	state := &tuiState{sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: instance, SessionID: "AAA111", RuntimeGeneration: 3},
		{Client: "host", InstanceID: instance, SessionID: "BBB222", RuntimeGeneration: 7},
	}, selected: 1, outputText: "current-b"}
	if state.applyPreviewOutput(previewOutputEvent{id: 1, key: instance + "/AAA111", generation: 3, text: "late-a"}, 2) {
		t.Fatal("late preview result was applied")
	}
	if state.outputText != "current-b" {
		t.Fatalf("late preview overwrote selection: %q", state.outputText)
	}
	result := previewOutputEvent{id: 2, key: instance + "/BBB222", generation: 7, text: "fresh-b"}
	if !state.applyPreviewOutput(result, 2) || !strings.Contains(state.outputText, "fresh-b") {
		t.Fatalf("current preview was not applied: %q", state.outputText)
	}
	state.focused = true
	if state.applyPreviewOutput(previewOutputEvent{id: 3, key: instance + "/BBB222", generation: 7, text: "stale-preview"}, 3) {
		t.Fatal("preview overwrote focused attachment")
	}
}

func TestTUIResizeOnlyForCurrentWriterOrSharedShell(t *testing.T) {
	state := &tuiState{ownerName: "desk", sessions: []ducklord.RemoteSession{{Kind: string(model.KindAgent), WriterKind: string(model.OwnerTerminal), WriterID: "desk"}}}
	if !state.canResizeCurrentSession() {
		t.Fatal("current terminal writer could not resize")
	}
	state.sessions[0].WriterID = "other"
	if state.canResizeCurrentSession() {
		t.Fatal("read-only agent attachment could resize")
	}
	state.sessions[0].Kind = string(model.KindShell)
	if !state.canResizeCurrentSession() {
		t.Fatal("shared shell attachment could not resize")
	}
	rows, cols := state.activePTYSize()
	if rows < 5 || rows > 200 || cols < 40 || cols > 500 {
		t.Fatalf("PTY size outside protocol bounds: %dx%d", rows, cols)
	}
	for _, test := range []struct {
		width       int
		focused     bool
		overlay     bool
		showList    bool
		contentCols int
	}{{78, false, true, true, 78}, {78, true, false, false, 78}, {79, false, false, true, 40}, {20, true, false, false, 20}, {1, true, false, false, 1}} {
		layout := calculateTUILayout(test.width, test.focused, 36, true)
		if layout.width != test.width || layout.overlay != test.overlay || layout.showList != test.showList || layout.contentWidth != test.contentCols {
			t.Fatalf("layout(%d,%v)=%+v", test.width, test.focused, layout)
		}
	}
}

func TestTUIPendingAttachIsFencedWhenSessionRemoved(t *testing.T) {
	instance := string(model.NewInstanceID())
	state := &tuiState{focused: true, pendingAttachKey: instance + "/ABC123", hostSync: make(map[string]ducklord.SessionUpdate), activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{
		{Client: "host-a", InstanceID: instance, SessionID: "ABC123", Name: "quiet"},
	}}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: instance, Generation: 1, Revision: 2, State: "live"})
	if state.focused || state.effectiveAttachKey() != "" || state.outputErr != "active session was removed remotely" {
		t.Fatalf("pending attach survived removal: focused=%v key=%q err=%q", state.focused, state.effectiveAttachKey(), state.outputErr)
	}
}

func TestResizeWorkerIsSingleFlightAndCoalescesLatest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan resizeRequest, 2)
	results := make(chan resizeDoneEvent, 2)
	started := make(chan [2]uint16, 2)
	release := make(chan struct{}, 2)
	inFlight := 0
	maxInFlight := 0
	var mu sync.Mutex
	resize := func(rows, cols uint16) (uint64, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		started <- [2]uint16{rows, cols}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return 0, nil
	}
	go runResizeWorker(ctx, requests, results)
	requests <- resizeRequest{id: 1, rows: 20, cols: 80, resize: resize}
	if got := <-started; got != [2]uint16{20, 80} {
		t.Fatalf("first=%v", got)
	}
	requests <- resizeRequest{id: 1, rows: 21, cols: 81, resize: resize}
	requests <- resizeRequest{id: 1, rows: 22, cols: 82, resize: resize}
	release <- struct{}{}
	<-results
	if got := <-started; got != [2]uint16{22, 82} {
		t.Fatalf("coalesced=%v", got)
	}
	release <- struct{}{}
	<-results
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Fatalf("max in flight=%d", maxInFlight)
	}
}

func TestApplyOrderedAttachChunkSplitsAtResizeBarrier(t *testing.T) {
	state := &tuiState{terminal: ducklord.NewTerminal(3, 5, 10), terminalGeneration: 2}
	resize := &resizeDoneEvent{id: 7, rows: 4, cols: 4, barrier: 5}
	chunk := attachOutputEvent{id: 7, text: "abcdeFG", runtimeGeneration: 2, startOffset: 0, outputOffset: 7}
	applyOrderedAttachChunk(state, chunk, &resize)
	if resize != nil {
		t.Fatal("resize barrier was not consumed")
	}
	if state.terminal.Rows != 4 || state.terminal.Cols != 4 || state.terminalOffset != 7 {
		t.Fatalf("terminal size=%dx%d offset=%d", state.terminal.Rows, state.terminal.Cols, state.terminalOffset)
	}
	if got := strings.ReplaceAll(state.terminal.Text(), "\n", ""); !strings.Contains(got, "abcdeFG") {
		t.Fatalf("ordered text=%q", state.terminal.Text())
	}
}

func TestApplyOrderedAttachChunkRejectsOffsetGap(t *testing.T) {
	state := &tuiState{terminal: ducklord.NewTerminal(3, 5, 10), terminalGeneration: 2, terminalOffset: 10}
	var resize *resizeDoneEvent
	applyOrderedAttachChunk(state, attachOutputEvent{id: 7, text: "bad", runtimeGeneration: 2, startOffset: 11, outputOffset: 14}, &resize)
	if state.terminalOffset != 10 || !strings.Contains(state.outputErr, "lost byte continuity") {
		t.Fatalf("offset=%d error=%q", state.terminalOffset, state.outputErr)
	}
}

func TestDrainBufferedAttachAppliesTailBeforeCompletion(t *testing.T) {
	state := &tuiState{terminal: ducklord.NewTerminal(3, 5, 10), terminalGeneration: 2}
	resize := &resizeDoneEvent{id: 7, rows: 4, cols: 4, barrier: 4}
	events := []attachOutputEvent{
		{id: 7, text: "tail", runtimeGeneration: 2, startOffset: 0, outputOffset: 4},
		{id: 7, done: true},
	}
	completion := drainBufferedAttach(state, events, &resize)
	if completion == nil || !completion.done || resize != nil {
		t.Fatalf("completion=%+v resize=%+v", completion, resize)
	}
	if state.terminalOffset != 4 || !strings.Contains(state.terminal.Text(), "tail") {
		t.Fatalf("tail offset=%d text=%q", state.terminalOffset, state.terminal.Text())
	}
}

func TestTUIRenderShowsApplicationCursorOnlyForFocusedPTY(t *testing.T) {
	terminal := ducklord.NewTerminal(3, 20, 0)
	terminal.Write([]byte("prompt> "))
	state := &tuiState{cfg: &ducklord.Config{}, sessions: []ducklord.RemoteSession{{Client: "host", Name: "agent", AgentType: "codex"}},
		terminal: terminal, focused: true, outputFresh: true, listPaneWidth: ducklord.DefaultSessionListWidth}
	var output bytes.Buffer
	state.render(&output)
	if !strings.Contains(output.String(), "\x1b[?25h") {
		t.Fatalf("focused PTY cursor was not shown: %q", output.String())
	}
	terminal.Write([]byte("\x1b[?25l"))
	output.Reset()
	state.render(&output)
	if strings.Contains(output.String(), "\x1b[?25h") {
		t.Fatalf("application-hidden cursor was shown: %q", output.String())
	}
}

func TestTUINotificationMenuStagesAndAtomicallySaves(t *testing.T) {
	instance := string(model.NewInstanceID())
	stateDir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(stateDir, "state.json")},
		sessions: []ducklord.RemoteSession{{Client: "host-a", InstanceID: instance, SessionID: "ABC123", Name: "agent"}}}
	state.beginNotificationSettings()
	if !state.notificationMode || len(state.notificationStaged) != len(model.NotificationCategories()) {
		t.Fatalf("menu state=%+v", state.notificationStaged)
	}
	if action := state.handleNotificationInput([]byte(" ")); action != "" || state.notificationStaged[model.NotificationTerminalAttention] {
		t.Fatalf("toggle action=%q staged=%+v", action, state.notificationStaged)
	}
	if state.activity().Enabled(instance, "ABC123", model.NotificationTerminalAttention) == false {
		t.Fatal("staged toggle mutated durable state before save")
	}
	state.saveNotificationSettings()
	if state.notificationMode || state.activity().Enabled(instance, "ABC123", model.NotificationTerminalAttention) {
		t.Fatalf("saved mode=%v state=%+v", state.notificationMode, state.activityState)
	}
	loaded, err := state.activityStore.Load()
	if err != nil || loaded.Enabled(instance, "ABC123", model.NotificationTerminalAttention) {
		t.Fatalf("loaded state=%+v err=%v", loaded, err)
	}
}

func TestTUINotificationSaveFailureLeavesLiveStateUntouched(t *testing.T) {
	instance := string(model.NewInstanceID())
	badTarget := filepath.Join(t.TempDir(), "state-is-a-directory")
	if err := os.MkdirAll(badTarget, 0700); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: badTarget},
		sessions: []ducklord.RemoteSession{{Client: "host-a", InstanceID: instance, SessionID: "ABC123", Name: "agent"}}}
	state.beginNotificationSettings()
	state.notificationStaged[model.NotificationTerminalAttention] = false
	state.saveNotificationSettings()
	if !state.notificationMode || !state.activity().Enabled(instance, "ABC123", model.NotificationTerminalAttention) {
		t.Fatalf("failed save mutated state or closed menu: mode=%v state=%+v", state.notificationMode, state.activityState)
	}
	if !strings.Contains(state.outputErr, "notification state") {
		t.Fatalf("missing save feedback: %q", state.outputErr)
	}
	var rendered strings.Builder
	state.renderNotificationSettings(&rendered, 1, 100, 30, state.currentSession())
	if !strings.Contains(rendered.String(), "Not saved:") {
		t.Fatalf("menu did not render error: %q", rendered.String())
	}
}

func TestTUIAcceptsNewInstanceAndRejectsLateOldGeneration(t *testing.T) {
	state := &tuiState{hostSync: map[string]ducklord.SessionUpdate{"host-a": {Client: "host-a", InstanceID: "old", Revision: 100, Generation: 1, State: "live"}},
		sessions: []ducklord.RemoteSession{{Client: "host-a", InstanceID: "old", SessionID: "ABC123", Name: "old"}}}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: "new", Revision: 0, Generation: 2, State: "reconnecting",
		Err: errors.New("subscription temporarily unavailable")})
	if state.hostIsLive("host-a") || len(state.sessions) != 1 || state.sessions[0].Name != "old" {
		t.Fatalf("new instance reconnect did not fence controls and retain rows: sessions=%+v sync=%+v", state.sessions, state.hostSync)
	}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: "old", Revision: 101, Generation: 1, State: "live",
		Sessions: []ducklord.RemoteSession{{Client: "host-a", InstanceID: "old", SessionID: "ABC123", Name: "late-old"}}})
	if state.hostIsLive("host-a") || state.sessions[0].Name != "old" {
		t.Fatalf("late old generation escaped reconnect fence: sessions=%+v sync=%+v", state.sessions, state.hostSync)
	}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: "new", Revision: 1, Generation: 2, State: "live",
		Sessions: []ducklord.RemoteSession{{Client: "host-a", InstanceID: "new", SessionID: "DEF456", Name: "new"}}})
	if len(state.sessions) != 1 || state.sessions[0].Name != "new" || !state.hostIsLive("host-a") {
		t.Fatalf("new instance not accepted: sessions=%+v sync=%+v", state.sessions, state.hostSync)
	}
}

func TestTUIReconnectStateDisablesHostControls(t *testing.T) {
	state := &tuiState{hostSync: map[string]ducklord.SessionUpdate{"host-a": {Client: "host-a", State: "reconnecting"}}}
	if state.hostIsLive("host-a") {
		t.Fatal("reconnecting host remained writable")
	}
	if !state.hostIsLive("legacy-host") {
		t.Fatal("legacy polling host was unexpectedly disabled")
	}
}

func TestTUICreatePromptUsesSelectedClient(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}, {Name: "client-b", Host: "client-b"}}}
	state := &tuiState{
		cfg: cfg,
		sessions: []ducklord.RemoteSession{
			{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell"},
			{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell"},
		},
		selected: 1,
	}
	state.beginCreate()
	if !state.newSessionMode || state.newSessionClient != "client-b" || state.newSessionStep != "kind" {
		t.Fatalf("create state = newSessionMode %v client %q step %q", state.newSessionMode, state.newSessionClient, state.newSessionStep)
	}
	for _, b := range []byte("new") {
		state.handleCreateInput([]byte{b})
	}
	if state.newSessionLine != "new" {
		t.Fatalf("create input = %q", state.newSessionLine)
	}
	state.handleCreateInput([]byte{0x7f})
	if state.newSessionLine != "ne" {
		t.Fatalf("create input after backspace = %q", state.newSessionLine)
	}
	if action := state.handleCreateInput([]byte("\x1b")); action != "cancel" {
		t.Fatalf("cancel action = %q", action)
	}
}

func TestTUICreateWizardBuildsShellSessionFromProject(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}, {Name: "client-b", Host: "client-b"}}}
	state := &tuiState{
		cfg: cfg,
		runner: fakeRunner{projects: []ducklord.RemoteProject{
			{Name: "duckway", Path: "/home/duck/duckway", Source: "duckway-client"},
		}, agents: []ducklord.RemoteAgent{{Type: "shell", Command: []string{"/bin/bash"}}}},
	}
	state.beginCreate()
	state.newSessionLine = "shell"
	createSubmit(t, state)
	state.newSessionLine = "2"
	createSubmit(t, state)
	if state.newSessionClient != "client-b" || state.newSessionStep != "project" {
		t.Fatalf("client=%q step=%q", state.newSessionClient, state.newSessionStep)
	}
	state.newSessionLine = "1"
	createSubmit(t, state)
	if state.newSessionStep != "handle" {
		t.Fatalf("step=%q err=%s", state.newSessionStep, state.newSessionErr)
	}
	state.newSessionLine = ""
	clientName, name, args, ready := createSubmit(t, state)
	want := []string{"--name", "duckway", "--kind", "shell", "--cwd", "/home/duck/duckway", "--", "/bin/bash"}
	if !ready || name != "duckway" || clientName != "client-b" || strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ready=%v name=%q client=%q args=%#v", ready, name, clientName, args)
	}
}

func TestTUICreateWizardRejectsCustomProjectPath(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}}}
	state := &tuiState{cfg: cfg, runner: fakeRunner{projects: []ducklord.RemoteProject{{Name: "app", Path: "/work/app", Source: "duckway-client"}}, agents: []ducklord.RemoteAgent{{Type: "codex", Command: []string{"/usr/bin/codex"}}}}}
	state.beginCreate()
	state.newSessionLine = ""
	createSubmit(t, state) // agent
	state.newSessionLine = ""
	createSubmit(t, state) // host
	state.newSessionLine = "/other/path"
	if _, _, _, _, err := state.submitCreateStep(context.Background(), make(chan createDiscoveryEvent, 1)); err == nil {
		t.Fatal("arbitrary project path accepted")
	}
}

func TestTUICreateWizardRejectsAgentNotReportedByRemoteHost(t *testing.T) {
	state := &tuiState{newSessionStep: "agent", newSessionAgents: []ducklord.RemoteAgent{{Type: "shell", Command: []string{"/bin/sh"}}}, newSessionLine: "codex"}
	if _, _, _, ready, err := state.submitCreateStep(context.Background(), make(chan createDiscoveryEvent, 1)); err == nil || ready || state.newSessionStep != "agent" {
		t.Fatalf("ready=%v step=%q err=%v", ready, state.newSessionStep, err)
	}
}

func TestTUICreateWizardKeepsProjectStepWhenRemoteDiscoveryFails(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}
	state := &tuiState{cfg: cfg, runner: fakeRunner{agentsErr: errors.New("remote PATH unavailable")}, newSessionMode: true, newSessionStep: "project", newSessionClient: "host",
		newSessionProjects: []ducklord.RemoteProject{{Name: "app", Path: "/work/app"}}, newSessionLine: "1"}
	done := make(chan createDiscoveryEvent, 1)
	if _, _, _, ready, err := state.submitCreateStep(context.Background(), done); err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	state.applyCreateDiscovery(<-done)
	if state.newSessionStep != "project" || !strings.Contains(state.newSessionErr, "remote PATH") {
		t.Fatalf("step=%q err=%q", state.newSessionStep, state.newSessionErr)
	}
}

func TestTUICreateDiscoveryCancellationIsImmediateAndStaleResultIsIgnored(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	runner := &blockingCreateRunner{fakeRunner: fakeRunner{agents: []ducklord.RemoteAgent{{Type: "codex", Command: []string{"codex"}}}}, started: started, release: release}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: runner,
		newSessionMode: true, newSessionKind: model.KindAgent, newSessionStep: "project", newSessionClient: "host",
		newSessionProjects: []ducklord.RemoteProject{{Name: "app", Path: "/work/app", Source: "duckway-client"}}, newSessionLine: "1"}
	done := make(chan createDiscoveryEvent, 1)
	if _, _, _, _, err := state.submitCreateStep(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	<-started
	state.cancelCreate()
	if state.newSessionMode || state.newSessionDiscovering {
		t.Fatal("escape did not cancel wizard immediately")
	}
	close(release)
	select {
	case event := <-done:
		state.applyCreateDiscovery(event)
	case <-time.After(100 * time.Millisecond):
	}
	if state.newSessionMode || state.newSessionStep != "" {
		t.Fatalf("stale completion restored wizard: %+v", state)
	}
	state.createWorkers.Wait()
}

func TestTUICreateDiscoveryRejectsHostGenerationChange(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	runner := &blockingCreateRunner{fakeRunner: fakeRunner{projects: []ducklord.RemoteProject{{Name: "app", Path: "/work/app", Source: "duckway-client"}}}, started: started, release: release, blockProjects: true}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: runner, hostSync: map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 1, InstanceID: "old"}},
		newSessionMode: true, newSessionKind: model.KindAgent, newSessionStep: "host", newSessionClient: "host"}
	done := make(chan createDiscoveryEvent, 1)
	if _, _, _, _, err := state.submitCreateStep(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	<-started
	state.hostSync["host"] = ducklord.SessionUpdate{Client: "host", State: "live", Generation: 2, InstanceID: "new"}
	close(release)
	state.applyCreateDiscovery(<-done)
	if state.newSessionStep != "host" || !strings.Contains(state.newSessionErr, "host changed") {
		t.Fatalf("step=%q err=%q", state.newSessionStep, state.newSessionErr)
	}
}

func TestTUIStartIsFencedWhenHostGenerationChanges(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionStarting: true, newSessionClient: "host", newSessionStep: "handle", newSessionStartGeneration: 3, newSessionStartInstance: "old"}
	if state.invalidateCreateStart(ducklord.SessionUpdate{Client: "other", State: "live", Generation: 4, InstanceID: "new"}) {
		t.Fatal("unrelated host canceled start")
	}
	if !state.invalidateCreateStart(ducklord.SessionUpdate{Client: "host", State: "live", Generation: 4, InstanceID: "new"}) {
		t.Fatal("changed host did not cancel start")
	}
	if state.newSessionStarting || state.newSessionStep != "host" || !strings.Contains(state.newSessionErr, "refresh") {
		t.Fatalf("state=%+v", state)
	}
}

func TestTUIStartCompletionSelectsDuplicateHandleBySessionID(t *testing.T) {
	state := &tuiState{runner: fakeRunner{sessions: []ducklord.RemoteSession{
		{Client: "host", Name: "中文工作階段", SessionID: "OLD111", InstanceID: "instance"},
		{Client: "host", Name: "中文工作階段", SessionID: "NEW222", InstanceID: "instance"},
	}}, cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, activityState: ducklord.NewActivityState()}
	state.completeNewSessionStart(context.Background(), "host", "NEW222", nil)
	if got := state.currentSession().SessionID; got != "NEW222" {
		t.Fatalf("selected session = %q", got)
	}
}

func TestTUICreateFinalRevalidationReturnsToStaleProject(t *testing.T) {
	selected := ducklord.RemoteProject{Name: "app", Path: "/work/app", Source: "duckway-client"}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: fakeRunner{projects: nil},
		newSessionMode: true, newSessionKind: model.KindAgent, newSessionStep: "handle", newSessionClient: "host", newSessionProject: selected,
		newSessionCWD: selected.Path, newSessionAgent: "codex", newSessionCommand: []string{"codex"}, newSessionLine: "work"}
	createSubmit(t, state)
	if state.newSessionStep != "project" || !strings.Contains(state.newSessionErr, "no longer available") {
		t.Fatalf("step=%q err=%q", state.newSessionStep, state.newSessionErr)
	}
}

func TestTUICreateFinalRevalidationReturnsToUnavailableAgent(t *testing.T) {
	selected := ducklord.RemoteProject{Name: "app", Path: "/work/app", Source: "duckway-client"}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: fakeRunner{projects: []ducklord.RemoteProject{selected}, agents: []ducklord.RemoteAgent{{Type: "claude_code", Command: []string{"claude"}}}},
		newSessionMode: true, newSessionKind: model.KindAgent, newSessionStep: "handle", newSessionClient: "host", newSessionProject: selected,
		newSessionCWD: selected.Path, newSessionAgent: "codex", newSessionCommand: []string{"codex"}, newSessionLine: "work"}
	createSubmit(t, state)
	if state.newSessionStep != "agent" || !strings.Contains(state.newSessionErr, "no longer available") {
		t.Fatalf("step=%q err=%q", state.newSessionStep, state.newSessionErr)
	}
}

func createSubmit(t *testing.T, state *tuiState) (client, name string, args []string, ready bool) {
	t.Helper()
	done := make(chan createDiscoveryEvent, 1)
	_, _, _, immediate, err := state.submitCreateStep(context.Background(), done)
	if err != nil {
		t.Fatal(err)
	}
	if immediate {
		t.Fatal("wizard unexpectedly completed synchronously")
	}
	if !state.newSessionDiscovering {
		return "", "", nil, false
	}
	event := <-done
	client, name, args, ready = state.applyCreateDiscovery(event)
	return
}

type blockingCreateRunner struct {
	fakeRunner
	started       chan struct{}
	release       chan struct{}
	blockProjects bool
}

func (r *blockingCreateRunner) wait(ctx context.Context) error {
	select {
	case r.started <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *blockingCreateRunner) Projects(ctx context.Context, c ducklord.Client) ([]ducklord.RemoteProject, error) {
	if r.blockProjects {
		if err := r.wait(ctx); err != nil {
			return nil, err
		}
	}
	return r.fakeRunner.Projects(ctx, c)
}
func (r *blockingCreateRunner) Agents(ctx context.Context, c ducklord.Client, cwd string) ([]ducklord.RemoteAgent, error) {
	if !r.blockProjects {
		if err := r.wait(ctx); err != nil {
			return nil, err
		}
	}
	return r.fakeRunner.Agents(ctx, c, cwd)
}

func TestTUIAddClientFromSSHHostProbesAndSaves(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}}}
	state := &tuiState{
		cfg:     cfg,
		cfgPath: config,
		runner:  fakeRunner{probe: ducklord.DucklionProbe{Available: true, Command: "ducklion", Version: "ducklion v1"}},
		addClientHosts: []ducklord.SSHHost{
			{Name: "duck@example.internal"},
		},
		addClientMode: true,
		addClientLine: "1",
	}
	if err := state.submitAddClient(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.addClientMode {
		t.Fatal("add client prompt still active")
	}
	loaded, err := ducklord.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := loaded.Client("example-internal")
	if !ok {
		t.Fatalf("saved clients = %+v", loaded.Clients)
	}
	if client.User != "duck" || client.Host != "example.internal" || client.Ducklion != "ducklion" {
		t.Fatalf("client = %+v", client)
	}
}

func TestTUIAddClientAcceptsSSHCommandTarget(t *testing.T) {
	cfg := &ducklord.Config{}
	state := &tuiState{cfg: cfg}
	client, err := state.clientFromAddLine("ssh -p 2222 -i /tmp/id_ed25519 duck@client-a")
	if err != nil {
		t.Fatal(err)
	}
	if client.Name != "client-a" || client.User != "duck" || client.Host != "client-a" || client.SSH != "ssh -p 2222 -i /tmp/id_ed25519" {
		t.Fatalf("client = %+v", client)
	}
}

func TestTUIAddClientInstallsMissingDucklionAndReprobes(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &ducklord.Config{}
	runner := &recordingRunner{
		installPath: "/home/duck/.local/bin/ducklion",
		probes: []ducklord.DucklionProbe{
			{Available: false},
			{Available: true, Command: "/home/duck/.local/bin/ducklion", Version: "ducklion v2", ListOK: true, Sessions: 1},
		},
	}
	state := &tuiState{
		cfg:           cfg,
		cfgPath:       config,
		runner:        runner,
		addClientMode: true,
		addClientLine: "ssh duck@client-a",
	}
	if err := state.submitAddClient(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.installClient != "client-a" || runner.probeCalls != 2 {
		t.Fatalf("installClient=%q probeCalls=%d", runner.installClient, runner.probeCalls)
	}
	loaded, err := ducklord.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := loaded.Client("client-a")
	if !ok {
		t.Fatalf("saved clients = %+v", loaded.Clients)
	}
	if client.Ducklion != "/home/duck/.local/bin/ducklion" {
		t.Fatalf("ducklion = %q", client.Ducklion)
	}
	if !strings.Contains(state.outputErr, "installed ducklion") || !strings.Contains(state.outputErr, "ducklion v2") {
		t.Fatalf("outputErr = %q", state.outputErr)
	}
}

func TestTUIRemoveSelectedHostEntryUpdatesCurrentConfig(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &ducklord.Config{Clients: []ducklord.Client{
		{Name: "client-a", Host: "client-a", Group: "lab"},
		{Name: "client-b", Host: "client-b", Group: "lab"},
	}}
	if err := ducklord.SaveConfig(config, cfg); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{
		cfg:     cfg,
		cfgPath: config,
		runner:  fakeRunner{},
		sessions: []ducklord.RemoteSession{
			{Client: "client-a", Name: "alpha", Status: "running"},
			{Client: "client-b", Name: "beta", Status: "running"},
		},
		selected: 1,
	}
	if err := state.removeSelectedClient(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := ducklord.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Client("client-b"); ok {
		t.Fatalf("client-b still present: %+v", loaded.Clients)
	}
	if _, ok := loaded.Client("client-a"); !ok {
		t.Fatalf("client-a missing: %+v", loaded.Clients)
	}
	if !strings.Contains(state.outputErr, "removed host entry client-b") {
		t.Fatalf("outputErr = %q", state.outputErr)
	}
}

func TestTUICompleteNewSessionStartRefreshesSelectsAndReads(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}}}
	runner := &recordingRunner{
		readText: "created\n",
		sessionsByClient: map[string][]ducklord.RemoteSession{
			"client-a": {{Client: "client-a", Name: "fresh", Status: "running", AgentType: "shell"}},
		},
	}
	state := &tuiState{
		cfg:                cfg,
		runner:             runner,
		hashes:             map[string]string{},
		newSessionMode:     true,
		newSessionClient:   "client-a",
		newSessionLine:     "fresh",
		newSessionErr:      "starting...",
		newSessionStarting: true,
	}
	state.completeNewSessionStart(context.Background(), "client-a", "fresh", nil)
	if state.newSessionMode || state.newSessionStarting || state.newSessionErr != "" {
		t.Fatalf("new session state not cleared: mode=%v starting=%v err=%q", state.newSessionMode, state.newSessionStarting, state.newSessionErr)
	}
	if state.currentKey() != "/client-a/fresh" || state.outputText != "created\n" {
		t.Fatalf("key=%q output=%q", state.currentKey(), state.outputText)
	}
}

func TestTUICompleteNewSessionStartKeepsPromptOnError(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionLine: "fresh", newSessionStarting: true}
	state.completeNewSessionStart(context.Background(), "client-a", "fresh", fmt.Errorf("already running"))
	if !state.newSessionMode || state.newSessionLine != "fresh" || state.newSessionStarting || !strings.Contains(state.newSessionErr, "already running") {
		t.Fatalf("state after error = %+v", state)
	}
}

func TestTUIOfflineRowDoesNotRead(t *testing.T) {
	runner := &recordingRunner{readText: "pane\n"}
	state := &tuiState{
		runner:   runner,
		sessions: []ducklord.RemoteSession{{Client: "client-a", Name: "(offline)", Status: "error", Error: "ssh failed"}},
		selected: 0,
	}
	state.refreshSelectedOutput(context.Background())
	if runner.readClient != "" {
		t.Fatalf("offline row read target = %s/%s", runner.readClient, runner.readSession)
	}
	if !strings.Contains(state.outputErr, "ssh failed") {
		t.Fatalf("outputErr = %q", state.outputErr)
	}
}

func TestTUIMouseClickOnlySelectsMenuPane(t *testing.T) {
	state := &tuiState{
		sessions: []ducklord.RemoteSession{
			{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell", Group: "lab"},
			{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell", Group: "lab"},
		},
	}
	if idx, ok := state.sessionIndexForMouse("\x1b[<0;2;6M"); !ok || idx != 0 {
		t.Fatalf("left pane click = idx %d ok %v", idx, ok)
	}
	if _, ok := state.sessionIndexForMouse("\x1b[<0;70;6M"); ok {
		t.Fatal("right pane click selected a session")
	}
}

func TestTUIRightClickMenuRowSelectsAndAttaches(t *testing.T) {
	state := &tuiState{
		sessions: []ducklord.RemoteSession{
			{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell", Group: "lab"},
			{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell", Group: "lab"},
		},
	}
	if action := state.handleInput([]byte("\x1b[<2;2;7M")); action != "attach" {
		t.Fatalf("right-click action = %q", action)
	}
	if state.selected != 1 {
		t.Fatalf("selected = %d, want 1", state.selected)
	}
}

func TestTUIRightClickContentPaneKeepsSelectionButAttaches(t *testing.T) {
	state := &tuiState{
		selected: 1,
		sessions: []ducklord.RemoteSession{
			{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell", Group: "lab"},
			{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell", Group: "lab"},
		},
	}
	if action := state.handleInput([]byte("\x1b[<2;70;7M")); action != "attach" {
		t.Fatalf("right pane right-click action = %q", action)
	}
	if state.selected != 1 {
		t.Fatalf("selected changed to %d", state.selected)
	}
}

func TestSanitizeTerminalTextStripsControlBytes(t *testing.T) {
	got := sanitizeTerminalText("ok\x1b]52;c;pw\a\nnext\rline\x9b2J")
	for _, b := range []byte{0x1b, 0x07, 0x9b, 0x0d} {
		if strings.ContainsRune(got, rune(b)) {
			t.Fatalf("control byte 0x%x survived: %q", b, got)
		}
	}
	if !strings.Contains(got, "ok ]52;c;pw") || !strings.Contains(got, "next\nline") {
		t.Fatalf("sanitized text = %q", got)
	}
}

func TestSuperviseAttachReportsCommandError(t *testing.T) {
	stdoutR, stdoutW := io.Pipe()
	done := make(chan error, 1)
	session := &ducklord.AttachSession{Stdin: nopWriteCloser{}, Stdout: stdoutR, Done: done}
	out := make(chan attachOutputEvent, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go superviseAttach(ctx, 7, session, out)
	_, _ = stdoutW.Write([]byte("final-tail"))
	_ = stdoutW.Close()
	done <- fmt.Errorf("remote failed")
	first := <-out
	if first.done || first.text != "final-tail" {
		t.Fatalf("first ordered attach event = %+v", first)
	}
	got := <-out
	if got.id != 7 || !got.done || got.err == nil || !strings.Contains(got.err.Error(), "remote failed") {
		t.Fatalf("completion = %+v", got)
	}
}

func TestAppendOutputTextPreservesChunkNewlines(t *testing.T) {
	got := appendOutputText(appendOutputText("", "foo\n", 10), "bar\n", 10)
	if got != "foo\nbar\n" {
		t.Fatalf("output = %q", got)
	}
	got = appendOutputText("a\nb\nc\n", "d\n", 3)
	if got != "b\nc\nd\n" {
		t.Fatalf("tail output = %q", got)
	}
}

func TestAppendOutputTextAppliesBackspaceEcho(t *testing.T) {
	got := appendOutputText("client-a:~$ abc", "\b \bd\n", 10)
	if got != "client-a:~$ abd\n" {
		t.Fatalf("backspace echo output = %q", got)
	}
	got = appendOutputText("client-a:~$ abc", string([]byte{0x7f})+"d\n", 10)
	if got != "client-a:~$ abd\n" {
		t.Fatalf("del output = %q", got)
	}
}

func TestNextInputEventSplitsCoalescedKeys(t *testing.T) {
	input := []byte("j\recho ok\n")
	want := []string{"j", "\r", "e", "c", "h", "o", " ", "o", "k", "\n"}
	for i, expected := range want {
		event, rest, ok := nextInputEvent(input)
		if !ok {
			t.Fatalf("event %d missing", i)
		}
		if string(event) != expected {
			t.Fatalf("event %d = %q, want %q", i, event, expected)
		}
		input = rest
	}
	if len(input) != 0 {
		t.Fatalf("remaining input = %q", input)
	}
}

func TestNextInputEventKeepsMouseSequenceTogether(t *testing.T) {
	event, rest, ok := nextInputEvent([]byte("\x1b[<2;2;7Mabc"))
	if !ok {
		t.Fatal("mouse event incomplete")
	}
	if string(event) != "\x1b[<2;2;7M" || string(rest) != "abc" {
		t.Fatalf("event=%q rest=%q", event, rest)
	}
	_, rest, ok = nextInputEvent([]byte("\x1b[<2;2"))
	if ok || string(rest) != "\x1b[<2;2" {
		t.Fatalf("incomplete mouse sequence parsed ok=%v rest=%q", ok, rest)
	}
}

func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{"hosts":[{"name":"client-a","host":"client-a","user":"duck","group":"demo"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakeRunner struct {
	sessions         []ducklord.RemoteSession
	sessionsByClient map[string][]ducklord.RemoteSession
	readText         string
	readErr          error
	projects         []ducklord.RemoteProject
	agents           []ducklord.RemoteAgent
	agentsErr        error
	probe            ducklord.DucklionProbe
	installPath      string
}

func (f fakeRunner) Sessions(_ context.Context, client ducklord.Client, _ int) ([]ducklord.RemoteSession, error) {
	if f.sessionsByClient != nil {
		return f.sessionsByClient[client.Name], nil
	}
	return f.sessions, nil
}
func (f fakeRunner) Read(context.Context, ducklord.Client, string, int) (string, error) {
	return f.readText, f.readErr
}
func (f fakeRunner) Send(context.Context, ducklord.Client, string, string) error { return nil }
func (f fakeRunner) Start(context.Context, ducklord.Client, []string) (string, error) {
	return "ABC123", nil
}
func (f fakeRunner) Stop(context.Context, ducklord.Client, string) error { return nil }
func (f fakeRunner) Lifecycle(context.Context, ducklord.Client, string, protocol.SessionLifecycleOperation, protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error) {
	return protocol.SessionLifecycleResult{SessionID: "ABC123", Operation: protocol.SessionLifecycleRestart, Mode: protocol.SessionLifecycleWait, State: protocol.SessionLifecycleCompleted, OwnershipEpoch: 1, RuntimeGeneration: 2}, nil
}
func (f fakeRunner) Yield(context.Context, ducklord.Client, string, bool) (protocol.SessionYieldResult, error) {
	return protocol.SessionYieldResult{}, nil
}
func (f fakeRunner) Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error) {
	return f.projects, nil
}
func (f fakeRunner) Agents(context.Context, ducklord.Client, string) ([]ducklord.RemoteAgent, error) {
	if f.agentsErr != nil {
		return nil, f.agentsErr
	}
	if len(f.agents) != 0 {
		return f.agents, nil
	}
	return []ducklord.RemoteAgent{{Type: "shell", Command: []string{"bash"}}, {Type: "codex", Command: []string{"codex"}}, {Type: "claude_code", Command: []string{"claude"}}}, nil
}
func (f fakeRunner) ProbeDucklion(context.Context, ducklord.Client) (ducklord.DucklionProbe, error) {
	return f.probe, nil
}
func (f fakeRunner) InstallDucklion(context.Context, ducklord.Client, string, string) (string, error) {
	if f.installPath != "" {
		return f.installPath, nil
	}
	return "/home/duck/.local/bin/ducklion", nil
}
func (f fakeRunner) Attach(ducklord.Client, string) error { return nil }
func (f fakeRunner) AttachStream(context.Context, ducklord.Client, string) (*ducklord.AttachSession, error) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	_ = stdinR.Close()
	_ = stdoutW.Close()
	done := make(chan error, 1)
	done <- nil
	return &ducklord.AttachSession{Stdin: stdinW, Stdout: stdoutR, Done: done}, nil
}

type recordingRunner struct {
	readText         string
	readClient       string
	readSession      string
	startClient      string
	startArgs        []string
	installClient    string
	installSource    string
	installDest      string
	installPath      string
	probes           []ducklord.DucklionProbe
	probeCalls       int
	sessionsByClient map[string][]ducklord.RemoteSession
	yieldClient      string
	yieldSession     string
	yieldWait        bool
	yieldResult      protocol.SessionYieldResult
	lifecycleClient  string
	lifecycleSession string
	lifecycleOp      protocol.SessionLifecycleOperation
	lifecycleMode    protocol.SessionLifecycleMode
}

func (r *recordingRunner) Sessions(_ context.Context, client ducklord.Client, _ int) ([]ducklord.RemoteSession, error) {
	if r.sessionsByClient != nil {
		return r.sessionsByClient[client.Name], nil
	}
	return nil, nil
}
func (r *recordingRunner) Read(_ context.Context, client ducklord.Client, session string, _ int) (string, error) {
	r.readClient = client.Name
	r.readSession = session
	return r.readText, nil
}
func (r *recordingRunner) Send(context.Context, ducklord.Client, string, string) error {
	return nil
}
func (r *recordingRunner) Start(_ context.Context, client ducklord.Client, args []string) (string, error) {
	r.startClient = client.Name
	r.startArgs = append([]string(nil), args...)
	return "ABC123", nil
}
func (r *recordingRunner) Stop(context.Context, ducklord.Client, string) error { return nil }
func (r *recordingRunner) Lifecycle(_ context.Context, client ducklord.Client, session string, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error) {
	r.lifecycleClient, r.lifecycleSession, r.lifecycleOp, r.lifecycleMode = client.Name, session, operation, mode
	return protocol.SessionLifecycleResult{SessionID: session, Operation: operation, Mode: mode, State: protocol.SessionLifecycleCompleted, OwnershipEpoch: 1, RuntimeGeneration: 2}, nil
}
func (r *recordingRunner) Yield(_ context.Context, client ducklord.Client, session string, wait bool) (protocol.SessionYieldResult, error) {
	r.yieldClient = client.Name
	r.yieldSession = session
	r.yieldWait = wait
	return r.yieldResult, nil
}
func (r *recordingRunner) Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error) {
	return nil, nil
}
func (r *recordingRunner) Agents(context.Context, ducklord.Client, string) ([]ducklord.RemoteAgent, error) {
	return []ducklord.RemoteAgent{{Type: "shell", Command: []string{"bash"}}, {Type: "codex", Command: []string{"codex"}}}, nil
}
func (r *recordingRunner) ProbeDucklion(context.Context, ducklord.Client) (ducklord.DucklionProbe, error) {
	r.probeCalls++
	if len(r.probes) >= r.probeCalls {
		return r.probes[r.probeCalls-1], nil
	}
	return ducklord.DucklionProbe{}, nil
}
func (r *recordingRunner) InstallDucklion(_ context.Context, client ducklord.Client, source, dest string) (string, error) {
	r.installClient = client.Name
	r.installSource = source
	r.installDest = dest
	if r.installPath != "" {
		return r.installPath, nil
	}
	return "/home/duck/.local/bin/ducklion", nil
}
func (r *recordingRunner) Attach(ducklord.Client, string) error { return nil }
func (r *recordingRunner) AttachStream(context.Context, ducklord.Client, string) (*ducklord.AttachSession, error) {
	return fakeRunner{}.AttachStream(context.Background(), ducklord.Client{}, "")
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
