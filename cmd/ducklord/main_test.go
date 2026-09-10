package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
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
		{"start", "client-a", "--name", "bad\nname", "--config", config, "--", "bash"},
		{"start", "client-a", "--bad", "--config", config, "--", "bash"},
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
	err := run([]string{"start", "client-a", "--name", "alpha", "--agent", "shell", "--cwd", "/tmp", "--config", config, "--", "bash"}, io.Discard, runner)
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
	if err := run([]string{"start", "client-a", "--name", "terminal", "--kind", "shell", "--cwd", "/tmp", "--config", config, "--", "bash"}, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	want := []string{"--name", "terminal", "--kind", "shell", "--cwd", "/tmp", "--", "bash"}
	if strings.Join(runner.startArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("start args=%#v", runner.startArgs)
	}
	for _, args := range [][]string{
		{"start", "client-a", "--name", "bad-shell", "--kind", "shell", "--agent", "codex", "--cwd", "/tmp", "--config", config, "--", "bash"},
		{"start", "client-a", "--name", "bad-shell", "--kind", "shell", "--cwd", "/tmp", "--config", config, "--", "bash", "-l"},
		{"start", "client-a", "--name", "bad-kind", "--kind", "other", "--cwd", "/tmp", "--config", config, "--", "bash"},
	} {
		if err := run(args, io.Discard, runner); err == nil {
			t.Fatalf("args %#v accepted", args)
		}
	}
}

func TestLoadWithFlagsStopsAtCommandDelimiter(t *testing.T) {
	config := writeConfig(t)
	_, rest, err := loadWithFlags([]string{
		"client-a", "--config", config, "--",
		"sh", "-c", "printf ok", "--config", "child.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"client-a", "--", "sh", "-c", "printf ok", "--config", "child.yaml"}
	if strings.Join(rest, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("rest=%#v, want %#v", rest, want)
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
	session := ducklord.RemoteSession{Client: "host", Name: "agent", SessionID: "ABC123", Kind: string(model.KindAgent), Status: "running"}
	state := &tuiState{ownerName: "desk", lifecycleConfirm: protocol.SessionLifecycleDestroy, lifecycleTarget: session,
		sessions: []ducklord.RemoteSession{session}}
	var out bytes.Buffer
	state.render(&out)
	got := out.String()
	for _, want := range []string{"DESTROY SESSION", "Enter now", "w wait", "f force-cancel", "permanently removes", "ABC123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("confirmation missing %q in %q", want, got)
		}
	}
}

func TestTUIRenderExplainsImmediateShellLifecycle(t *testing.T) {
	session := ducklord.RemoteSession{Client: "host", Name: "shell", SessionID: "ABC123", Kind: string(model.KindShell), Status: "running"}
	state := &tuiState{ownerName: "desk", lifecycleConfirm: protocol.SessionLifecycleRestart, lifecycleTarget: session,
		sessions: []ducklord.RemoteSession{session}}
	var out bytes.Buffer
	state.render(&out)
	got := out.String()
	for _, want := range []string{"Enter terminate process immediately", "it never waits"} {
		if !strings.Contains(got, want) {
			t.Fatalf("shell confirmation missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "force-cancel") || strings.Contains(got, "w wait") {
		t.Fatalf("shell confirmation exposed agent lifecycle modes: %q", got)
	}
}

func TestLifecycleConfirmationIsBoundToExactSessionRevision(t *testing.T) {
	target := ducklord.RemoteSession{Client: "host-a", InstanceID: "instance-a", SessionID: "ABC123", Name: "target",
		Kind: string(model.KindAgent), WriterKind: string(model.OwnerTerminal), WriterID: "desk", OwnershipEpoch: 4, RuntimeGeneration: 7}
	state := &tuiState{ownerName: "desk", selected: 0, lifecycleConfirm: protocol.SessionLifecycleDestroy, lifecycleTarget: target,
		sessions: []ducklord.RemoteSession{target}}
	if !state.lifecycleTargetIsCurrent() {
		t.Fatal("unchanged lifecycle target was rejected")
	}

	// A list refresh may select another row with the same short ID. It must not
	// inherit the destructive confirmation from the original host/instance.
	state.sessions = []ducklord.RemoteSession{{Client: "host-b", InstanceID: "instance-b", SessionID: "ABC123", Name: "other",
		Kind: string(model.KindAgent), WriterKind: string(model.OwnerTerminal), WriterID: "desk", OwnershipEpoch: 4, RuntimeGeneration: 7}}
	if state.lifecycleTargetIsCurrent() {
		t.Fatal("confirmation followed a same-ID session onto another host")
	}

	state.sessions = []ducklord.RemoteSession{target}
	state.sessions[0].RuntimeGeneration++
	if state.lifecycleTargetIsCurrent() {
		t.Fatal("confirmation advanced to a newer runtime generation")
	}
	state.sessions[0] = target
	state.sessions[0].OwnershipEpoch++
	if state.lifecycleTargetIsCurrent() {
		t.Fatal("confirmation advanced to a newer ownership epoch")
	}
}

func TestLifecycleConfirmationRendersCapturedTargetAfterSelectionMoves(t *testing.T) {
	target := ducklord.RemoteSession{Client: "host-a", InstanceID: "instance-a", SessionID: "TARGET", Name: "original", Kind: string(model.KindAgent)}
	state := &tuiState{lifecycleConfirm: protocol.SessionLifecycleDestroy, lifecycleTarget: target,
		sessions: []ducklord.RemoteSession{{Client: "host-b", InstanceID: "instance-b", SessionID: "OTHER", Name: "new selection", Kind: string(model.KindAgent)}}}
	var out bytes.Buffer
	state.renderLifecycleModal(&out, 80, 24)
	if got := out.String(); !strings.Contains(got, "original") || !strings.Contains(got, "TARGET") || strings.Contains(got, "new selection") {
		t.Fatalf("confirmation rendered mutable selection: %q", got)
	}
}

func TestParseSessionsArgsSupportsMachineReadableInventory(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		client   string
		json     bool
		wantFail bool
	}{
		{args: []string{"host-a"}, client: "host-a"},
		{args: []string{"--json", "host-a"}, client: "host-a", json: true},
		{args: []string{"host-a", "--json"}, client: "host-a", json: true},
		{args: nil, wantFail: true},
		{args: []string{"host-a", "host-b"}, wantFail: true},
	} {
		client, jsonOutput, err := parseSessionsArgs(tc.args)
		if (err != nil) != tc.wantFail || client != tc.client || jsonOutput != tc.json {
			t.Fatalf("parseSessionsArgs(%q) = %q,%v,%v", tc.args, client, jsonOutput, err)
		}
	}
}

func TestSessionActionsReflectKindOwnerTaskAndAdapter(t *testing.T) {
	live := map[string]ducklord.SessionUpdate{"host": {State: "live"}}
	state := &tuiState{ownerName: "desk", hostSync: live}
	shell := ducklord.RemoteSession{Client: "host", InstanceID: "instance", SessionID: "SHELL1", Kind: string(model.KindShell), Status: string(model.StatusRunning)}
	shellActions := state.sessionActions(shell)
	for _, action := range shellActions {
		if strings.HasPrefix(action.ID, "yield") || strings.Contains(action.ID, "wait") || strings.Contains(action.ID, "force") {
			t.Fatalf("shell exposed agent-only action %#v", action)
		}
	}
	if len(shellActions) != 6 || shellActions[1].ID != "reconnect" || !shellActions[1].Enabled || shellActions[3].Mode != protocol.SessionLifecycleImmediate {
		t.Fatalf("shell actions=%#v", shellActions)
	}

	agent := ducklord.RemoteSession{Client: "host", InstanceID: "instance", SessionID: "AGENT1", Kind: string(model.KindAgent), Status: string(model.StatusRunning),
		WriterKind: string(model.OwnerCC), WriterID: "channel", OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: string(model.TaskRunning), AdapterState: string(model.AdapterHealthy)}
	agentActions := state.sessionActions(agent)
	if agentActions[0].Label != "View PTY (read-only)" || agentActions[1].ID != "reconnect" || !agentActions[1].Enabled || agentActions[2].Enabled || !agentActions[3].Enabled {
		t.Fatalf("busy CC-owned agent actions=%#v", agentActions[:4])
	}
	for _, action := range agentActions {
		if action.Operation != "" && action.Enabled {
			t.Fatalf("read-only agent enabled lifecycle action %#v", action)
		}
	}
	agent.WriterKind, agent.WriterID = string(model.OwnerTerminal), "desk"
	agentActions = state.sessionActions(agent)
	actionByID := make(map[string]sessionAction, len(agentActions))
	for _, action := range agentActions {
		actionByID[action.ID] = action
	}
	if actionByID["restart"].Enabled || !actionByID["restart-wait"].Enabled || !actionByID["restart-force"].Enabled {
		t.Fatalf("busy terminal-owned lifecycle modes=%#v", actionByID)
	}
	agent.WriterKind, agent.WriterID = string(model.OwnerCC), "channel"
	agent.AdapterState = string(model.AdapterUnhealthy)
	agent.TaskState = string(model.TaskIdle)
	agentActions = state.sessionActions(agent)
	if agentActions[2].Enabled || agentActions[3].Enabled {
		t.Fatalf("unhealthy adapter enabled yield actions=%#v", agentActions[:4])
	}
}

func TestSessionActionsDisableLifecycleWhileAnotherOperationIsPending(t *testing.T) {
	session := ducklord.RemoteSession{Client: "host", InstanceID: "instance", SessionID: "SHELL1", Kind: string(model.KindShell), Status: string(model.StatusRunning)}
	state := &tuiState{ownerName: "desk", lifecycleBusy: true, hostSync: map[string]ducklord.SessionUpdate{"host": {State: "live"}}}
	for _, action := range state.sessionActions(session) {
		if action.Operation == "" {
			continue
		}
		if action.Enabled || action.DisabledReason != "another lifecycle operation is pending" {
			t.Fatalf("pending lifecycle action=%#v", action)
		}
	}
}

func TestActionModalIsCenteredColoredAndSelectsStableAction(t *testing.T) {
	target := ducklord.RemoteSession{Client: "host", InstanceID: "instance", SessionID: "SHELL1", Name: "中文 shell", Kind: string(model.KindShell), Status: string(model.StatusRunning), RuntimeGeneration: 4}
	state := &tuiState{ownerName: "desk", actionMenu: true, actionTarget: target, actionIndex: 5,
		hostSync: map[string]ducklord.SessionUpdate{"host": {State: "live"}}, sessions: []ducklord.RemoteSession{target}}
	var out bytes.Buffer
	state.renderActionModal(&out, 80, 24)
	got := out.String()
	for _, want := range []string{"Session actions", "中文 shell", "Destroy shell and logs", modalDanger, "SHELL1", "╭", "╯"} {
		if !strings.Contains(got, want) {
			t.Fatalf("action modal missing %q in %q", want, got)
		}
	}
	if action := state.handleActionMenuInput([]byte("\r")); action != "lifecycle-action" || state.actionOperation != protocol.SessionLifecycleDestroy || state.actionMode != protocol.SessionLifecycleImmediate {
		t.Fatalf("chosen action=%q operation=%q mode=%q", action, state.actionOperation, state.actionMode)
	}
	if !state.selectActionTarget() || state.selected != 0 {
		t.Fatal("captured action target could not be rebound")
	}
	state.actionMenu, state.actionTarget, state.actionIndex = true, target, 0
	if action := state.handleActionMenuInput([]byte("x")); action != "lifecycle-action" || state.actionOperation != protocol.SessionLifecycleDestroy {
		t.Fatalf("stable destroy accelerator action=%q operation=%q", action, state.actionOperation)
	}
}

func TestActionTargetFailsClosedAfterRevisionOrHostChange(t *testing.T) {
	target := ducklord.RemoteSession{Client: "host", InstanceID: "instance", SessionID: "AGENT1", Kind: string(model.KindAgent), Status: string(model.StatusRunning),
		WriterKind: string(model.OwnerTerminal), WriterID: "desk", OwnershipEpoch: 2, RuntimeGeneration: 3}
	state := &tuiState{ownerName: "desk", actionTarget: target, sessions: []ducklord.RemoteSession{target}, hostSync: map[string]ducklord.SessionUpdate{"host": {State: "live"}}}
	if !state.selectActionTarget() {
		t.Fatal("unchanged action target rejected")
	}
	state.sessions[0].RuntimeGeneration++
	if state.selectActionTarget() {
		t.Fatal("action target followed new runtime generation")
	}
	state.sessions[0] = target
	state.hostSync["host"] = ducklord.SessionUpdate{State: "reconnecting"}
	if state.selectActionTarget() {
		t.Fatal("action target remained valid while host reconnects")
	}
	state.lifecycleTarget = target
	if state.lifecycleTargetIsCurrent() {
		t.Fatal("lifecycle confirmation remained valid while host reconnects")
	}
}

func TestTUIRenderShowsStoppedRetainedOutputWindow(t *testing.T) {
	until := time.Date(2030, time.January, 2, 15, 4, 0, 0, time.Local).UnixMilli()
	state := &tuiState{ownerName: "desk", sessions: []ducklord.RemoteSession{{Client: "host", Name: "agent", SessionID: "ABC123",
		Kind: string(model.KindAgent), Status: string(model.StatusStopped), RetainedOutputBytes: 1536, RetainedOutputUntilMS: until}}}
	var out bytes.Buffer
	state.render(&out)
	for _, want := range []string{"💀", "stopped", "retained:1.5 KiB", "until Jan 02 15:04"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("retention header missing %q in %q", want, out.String())
		}
	}
}

func TestSessionNeedsAttentionForStoppedOrUnresponsiveRuntime(t *testing.T) {
	for _, session := range []ducklord.RemoteSession{
		{Status: string(model.StatusStopped)},
		{Status: string(model.StatusRunning), AdapterState: string(model.AdapterUnhealthy)},
	} {
		if !sessionNeedsAttention(session) {
			t.Fatalf("session should show death indicator: %+v", session)
		}
	}
	succeeded := true
	if sessionNeedsAttention(ducklord.RemoteSession{Status: string(model.StatusStopped), ExitSuccess: &succeeded}) {
		t.Fatal("successful stopped runtime was marked as dead")
	}
	if sessionNeedsAttention(ducklord.RemoteSession{Status: string(model.StatusRunning), AdapterState: string(model.AdapterRecovering)}) {
		t.Fatal("recovering session was incorrectly marked dead")
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

func TestTUIHelpUsesConfiguredBindingsAndCategories(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{Shortcuts: map[string]string{"help": "!"}}}
	if action := state.handleInput([]byte("!")); action != "help" {
		t.Fatalf("custom help action=%q", action)
	}
	state.helpMode = true
	var out bytes.Buffer
	state.renderHelpModal(&out, 100, 40)
	for _, want := range []string{"Keyboard shortcuts", "SESSION LIST & GROUPS", "SESSION", "HOST", "PTY PANEL", "MOUSE", "MODALS", "!"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q: %q", want, out.String())
		}
	}
	if action := state.handleInput([]byte("!")); action != "help" {
		t.Fatalf("configured help toggle action=%q", action)
	}
	state.helpMode = !state.helpMode
	if state.helpMode {
		t.Fatal("configured help key did not toggle pinned help")
	}
}

func TestPinnedHelpDoesNotCaptureEscapeOrNavigation(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{}, helpMode: true, sessions: []ducklord.RemoteSession{{Name: "one"}, {Name: "two"}}}
	if action := state.handleInput([]byte("\x1b")); action != "" || !state.helpMode {
		t.Fatalf("escape action=%q help=%v", action, state.helpMode)
	}
	if action := state.handleInput([]byte("j")); action != "select" || state.selected != 1 || !state.helpMode {
		t.Fatalf("navigation action=%q selected=%d help=%v", action, state.selected, state.helpMode)
	}
}

func TestParseSGRMouseWheel(t *testing.T) {
	button, x, y, ok := parseSGRMouse("\x1b[<64;80;12M")
	if !ok || button != 64 || x != 80 || y != 12 {
		t.Fatalf("parsed=(%d,%d,%d,%v)", button, x, y, ok)
	}
	if _, _, _, ok := parseSGRMouse("\x1b[<64;999999;12M"); ok {
		t.Fatal("accepted oversized coordinate")
	}
}

func TestShortcutEditorStagesSaveUntilRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	state := &tuiState{cfg: &ducklord.Config{}, cfgPath: path}
	state.beginShortcutSettings()
	actions := shortcutActions()
	for index, action := range actions {
		if action == "help" {
			state.shortcutIndex = index
			break
		}
	}
	state.handleShortcutInput([]byte("\r"))
	state.shortcutLine = "!"
	state.handleShortcutInput([]byte("\r"))
	if state.shortcutStep != "restart" || state.cfg.Shortcut("help") != "?" {
		t.Fatalf("step=%q runtime help=%q", state.shortcutStep, state.cfg.Shortcut("help"))
	}
	loaded, err := ducklord.LoadConfig(path)
	if err != nil || loaded.Shortcut("help") != "!" {
		t.Fatalf("loaded help=%q err=%v", loaded.Shortcut("help"), err)
	}
}

func TestTUIHostMenuTargetsSelectedHostGroup(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host-a", Host: "a"}, {Name: "host-b", Host: "b"}}}, activityState: ducklord.NewActivityState(), selectedGroupID: "host-b"}
	state.activity().Organization.Mode = ducklord.OrganizationHost
	state.beginHostMenu()
	if !state.hostMenuMode || state.hostMenuTarget != "host-b" {
		t.Fatalf("host menu mode=%v target=%q", state.hostMenuMode, state.hostMenuTarget)
	}
	state.hostMenuIndex = 1
	if action := state.handleHostMenuInput([]byte("\r")); action != "" || state.hostMenuStep != "hosts" {
		t.Fatalf("host menu selection action=%q step=%q", action, state.hostMenuStep)
	}
	if action := state.handleHostMenuInput([]byte("\r")); action != "host-disconnect-selected" {
		t.Fatalf("host menu action=%q", action)
	}
	var out bytes.Buffer
	state.renderHostModal(&out, 100, 30)
	if !strings.Contains(out.String(), "Disconnect hosts") || !strings.Contains(out.String(), "[✓] host-b") {
		t.Fatalf("host modal=%q", out.String())
	}
}

func TestTUIHostConnectDisconnectMenuSupportsMultiSelect(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "a"}, {Name: "b"}}}, disconnectedHosts: map[string]bool{}, hostMenuTarget: "a", hostMenuSelected: map[string]bool{}, hostMenuMode: true, hostMenuStep: "actions", hostMenuIndex: 1}
	state.handleHostMenuInput([]byte("\r"))
	state.handleHostMenuInput([]byte("\x1b[B"))
	state.handleHostMenuInput([]byte(" "))
	if action := state.handleHostMenuInput([]byte("\r")); action != "host-disconnect-selected" {
		t.Fatalf("action=%q", action)
	}
	if targets := state.selectedHostMenuTargets(); len(targets) != 2 || targets[0] != "a" || targets[1] != "b" {
		t.Fatalf("targets=%v", targets)
	}
}

type countingSessionsRunner struct {
	fakeRunner
	calls int
}

func (r *countingSessionsRunner) Sessions(ctx context.Context, client ducklord.Client, tail int) ([]ducklord.RemoteSession, error) {
	r.calls++
	return r.fakeRunner.Sessions(ctx, client, tail)
}

func TestTUIDisconnectedHostIsExcludedFromPollingRefresh(t *testing.T) {
	runner := &countingSessionsRunner{fakeRunner: fakeRunner{sessions: []ducklord.RemoteSession{{Client: "host", Name: "late"}}}}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: runner, disconnectedHosts: map[string]bool{"host": true}, hashes: map[string]string{}, sessions: []ducklord.RemoteSession{{Client: "host", Name: "retained"}}}
	state.refreshSessions(context.Background())
	if runner.calls != 0 || len(state.sessions) != 1 || state.sessions[0].Name != "retained" {
		t.Fatalf("disconnected refresh calls=%d sessions=%+v", runner.calls, state.sessions)
	}
}

func TestTUIHostModeHidesRepeatedHostAndGraysDisconnectedRows(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host-a"}}}, activityState: ducklord.NewActivityState(), disconnectedHosts: map[string]bool{"host-a": true}, sessions: []ducklord.RemoteSession{{Client: "host-a", Name: "alpha", Status: "running"}}}
	state.activity().Organization.Mode = ducklord.OrganizationHost
	state.applyOrganizationOrder()
	var out bytes.Buffer
	state.render(&out)
	// The host appears once in the group header and once in the PTY header, but
	// is intentionally omitted from the nested session row.
	if strings.Count(out.String(), "host-a") != 2 || !strings.Contains(out.String(), disconnectedRowColor) {
		t.Fatalf("rendered host mode=%q", out.String())
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

type mutableSessionsRunner struct {
	fakeRunner
	current []ducklord.RemoteSession
}

func (r *mutableSessionsRunner) Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error) {
	return append([]ducklord.RemoteSession(nil), r.current...), nil
}

func TestTUIRefreshDistinguishesDuplicateHandlesByIdentity(t *testing.T) {
	runner := &mutableSessionsRunner{current: []ducklord.RemoteSession{
		{Client: "host-a", InstanceID: "instance", SessionID: "DEF456", Name: "same", TailHash: "b1"},
		{Client: "host-a", InstanceID: "instance", SessionID: "ABC123", Name: "same", TailHash: "a1"},
	}}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host-a"}}}, runner: runner, hashes: map[string]string{}, activityState: ducklord.NewActivityState()}
	state.refreshSessions(context.Background())
	if state.sessions[0].SessionID != "ABC123" || state.sessions[1].SessionID != "DEF456" {
		t.Fatalf("initial stable order = %+v", state.sessions)
	}
	runner.current = []ducklord.RemoteSession{
		{Client: "host-a", InstanceID: "instance", SessionID: "ABC123", Name: "same", TailHash: "a1"},
		{Client: "host-a", InstanceID: "instance", SessionID: "DEF456", Name: "same", TailHash: "b2"},
	}
	state.refreshSessions(context.Background())
	if state.sessions[0].Updated || !state.sessions[1].Updated {
		t.Fatalf("duplicate handle updated markers = %+v", state.sessions)
	}
}

func TestTUIOrganizationModeAndManualOrderPersist(t *testing.T) {
	instance := string(model.NewInstanceID())
	statePath := filepath.Join(t.TempDir(), "private", "state.json")
	state := &tuiState{
		activityState: ducklord.NewActivityState(),
		activityStore: ducklord.ActivityStateStore{Path: statePath},
		sessions: []ducklord.RemoteSession{
			{Client: "host-b", InstanceID: instance, SessionID: "BBB222", Name: "second", Kind: string(model.KindAgent)},
			{Client: "host-a", InstanceID: instance, SessionID: "AAA111", Name: "first", Kind: string(model.KindShell)},
		},
	}
	if !state.applyOrganizationOrder() {
		t.Fatal("new session identities were not appended to local order")
	}
	state.selected = 1
	state.moveSelectedSession(-1)
	if state.currentSession().SessionID != "AAA111" || state.sessions[0].SessionID != "AAA111" {
		t.Fatalf("moved session/order = %+v selected=%+v", state.sessions, state.currentSession())
	}
	state.cycleOrganizationMode()
	if state.organizationMode() != ducklord.OrganizationHost {
		t.Fatalf("mode = %q", state.organizationMode())
	}
	loaded, err := (ducklord.ActivityStateStore{Path: statePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Organization.Mode != ducklord.OrganizationHost || len(loaded.Organization.SessionOrder) != 2 || loaded.Organization.SessionOrder[0].SessionID != "AAA111" {
		t.Fatalf("persisted organization = %+v", loaded.Organization)
	}
	state.cycleOrganizationMode()
	if state.organizationMode() != ducklord.OrganizationType || state.organizationGroupID(state.sessions[0]) != "shell" {
		t.Fatalf("type projection mode=%q sessions=%+v", state.organizationMode(), state.sessions)
	}
}

func TestTUICustomKeyboardReorderCrossesGroup(t *testing.T) {
	instance := string(model.NewInstanceID())
	first := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "first"}
	second := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "BBB222", Name: "second"}
	firstID, _ := ducklord.IdentityFromSession(first)
	secondID, _ := ducklord.IdentityFromSession(second)
	groupA, groupB := uuid.NewString(), uuid.NewString()
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}, sessions: []ducklord.RemoteSession{first, second}}
	state.activity().Organization.Groups = []ducklord.CustomGroup{{ID: groupA, Name: "A"}, {ID: groupB, Name: "B"}}
	state.activity().Organization.Membership[firstID] = groupA
	state.activity().Organization.Membership[secondID] = groupB
	state.applyOrganizationOrder()
	state.selected = 1
	state.moveSelectedSession(-1)
	if state.activity().Organization.Membership[secondID] != groupA || state.sessions[0].SessionID != second.SessionID {
		t.Fatalf("membership=%+v sessions=%+v", state.activity().Organization.Membership, state.sessions)
	}
}

func TestTUICustomKeyboardReorderMovesIntoEmptyGroup(t *testing.T) {
	instance := string(model.NewInstanceID())
	session := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "first"}
	identity, _ := ducklord.IdentityFromSession(session)
	groupA, emptyGroup := uuid.NewString(), uuid.NewString()
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}, sessions: []ducklord.RemoteSession{session}}
	state.activity().Organization.Groups = []ducklord.CustomGroup{{ID: groupA, Name: "A"}, {ID: emptyGroup, Name: "Empty"}}
	state.activity().Organization.GroupOrders[ducklord.OrganizationCustom] = []string{groupA, emptyGroup, ducklord.UngroupedGroupID}
	state.activity().Organization.Membership[identity] = groupA
	state.applyOrganizationOrder()
	state.moveSelectedSession(1)
	if state.activity().Organization.Membership[identity] != emptyGroup {
		t.Fatalf("membership=%+v error=%q", state.activity().Organization.Membership, state.outputErr)
	}
}

func TestTUIUnreadSessionTemporarilyPromotesWithinGroup(t *testing.T) {
	instance := string(model.NewInstanceID())
	state := &tuiState{cfg: &ducklord.Config{}, activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "first"},
		{Client: "host", InstanceID: instance, SessionID: "BBB222", Name: "second", Unread: true},
	}}
	state.applyOrganizationOrder()
	if state.sessions[0].SessionID != "BBB222" {
		t.Fatalf("unread session was not promoted: %+v", state.sessions)
	}
	state.sessions[0].Unread = false
	state.applyOrganizationOrder()
	if state.sessions[0].SessionID != "AAA111" {
		t.Fatalf("cleared session did not return to saved order: %+v", state.sessions)
	}
	disabled := false
	state.cfg.PromoteUnreadSessions = &disabled
	state.sessions[1].Unread = true
	state.applyOrganizationOrder()
	if state.sessions[0].SessionID != "AAA111" {
		t.Fatalf("disabled promotion changed order: %+v", state.sessions)
	}
}

func TestTUIListSelectsAndCollapsesStableGroupRows(t *testing.T) {
	instanceA, instanceB := string(model.NewInstanceID()), string(model.NewInstanceID())
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")},
		sessions: []ducklord.RemoteSession{
			{Client: "host-a", InstanceID: instanceA, SessionID: "AAA111", Name: "alpha"},
			{Client: "host-b", InstanceID: instanceB, SessionID: "BBB222", Name: "beta", Unread: true},
		}, selected: 0, activeAttachKey: "host-a/" + instanceA + "/AAA111"}
	state.activity().Organization.Mode = ducklord.OrganizationHost
	state.applyOrganizationOrder()
	if rows := state.sessionListRows(); len(rows) != 4 || !rows[0].isGroup || rows[1].sessionIndex != 0 || !rows[2].isGroup || rows[3].sessionIndex != 1 {
		t.Fatalf("visible row projection=%+v", rows)
	}
	if action := state.handleInput([]byte("j")); action != "group-select" || state.selectedGroupID != "host-b" {
		t.Fatalf("group navigation action=%q selected=%q", action, state.selectedGroupID)
	}
	for _, input := range []string{"m", "n", "y", "Y", "E", "R", "X", "\x0b", "\n"} {
		if action := state.handleInput([]byte(input)); action != "group-select" {
			t.Fatalf("group header accepted session action %q as %q", input, action)
		}
	}
	if action := state.handleInput([]byte("g")); action != "groups" {
		t.Fatalf("group management action=%q", action)
	}
	if action := state.handleInput([]byte("d")); action != "remove-client" {
		t.Fatalf("host-group remove action=%q", action)
	}
	var rendered bytes.Buffer
	state.render(&rendered)
	if strings.Count(rendered.String(), ">") != 1 {
		t.Fatalf("group selection rendered more than one cursor: %q", rendered.String())
	}
	if action := state.handleInput([]byte("\x1b[D")); action != "group-toggle" || !state.groupCollapsed("host-b") {
		t.Fatalf("collapse action=%q collapsed=%v", action, state.groupCollapsed("host-b"))
	}
	state.handleInput([]byte("\x1b[D"))
	if !state.groupCollapsed("host-b") {
		t.Fatal("repeated Left expanded the group")
	}
	state.handleInput([]byte("\x1b[C"))
	if state.groupCollapsed("host-b") {
		t.Fatal("Right did not expand the group")
	}
	state.handleInput([]byte("\x1b[C"))
	if state.groupCollapsed("host-b") {
		t.Fatal("repeated Right collapsed the group")
	}
	state.handleInput([]byte("\r"))
	if rows := state.sessionListRows(); len(rows) != 3 || !rows[2].isGroup || !state.groupHasUnread("host-b") {
		t.Fatalf("collapsed rows/unread=%+v unread=%v", rows, state.groupHasUnread("host-b"))
	}
	if state.activeAttachKey == "" {
		t.Fatal("collapsing the list detached the active PTY")
	}
	loaded, err := (ducklord.ActivityStateStore{Path: state.activityStore.Path}).Load()
	if err != nil || len(loaded.Organization.Collapsed[ducklord.OrganizationHost]) != 1 || loaded.Organization.Collapsed[ducklord.OrganizationHost][0] != "host-b" {
		t.Fatalf("persisted collapse=%+v err=%v", loaded.Organization.Collapsed, err)
	}
}

func TestTUIHostRowsUseStableSafePalette(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host-a"}, {Name: "host-b"}}}}
	if state.hostRowColor("host-a") == state.hostRowColor("host-b") {
		t.Fatal("fixture hosts unexpectedly share a palette slot")
	}
	if strings.Contains(state.hostRowColor("host-a\x1b]52;c;bad\a"), "52;") {
		t.Fatal("host input escaped the fixed ANSI palette")
	}
}

func TestTUIMouseDragMovesExactSessionToCustomGroup(t *testing.T) {
	instance := string(model.NewInstanceID())
	groupID := uuid.NewString()
	identity := ducklord.SessionIdentity{InstanceID: instance, SessionID: "AAA111"}
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")},
		sessions: []ducklord.RemoteSession{{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "same"}}}
	state.activity().Organization.Groups = []ducklord.CustomGroup{{ID: groupID, Name: "target"}}
	state.activity().Organization.GroupOrders[ducklord.OrganizationCustom] = []string{ducklord.UngroupedGroupID, groupID}
	state.applyOrganizationOrder()
	if action := state.handleInput([]byte("\x1b[<0;2;6M")); action != "select" || state.dragSession != identity {
		t.Fatalf("drag start action=%q identity=%+v", action, state.dragSession)
	}
	if action := state.handleInput([]byte("\x1b[<32;2;7M")); action != "group-select" || state.dragTargetGroup != groupID {
		t.Fatalf("drag hover action=%q target=%q", action, state.dragTargetGroup)
	}
	if action := state.handleInput([]byte("\x1b[<0;2;7m")); action != "group-drop" || state.activity().Organization.Membership[identity] != groupID {
		t.Fatalf("drop action=%q membership=%+v", action, state.activity().Organization.Membership)
	}
}

func TestTUIMouseDragReordersSessionsInsideProjectedGroup(t *testing.T) {
	instance := string(model.NewInstanceID())
	first := ducklord.SessionIdentity{InstanceID: instance, SessionID: "AAA111"}
	second := ducklord.SessionIdentity{InstanceID: instance, SessionID: "BBB222"}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")},
		sessions: []ducklord.RemoteSession{{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "first"}, {Client: "host", InstanceID: instance, SessionID: "BBB222", Name: "second"}}}
	state.activity().Organization.Mode = ducklord.OrganizationHost
	state.applyOrganizationOrder()
	// Rows are group at 5, first at 6, second at 7. Drag second before first.
	if action := state.handleInput([]byte("\x1b[<0;2;7M")); action != "select" || state.dragSession != second {
		t.Fatalf("drag start action=%q identity=%+v", action, state.dragSession)
	}
	state.handleInput([]byte("\x1b[<32;2;6M"))
	if action := state.handleInput([]byte("\x1b[<0;2;6m")); action != "group-drop" {
		t.Fatalf("drop action=%q", action)
	}
	if order := state.activity().Organization.SessionOrder; len(order) != 2 || order[0] != second || order[1] != first {
		t.Fatalf("session order=%+v", order)
	}
}

func TestMouseCursorShapeIsBoundedAndRestorable(t *testing.T) {
	if got := mouseCursorShape("grabbing"); got != "\x1b]22;grabbing\x1b\\" {
		t.Fatalf("grabbing cursor sequence=%q", got)
	}
	if got := mouseCursorShape("untrusted;payload"); got != "\x1b]22;default\x1b\\" {
		t.Fatalf("cursor reset sequence=%q", got)
	}
}

func TestTUIOrganizationPersistenceFailureKeepsLocalChangeAndWarns(t *testing.T) {
	instance := string(model.NewInstanceID())
	badPath := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(badPath, 0700); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: badPath}, sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: instance, SessionID: "AAA111", Kind: string(model.KindShell)},
		{Client: "host", InstanceID: instance, SessionID: "BBB222", Kind: string(model.KindShell)},
	}}
	state.applyOrganizationOrder()
	state.cycleOrganizationMode()
	if state.organizationMode() != ducklord.OrganizationHost || !strings.Contains(state.outputErr, "changed locally but was not saved") {
		t.Fatalf("mode=%q error=%q", state.organizationMode(), state.outputErr)
	}
	state.activityStore = ducklord.ActivityStateStore{Path: badPath}
	state.activityState.Organization.Mode = ducklord.OrganizationCustom
	state.applyOrganizationOrder()
	state.selected = 1
	state.outputErr = ""
	state.moveSelectedSession(-1)
	if state.sessions[0].SessionID != "AAA111" || !strings.Contains(state.outputErr, "was not changed") {
		t.Fatalf("sessions=%+v error=%q", state.sessions, state.outputErr)
	}
}

func TestTUICustomGroupModalCRUDUsesStableIdentities(t *testing.T) {
	instanceA, instanceB := string(model.NewInstanceID()), string(model.NewInstanceID())
	state := &tuiState{
		activityState: ducklord.NewActivityState(),
		activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "private", "state.json")},
		sessions: []ducklord.RemoteSession{
			{Client: "a", InstanceID: instanceA, SessionID: "AAA111", Name: "same"},
			{Client: "b", InstanceID: instanceB, SessionID: "BBB222", Name: "same"},
		},
	}
	state.applyOrganizationOrder()
	create := func(name string) {
		state.beginGroupMenu()
		state.handleGroupMenuInput([]byte("\r"))
		state.handleGroupMenuInput([]byte(name))
		state.handleGroupMenuInput([]byte("\r"))
		if state.groupMenu {
			t.Fatalf("create %q remained open: %s", name, state.groupMenuErr)
		}
	}
	create("正式環境")
	create("正式環境")
	groups := state.activity().Organization.Groups
	if len(groups) != 2 || groups[0].ID == groups[1].ID || groups[0].Name != groups[1].Name {
		t.Fatalf("duplicate-name groups = %+v", groups)
	}
	firstID, secondID := groups[0].ID, groups[1].ID
	identityA := ducklord.SessionIdentity{InstanceID: instanceA, SessionID: "AAA111"}

	state.selected = 0
	state.beginGroupMenu()
	state.groupMenuAction, state.groupMenuStep, state.groupMenuTarget = "move", "group", firstID
	state.commitGroupOperation()
	if state.activity().Organization.Membership[identityA] != firstID {
		t.Fatalf("membership = %+v", state.activity().Organization.Membership)
	}

	state.beginGroupMenu()
	state.groupMenuAction, state.groupMenuStep, state.groupMenuTarget, state.groupMenuLine = "rename", "name", secondID, "已改名"
	state.commitGroupName()
	if state.activity().Organization.Groups[1].ID != secondID || state.activity().Organization.Groups[1].Name != "已改名" {
		t.Fatalf("rename changed identity: %+v", state.activity().Organization.Groups)
	}

	state.beginGroupMenu()
	state.groupMenuAction, state.groupMenuStep, state.groupMenuTarget = "group-up", "group", secondID
	state.commitGroupOperation()
	if state.activity().Organization.GroupOrders[ducklord.OrganizationCustom][1] != secondID {
		t.Fatalf("group order = %+v first=%s second=%s", state.activity().Organization.GroupOrders, firstID, secondID)
	}

	state.beginGroupMenu()
	state.groupMenuAction, state.groupMenuStep, state.groupMenuTarget = "delete", "group", firstID
	state.commitGroupOperation()
	if len(state.activity().Organization.Groups) != 1 || state.activity().Organization.Membership[identityA] != "" {
		t.Fatalf("delete state groups=%+v membership=%+v", state.activity().Organization.Groups, state.activity().Organization.Membership)
	}
}

func TestTUICustomGroupMoveRejectsStaleCapturedSession(t *testing.T) {
	instance := string(model.NewInstanceID())
	groupID := uuid.NewString()
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}, sessions: []ducklord.RemoteSession{
		{Client: "a", InstanceID: instance, SessionID: "AAA111"},
	}}
	state.activity().Organization.Groups = []ducklord.CustomGroup{{ID: groupID, Name: "target"}}
	state.activity().Organization.GroupOrders[ducklord.OrganizationCustom] = []string{ducklord.UngroupedGroupID, groupID}
	state.beginGroupMenu()
	state.sessions = []ducklord.RemoteSession{{Client: "a", InstanceID: instance, SessionID: "BBB222"}}
	state.groupMenuAction, state.groupMenuStep, state.groupMenuTarget = "move", "group", groupID
	state.commitGroupOperation()
	if !state.groupMenu || !strings.Contains(state.groupMenuErr, "no longer exists") || len(state.activity().Organization.Membership) != 0 {
		t.Fatalf("stale move modal=%v err=%q membership=%+v", state.groupMenu, state.groupMenuErr, state.activity().Organization.Membership)
	}
}

func TestTUICustomGroupModalIsCenteredAndColored(t *testing.T) {
	instance := string(model.NewInstanceID())
	state := &tuiState{activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{{Client: "host", InstanceID: instance, SessionID: "AAA111"}}}
	state.beginGroupMenu()
	var out strings.Builder
	state.renderGroupModal(&out, 80, 24)
	raw := out.String()
	if !strings.Contains(raw, "\033[7;5H") || !strings.Contains(raw, modalBorder) || !strings.Contains(raw, modalSelected) || !strings.Contains(raw, "Custom groups") {
		t.Fatalf("group modal geometry/style = %q", raw)
	}
}

func TestTUICustomGroupNameAcceptsQAndSaveFailureRollsBack(t *testing.T) {
	instance := string(model.NewInstanceID())
	badPath := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(badPath, 0700); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: badPath}, sessions: []ducklord.RemoteSession{{InstanceID: instance, SessionID: "AAA111"}}}
	state.beginGroupMenu()
	state.handleGroupMenuInput([]byte("\r"))
	for _, input := range []byte("qa") {
		state.handleGroupMenuInput([]byte{input})
	}
	state.handleGroupMenuInput([]byte("\r"))
	if !state.groupMenu || !strings.Contains(state.groupMenuErr, "not saved") || len(state.activity().Organization.Groups) != 0 {
		t.Fatalf("modal=%v line=%q err=%q groups=%+v", state.groupMenu, state.groupMenuLine, state.groupMenuErr, state.activity().Organization.Groups)
	}
	if order := state.activity().Organization.GroupOrders[ducklord.OrganizationCustom]; len(order) != 1 || order[0] != ducklord.UngroupedGroupID {
		t.Fatalf("default custom group order = %+v", order)
	}
}

func TestTUICustomGroupLongMenuKeepsSelectionVisible(t *testing.T) {
	state := &tuiState{activityState: ducklord.NewActivityState(), groupMenu: true, groupMenuStep: "group", groupMenuAction: "rename", groupMenuIndex: 11}
	for index := 0; index < 12; index++ {
		state.activity().Organization.Groups = append(state.activity().Organization.Groups, ducklord.CustomGroup{ID: uuid.NewString(), Name: fmt.Sprintf("group-%02d", index)})
	}
	var out strings.Builder
	state.renderGroupModal(&out, 42, 10)
	if !strings.Contains(out.String(), "group-11") || !strings.Contains(out.String(), modalSelected) {
		t.Fatalf("selected group is not visible: %q", out.String())
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
		activityStore: ducklord.ActivityStateStore{Path: statePath}, outputForKey: "host-a/" + instance + "/ABC123", outputFresh: true,
		sessions: []ducklord.RemoteSession{
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "ABC123", Name: "active", RuntimeGeneration: 1},
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "DEF456", Name: "background", RuntimeGeneration: 1},
		}}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: instance, Revision: 2, Generation: 1, State: "live", ChangedSessionID: "DEF456",
		Sessions: []ducklord.RemoteSession{
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "ABC123", Name: "active", RuntimeGeneration: 1},
			{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "DEF456", Name: "background",
				RuntimeGeneration: 1, ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTerminalAttention: 1}},
		}})
	if state.sessions[0].SessionID != "DEF456" || !state.sessions[0].Unread || state.sessions[1].Unread || !state.groupHasUnread(ducklord.UngroupedGroupID) {
		t.Fatalf("unread projection=%+v", state.sessions)
	}
	state.selected = 0
	state.outputForKey = "host-a/" + instance + "/DEF456"
	state.outputFresh = false // a stale detach snapshot is not proof of seeing it
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: instance, Revision: 3, Generation: 1, State: "live",
		Sessions: append([]ducklord.RemoteSession(nil), state.sessions...)})
	if !state.currentSession().Unread {
		t.Fatal("selection or stale output cleared unread")
	}
	state.focused = true
	state.pendingAttachKey = "host-a/" + instance + "/DEF456"
	state.applyAttachOutput("fresh\n", 1, 6)
	if state.currentSession().Unread || state.groupHasUnread(ducklord.UngroupedGroupID) {
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

func TestTUICopyModeKeepsBackgroundActivityUnreadUntilLatestFrameIsRevealed(t *testing.T) {
	instance := string(model.NewInstanceID())
	session := ducklord.RemoteSession{
		Client: "host-a", InstanceID: instance, SessionID: "ABC123", Name: "agent", Status: "running",
		RuntimeGeneration: 1, Unread: true,
		ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1},
	}
	state := &tuiState{
		activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")},
		sessions: []ducklord.RemoteSession{session}, focused: true, copyMode: true,
		outputForKey: sessionKey(session), activeAttachKey: sessionKey(session),
		terminal: ducklord.NewTerminal(24, 80, ducklord.DefaultTerminalScrollback), terminalGeneration: 1,
	}
	state.applyAttachOutput("completed while copying\n", 1, 24)
	if !state.currentSession().Unread || state.activity().Sessions[instance+"/ABC123"].Seen[model.NotificationTaskCompleted] != 0 {
		t.Fatalf("hidden output was marked seen: session=%+v activity=%+v", state.currentSession(), state.activity())
	}
	state.copyMode = false
	if !state.sessionFreshlyDisplayed(state.currentSession()) {
		t.Fatal("latest frame was not considered visible after leaving copy mode")
	}
	state.markActivitySeen(state.currentSession())
	if state.currentSession().Unread || state.activity().Sessions[instance+"/ABC123"].Seen[model.NotificationTaskCompleted] != 1 {
		t.Fatalf("revealed output stayed unread: session=%+v activity=%+v", state.currentSession(), state.activity())
	}
}

func TestTUIShowsInteractiveAgentCompletionWhileAnotherSessionIsActive(t *testing.T) {
	instance := string(model.NewInstanceID())
	active := ducklord.RemoteSession{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "ABC123", Name: "active"}
	background := ducklord.RemoteSession{Client: "host-a", Group: "work", InstanceID: instance, SessionID: "DEF456", Name: "agent-demo",
		ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 0}}
	state := &tuiState{hostSync: make(map[string]ducklord.SessionUpdate), activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{active, background},
		activeAttachKey: sessionKey(active), activeAttachFresh: true, outputForKey: sessionKey(active), outputFresh: true}
	completed := background
	completed.ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host-a", InstanceID: instance, Revision: 2, Generation: 1, State: "live", ChangedSessionID: "DEF456",
		Sessions: []ducklord.RemoteSession{active, completed}})
	if state.outputErr != "agent-demo: agent turn completed" {
		t.Fatalf("completion notice=%q", state.outputErr)
	}
	if state.sessions[0].SessionID != "DEF456" || !state.sessions[0].Unread || state.sessions[1].Unread || !state.groupHasUnread(ducklord.UngroupedGroupID) {
		t.Fatalf("completion unread projection=%+v", state.sessions)
	}
}

func TestTUISelectionDoesNotMoveActiveSessionOrClearUnread(t *testing.T) {
	instance := string(model.NewInstanceID())
	state := &tuiState{activityState: ducklord.NewActivityState(), activeAttachKey: "host-a/" + instance + "/ABC123", activeAttachFresh: true,
		outputForKey: "host-a/" + instance + "/ABC123", outputFresh: true, sessions: []ducklord.RemoteSession{
			{Client: "host-a", InstanceID: instance, SessionID: "ABC123", Name: "active"},
			{Client: "host-a", InstanceID: instance, SessionID: "DEF456", Name: "background", Unread: true,
				ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTerminalAttention: 1}},
		}}
	if action := state.handleInput([]byte("j")); action != "select" {
		t.Fatalf("action=%q", action)
	}
	if state.activeAttachKey != "host-a/"+instance+"/ABC123" || !state.currentSession().Unread {
		t.Fatalf("selection moved active or cleared unread: active=%q current=%+v", state.activeAttachKey, state.currentSession())
	}
}

func TestSessionKeySeparatesClientAliases(t *testing.T) {
	base := ducklord.RemoteSession{InstanceID: "same-instance", SessionID: "ABC123"}
	first, second := base, base
	first.Client, second.Client = "primary", "alias"
	if sessionKey(first) == sessionKey(second) {
		t.Fatalf("client aliases collided: %q", sessionKey(first))
	}
}

func TestSaveCurrentSnapshotRejectsMismatchedOutputIdentity(t *testing.T) {
	dir := t.TempDir()
	instance := string(model.NewInstanceID())
	state := &tuiState{
		snapshotStore: ducklord.SnapshotStore{Root: dir},
		sessions:      []ducklord.RemoteSession{{Client: "host-b", InstanceID: instance, SessionID: "BBB222"}},
		outputForKey:  "host-a/" + instance + "/AAA111",
		outputText:    "secret-from-a",
	}
	state.saveCurrentSnapshot()
	if _, err := state.snapshotStore.Load(instance, "BBB222"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched output was persisted: %v", err)
	}
}

func TestTUIDetachedSelectionImmediatelyLoadsSelectedSessionPreview(t *testing.T) {
	instance := string(model.NewInstanceID())
	runner := &recordingRunner{readText: "session-b-screen\n"}
	state := &tuiState{
		cfg:               &ducklord.Config{Clients: []ducklord.Client{{Name: "host-a", Host: "host-a"}}},
		runner:            runner,
		activityState:     ducklord.NewActivityState(),
		activeAttachKey:   "host-a/" + instance + "/ABC123",
		activeAttachFresh: true,
		pendingAttachKey:  "host-a/" + instance + "/ABC123",
		outputForKey:      "host-a/" + instance + "/ABC123",
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
	if state.applyPreviewOutput(previewOutputEvent{id: 1, key: "host/" + instance + "/AAA111", generation: 3, text: "late-a"}, 2) {
		t.Fatal("late preview result was applied")
	}
	if state.outputText != "current-b" {
		t.Fatalf("late preview overwrote selection: %q", state.outputText)
	}
	result := previewOutputEvent{id: 2, key: "host/" + instance + "/BBB222", generation: 7, text: "fresh-b"}
	if !state.applyPreviewOutput(result, 2) || !strings.Contains(state.outputText, "fresh-b") {
		t.Fatalf("current preview was not applied: %q", state.outputText)
	}
	state.focused = true
	if state.applyPreviewOutput(previewOutputEvent{id: 3, key: "host/" + instance + "/BBB222", generation: 7, text: "stale-preview"}, 3) {
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
	state := &tuiState{focused: true, pendingAttachKey: "host-a/" + instance + "/ABC123", hostSync: make(map[string]ducklord.SessionUpdate), activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{
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
	state.renderNotificationModal(&rendered, 100, 30)
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
		t.Fatalf("escape on first page action=%q", action)
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
	want := []string{"--name", "duckway", "--kind", "shell", "--cwd", "/home/duck/duckway", "--project-name", "duckway", "--", "/bin/bash"}
	if !ready || name != "duckway" || clientName != "client-b" || strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ready=%v name=%q client=%q args=%#v", ready, name, clientName, args)
	}
}

func TestTUICreateWizardBrowsesAndAddsProjectWhenRegistryIsEmpty(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}
	state := &tuiState{cfg: cfg, runner: fakeRunner{agents: []ducklord.RemoteAgent{{Type: "codex", Command: []string{"codex"}}}}, hostSync: map[string]ducklord.SessionUpdate{"host": {State: "live"}}}
	state.beginCreate()
	state.newSessionLine = "agent"
	createSubmit(t, state)
	state.newSessionLine = "host"
	createSubmit(t, state)
	if state.newSessionStep != "path" {
		t.Fatalf("empty registry step=%q err=%q", state.newSessionStep, state.newSessionErr)
	}
	state.newSessionLine = "/srv/中文 app"
	createSubmit(t, state)
	if state.newSessionStep != "project-policy" {
		t.Fatalf("path submit step=%q err=%v", state.newSessionStep, state.newSessionErr)
	}
	state.newSessionLine = ""
	if _, _, _, ready, err := state.submitCreateStep(context.Background(), make(chan createDiscoveryEvent, 1)); err != nil || ready || state.newSessionStep != "project-name" {
		t.Fatalf("policy submit ready=%v step=%q err=%v", ready, state.newSessionStep, err)
	}
	state.newSessionLine = ""
	createSubmit(t, state)
	if state.newSessionStep != "project" || state.newSessionProject.Path != "/srv/中文 app" || state.newSessionProject.Name != "app" {
		t.Fatalf("added project step=%q project=%#v err=%q", state.newSessionStep, state.newSessionProject, state.newSessionErr)
	}
	createSubmit(t, state)
	if state.newSessionStep != "agent" {
		t.Fatalf("added project did not continue to agent selection: step=%q err=%q", state.newSessionStep, state.newSessionErr)
	}
}

type missingPathRunner struct {
	fakeRunner
	createCalls int
}

func (r *missingPathRunner) EnsureDirectory(_ context.Context, _ ducklord.Client, path string, create bool) (ducklord.RemoteDirectoryStatus, error) {
	if create {
		r.createCalls++
		return ducklord.RemoteDirectoryStatus{Path: path, Exists: true, Created: true}, nil
	}
	return ducklord.RemoteDirectoryStatus{Path: path, Exists: false}, nil
}

func TestTUICreateWizardConfirmsRecursiveMissingDirectory(t *testing.T) {
	runner := &missingPathRunner{fakeRunner: fakeRunner{agents: []ducklord.RemoteAgent{{Type: "codex", Command: []string{"codex"}}}}}
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: runner, hostSync: map[string]ducklord.SessionUpdate{"host": {State: "live"}}}
	state.beginCreate()
	state.newSessionStep, state.newSessionClient, state.newSessionLine = "path", "host", "/srv/new/deep/path"
	createSubmit(t, state)
	if state.newSessionStep != "path-confirm" || runner.createCalls != 0 {
		t.Fatalf("step=%q creates=%d", state.newSessionStep, runner.createCalls)
	}
	var modal bytes.Buffer
	state.renderCreateModal(&modal, 100, 30)
	if !strings.Contains(modal.String(), "CREATE REMOTE DIRECTORY") || !strings.Contains(modal.String(), "/srv/new/deep/path") {
		t.Fatalf("confirmation modal=%q", modal.String())
	}
	state.backCreateStep()
	if state.newSessionStep != "path" || state.newSessionLine != "/srv/new/deep/path" {
		t.Fatalf("back step=%q line=%q", state.newSessionStep, state.newSessionLine)
	}
	createSubmit(t, state)
	createSubmit(t, state)
	if state.newSessionStep != "project-policy" || runner.createCalls != 1 {
		t.Fatalf("step=%q creates=%d err=%q", state.newSessionStep, runner.createCalls, state.newSessionErr)
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

func TestTUIPathSuggestionIsCanceledWhenHostChanges(t *testing.T) {
	state := &tuiState{
		cfg:                       &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}},
		newSessionMode:            true,
		newSessionStep:            "path",
		newSessionClient:          "host",
		newSessionPathBusy:        true,
		newSessionPathCancel:      func() {},
		newSessionPathSuggestions: []string{"/stale"},
		hostSync:                  map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 1, InstanceID: "old"}},
	}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", State: "reconnecting", Generation: 2, InstanceID: "new"})
	if state.newSessionPathBusy || state.newSessionStep != "host" || len(state.newSessionPathSuggestions) != 0 {
		t.Fatalf("stale suggestion state survived reconnect: busy=%v step=%q paths=%#v", state.newSessionPathBusy, state.newSessionStep, state.newSessionPathSuggestions)
	}
}

func TestTUIUseOnceAgentFailureReturnsToPolicy(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: fakeRunner{agentsErr: errors.New("runtime unavailable")},
		newSessionMode: true, newSessionKind: model.KindAgent, newSessionStep: "project-policy", newSessionClient: "host",
		newSessionProject: ducklord.RemoteProject{Path: "/work/once", Source: "path"}, newSessionLine: "2",
		hostSync: map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live"}}}
	done := make(chan createDiscoveryEvent, 1)
	if _, _, _, _, err := state.submitCreateStep(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	state.applyCreateDiscovery(<-done)
	if state.newSessionStep != "project-policy" || !strings.Contains(state.newSessionErr, "runtime unavailable") {
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
	if state.cfg != cfg {
		t.Fatal("add client replaced the live config pointer used by TUI watchers")
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

type blockingAddClientRunner struct {
	fakeRunner
	started map[string]chan struct{}
	release map[string]chan struct{}
}

func (r *blockingAddClientRunner) ProbeDucklion(_ context.Context, client ducklord.Client) (ducklord.DucklionProbe, error) {
	close(r.started[client.Name])
	<-r.release[client.Name] // deliberately ignores cancellation to exercise the stale-result fence
	return ducklord.DucklionProbe{Available: true, Command: "ducklion", Version: "test", ListOK: true}, nil
}

func TestAsyncAddClientCancelFencesLateSuccessAndLatestAttemptWins(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yaml")
	runner := &blockingAddClientRunner{
		started: map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})},
		release: map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})},
	}
	state := &tuiState{cfg: &ducklord.Config{}, cfgPath: config, runner: runner, addClientMode: true, addClientLine: "a"}
	done := make(chan addClientDoneEvent, 2)
	if err := state.startAddClient(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	<-runner.started["a"]
	state.cancelAddClient()

	state.addClientMode = true
	state.addClientLine = "b"
	if err := state.startAddClient(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	<-runner.started["b"]
	close(runner.release["b"])
	if !state.completeAddClient(<-done) {
		t.Fatal("latest add-client attempt was not committed")
	}
	close(runner.release["a"])
	if state.completeAddClient(<-done) {
		t.Fatal("canceled stale attempt was committed")
	}
	loaded, err := ducklord.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Clients) != 1 || loaded.Clients[0].Name != "b" {
		t.Fatalf("saved clients = %+v", loaded.Clients)
	}
}

func TestAsyncAddClientSaveFailureKeepsModal(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Mkdir(badPath, 0700); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{cfg: &ducklord.Config{}, cfgPath: badPath, runner: fakeRunner{}, addClientMode: true, addClientBusy: true, addClientRequestID: 7}
	result := addClientDoneEvent{id: 7, client: ducklord.Client{Name: "remote", Host: "remote"}, installed: true}
	if state.completeAddClient(result) {
		t.Fatal("save failure reported as committed")
	}
	if !state.addClientMode || len(state.cfg.Clients) != 0 {
		t.Fatalf("modal=%v config=%+v", state.addClientMode, state.cfg.Clients)
	}
	if !strings.Contains(state.addClientErr, "installed remotely") || !strings.Contains(state.addClientErr, "not saved") {
		t.Fatalf("addClientErr = %q", state.addClientErr)
	}
}

type verifyFailureAddClientRunner struct {
	fakeRunner
	probes int
}

func (r *verifyFailureAddClientRunner) ProbeDucklion(context.Context, ducklord.Client) (ducklord.DucklionProbe, error) {
	r.probes++
	if r.probes == 1 {
		return ducklord.DucklionProbe{Available: false}, nil
	}
	return ducklord.DucklionProbe{}, errors.New("verification unavailable")
}

func TestAsyncAddClientReportsInstalledButVerificationFailed(t *testing.T) {
	runner := &verifyFailureAddClientRunner{}
	result := runAddClient(context.Background(), runner, ducklord.Client{Name: "remote", Host: "remote"})
	if !result.installed || result.err == nil {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.err.Error(), "installed remotely") || !strings.Contains(result.err.Error(), "not saved") {
		t.Fatalf("error = %q", result.err)
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
	if state.cfg != cfg {
		t.Fatal("remove replaced the shared config pointer")
	}
	state.applySessionUpdate(ducklord.SessionUpdate{Client: "client-b", Generation: 99, State: "live", Sessions: []ducklord.RemoteSession{{Client: "client-b", Name: "late"}}})
	for _, session := range state.sessions {
		if session.Client == "client-b" {
			t.Fatalf("late removed-host update restored session: %+v", session)
		}
	}
	if !strings.Contains(state.outputErr, "removed host entry client-b") {
		t.Fatalf("outputErr = %q", state.outputErr)
	}
}

func TestTUIRemoveHostUsesChooserAndConfirmation(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "a"}, {Name: "client-b", Host: "b"}}}
	state := &tuiState{cfg: cfg, sessions: []ducklord.RemoteSession{{Client: "client-b"}}, selected: 0}
	state.beginRemoveClient()
	if !state.removeClientMode || state.removeClientSelected != 1 {
		t.Fatalf("remove chooser mode=%v selected=%d", state.removeClientMode, state.removeClientSelected)
	}
	if action := state.handleRemoveClientInput([]byte("\r")); action != "confirm" || state.removeClientConfirm != "client-b" {
		t.Fatalf("first enter action=%q confirm=%q", action, state.removeClientConfirm)
	}
	if action := state.handleRemoveClientInput([]byte("\x1b")); action != "back" || !state.removeClientMode {
		t.Fatalf("escape confirm action=%q mode=%v", action, state.removeClientMode)
	}
	if action := state.handleRemoveClientInput([]byte("\x1b")); action != "cancel" || state.removeClientMode {
		t.Fatalf("escape chooser action=%q mode=%v", action, state.removeClientMode)
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

func TestTUICopyModeShortcutAndFrozenRender(t *testing.T) {
	state := &tuiState{sessions: []ducklord.RemoteSession{{Name: "agent"}}}
	if action := state.handleInput([]byte("v")); action != "copy-mode" {
		t.Fatalf("copy shortcut action=%q", action)
	}
	state.copyMode = true
	var output bytes.Buffer
	state.render(&output)
	if output.Len() != 0 {
		t.Fatalf("copy mode redraw disturbed terminal selection: %q", output.String())
	}
}

func TestTUICopyModeTransitionsAndInputIsolation(t *testing.T) {
	state := &tuiState{terminal: ducklord.NewTerminal(2, 20, 20)}
	state.terminal.Write([]byte("frozen"))
	var output bytes.Buffer
	state.enterCopyMode(&output)
	if !state.copyMode || !strings.Contains(output.String(), "\033[?1002l\033[?1000h\033[?1006h") || !strings.Contains(output.String(), "COPY MODE") {
		t.Fatalf("enter copy mode output=%q state=%v", output.String(), state.copyMode)
	}
	before := output.String()
	state.enterCopyMode(&output)
	if output.String() != before {
		t.Fatal("enter copy mode was not idempotent")
	}
	state.terminal.Write([]byte(" live-change"))
	if state.copyTerminal == nil || strings.Contains(state.copyTerminal.Text(), "live-change") {
		t.Fatalf("copy framebuffer was not frozen: %q", state.copyTerminal.Text())
	}
	for _, input := range [][]byte{[]byte("x"), []byte("\x1bq"), []byte("\x1bc"), []byte("\x1b[A"), []byte("\x1b[<0;2;7M"), []byte("\x1b[<0;2;7m")} {
		if copyModeExitInput(input) {
			t.Fatalf("ordinary copy-mode input %q exited", input)
		}
	}
	for _, input := range [][]byte{[]byte("q"), []byte("v"), []byte("\x03"), []byte("\x1b")} {
		if !copyModeExitInput(input) {
			t.Fatalf("copy-mode exit input %q was ignored", input)
		}
	}
	state.exitCopyMode(&output)
	if state.copyMode || state.copyTerminal != nil || !strings.Contains(output.String(), "\033[?1000l\033[?1006h\033[?1002h") {
		t.Fatalf("exit copy mode output=%q state=%v", output.String(), state.copyMode)
	}
	before = output.String()
	state.exitCopyMode(&output)
	if output.String() != before {
		t.Fatal("exit copy mode was not idempotent")
	}
}

func TestNextInputEventKeepsAltChordAtomic(t *testing.T) {
	for _, chord := range []string{"\x1bq", "\x1bc", "\x1b界"} {
		event, rest, ok := nextInputEvent([]byte(chord))
		if !ok || string(event) != chord || len(rest) != 0 {
			t.Fatalf("chord=%q event=%q rest=%q ok=%v", chord, event, rest, ok)
		}
	}
}

func TestCreateModalIsCenteredColoredAndSanitizesRemoteLabels(t *testing.T) {
	states := []*tuiState{
		{newSessionMode: true, newSessionStep: "host", cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host\nspoof", Host: "target\tbad"}}}},
		{newSessionMode: true, newSessionStep: "project", newSessionClient: "host", newSessionProjects: []ducklord.RemoteProject{{Name: "中文專案", Path: "/work/安全\rhidden"}}},
		{newSessionMode: true, newSessionStep: "agent", newSessionClient: "host", newSessionCWD: "/work", newSessionAgents: []ducklord.RemoteAgent{{Type: "evil\u202Espoof\x1b]2;fake\a"}}},
	}
	for _, state := range states {
		state.newSessionErr = "choose\nforged\tstatus"
		var output bytes.Buffer
		state.renderCreateModal(&output, 80, 24)
		rendered := output.String()
		if !strings.Contains(rendered, modalBorder) || !strings.Contains(rendered, modalSelected) {
			t.Fatalf("modal is not centered/colored: %q", rendered)
		}
		if strings.Contains(rendered, "\n") || strings.Contains(rendered, "\t") || strings.ContainsRune(rendered, '\u202e') || !utf8.ValidString(rendered) {
			t.Fatalf("modal cell sanitization failed: %q", rendered)
		}
		if strings.Count(rendered, "╭") != 1 || strings.Count(rendered, "╯") != 1 {
			t.Fatalf("modal frame missing: %q", rendered)
		}
	}
}

func TestCreateModalKeyboardSelectionFeedsExistingWizard(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionStep: "kind", newSessionLine: "stale"}
	if action := state.handleCreateInput([]byte("\x1b[B")); action != "" || state.newSessionSelected != 1 {
		t.Fatalf("down action=%q selected=%d", action, state.newSessionSelected)
	}
	if state.newSessionLine != "" {
		t.Fatalf("arrow navigation retained stale typed selection %q", state.newSessionLine)
	}
	if action := state.handleCreateInput([]byte("\r")); action != "submit" || state.newSessionLine != "2" {
		t.Fatalf("enter action=%q line=%q", action, state.newSessionLine)
	}
	state.newSessionStep, state.newSessionLine = "handle", ""
	if action := state.handleCreateInput([]byte("j")); action != "" || state.newSessionLine != "j" {
		t.Fatalf("handle input action=%q line=%q", action, state.newSessionLine)
	}
	state.handleCreateInput([]byte("中文🦆"))
	if state.newSessionLine != "j中文🦆" {
		t.Fatalf("Unicode handle input = %q", state.newSessionLine)
	}
	state.handleCreateInput([]byte{0x7f})
	if state.newSessionLine != "j中文" {
		t.Fatalf("Unicode backspace input = %q", state.newSessionLine)
	}
}

func TestCreateModalShowsDefaultHandleAsMutedPlaceholder(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionStep: "handle", newSessionCWD: "/work/中文-project"}
	var output bytes.Buffer
	state.renderCreateModal(&output, 80, 24)
	rendered := output.String()
	if state.newSessionLine != "" {
		t.Fatalf("placeholder mutated input to %q", state.newSessionLine)
	}
	if !strings.Contains(rendered, modalInput+"  handle › "+modalMuted+"中文-project") {
		t.Fatalf("missing muted handle placeholder: %q", rendered)
	}
}

func TestCreatePathPickerSelectsSuggestionWithoutChangingInputEarly(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionStep: "path", newSessionLine: "/work/p", newSessionPathSuggestions: []string{"/work/project-a", "/work/project-b"}, newSessionPathBusy: true, newSessionPathCancel: func() {}}
	if action := state.handleCreateInput([]byte("\x1b[B")); action != "" || state.newSessionPathSelected != 1 || state.newSessionLine != "/work/p" {
		t.Fatalf("down action=%q selected=%d input=%q", action, state.newSessionPathSelected, state.newSessionLine)
	}
	if action := state.handleCreateInput([]byte("\r")); action != "complete" || state.newSessionLine != "/work/project-b" || state.newSessionStep != "path" {
		t.Fatalf("first enter action=%q input=%q step=%q", action, state.newSessionLine, state.newSessionStep)
	}
	if state.newSessionPathBusy || state.newSessionPathCancel != nil {
		t.Fatal("autocomplete did not fence the pending suggestion request")
	}
	if action := state.handleCreateInput([]byte("\r")); action != "submit" {
		t.Fatalf("second enter action=%q input=%q", action, state.newSessionLine)
	}
}

func TestCreateModalConsumesLeftAndRightAndEscapeMovesBack(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionStep: "project-policy", newSessionCWD: "/work/app", newSessionLine: "typed", newSessionSelected: 1}
	for _, input := range []string{"\x1b[D", "\x1b[C"} {
		beforeStep, beforeLine, beforeSelected := state.newSessionStep, state.newSessionLine, state.newSessionSelected
		if action := state.handleCreateInput([]byte(input)); action != "" || state.newSessionStep != beforeStep || state.newSessionLine != beforeLine || state.newSessionSelected != beforeSelected {
			t.Fatalf("direction %q changed modal: action=%q state=%+v", input, action, state)
		}
	}
	if action := state.handleCreateInput([]byte("\x1b")); action != "back" || state.newSessionStep != "path" || state.newSessionLine != "/work/app" {
		t.Fatalf("escape action=%q step=%q line=%q", action, state.newSessionStep, state.newSessionLine)
	}
	if action := state.handleCreateInput([]byte("\x03")); action != "cancel" {
		t.Fatalf("ctrl-c action=%q", action)
	}
}

func TestFreshSelectedPreviewClearsUnread(t *testing.T) {
	instance := string(model.NewInstanceID())
	session := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "ABC123", RuntimeGeneration: 3, Unread: true, Updated: true,
		ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}}
	state := &tuiState{sessions: []ducklord.RemoteSession{session}, activityState: ducklord.NewActivityState(),
		activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}}
	if !state.applyPreviewOutput(previewOutputEvent{id: 7, key: sessionKey(session), generation: 3, text: "done"}, 7) {
		t.Fatal("fresh preview was rejected")
	}
	if state.currentSession().Unread || state.currentSession().Updated {
		t.Fatal("fresh browsed preview retained an unread or updated marker")
	}
}

func TestFreshPreviewKeepsUnreadWhenSeenStateCannotPersist(t *testing.T) {
	instance := string(model.NewInstanceID())
	session := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "ABC123", RuntimeGeneration: 3, Unread: true,
		ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}}
	state := &tuiState{sessions: []ducklord.RemoteSession{session}, activityState: ducklord.NewActivityState(),
		activityStore: ducklord.ActivityStateStore{Path: t.TempDir()}}
	state.applyPreviewOutput(previewOutputEvent{id: 7, key: sessionKey(session), generation: 3, text: "done"}, 7)
	if !state.currentSession().Unread || state.activityState.Sessions[instance+"/ABC123"].Seen[model.NotificationTaskCompleted] != 0 || !strings.Contains(state.outputErr, "local state") {
		t.Fatalf("failed persistence falsely cleared unread: unread=%v state=%+v err=%q", state.currentSession().Unread, state.activityState, state.outputErr)
	}
}

func TestCreateModalTypedChoiceSynchronizesHighlight(t *testing.T) {
	state := &tuiState{
		newSessionMode:     true,
		newSessionStep:     "project",
		newSessionProjects: []ducklord.RemoteProject{{Name: "one", Path: "/work/one"}, {Name: "中文專案", Path: "/work/two"}},
	}
	state.handleCreateInput([]byte("2"))
	if state.newSessionSelected != 1 {
		t.Fatalf("numeric selection = %d, want 1", state.newSessionSelected)
	}
	state.newSessionLine = ""
	state.newSessionSelected = 0
	state.handleCreateInput([]byte("中文專案"))
	if state.newSessionSelected != 1 {
		t.Fatalf("named selection = %d, want 1", state.newSessionSelected)
	}
}

func TestSessionSearchMatchesUnicodeTermsAcrossFieldsWithoutReordering(t *testing.T) {
	state := &tuiState{sessions: []ducklord.RemoteSession{
		{Client: "host-b", Name: "beta", ProjectName: "中文專案", Kind: "agent", AgentType: "codex"},
		{Client: "host-a", Name: "alpha", ProjectName: "other", Kind: "shell"},
		{Client: "host-c", Name: "gamma", ProjectName: "中文專案", Kind: "agent", AgentType: "claude"},
	}}
	state.beginSearch()
	for _, input := range []string{"中", "文", " ", "C", "O", "D", "E", "X"} {
		if action := state.handleSearchInput([]byte(input)); action != "" {
			t.Fatalf("input %q action=%q", input, action)
		}
	}
	results := state.searchResults()
	if len(results) != 1 || results[0].Name != "beta" {
		t.Fatalf("results=%#v", results)
	}
	if got := []string{state.sessions[0].Name, state.sessions[1].Name, state.sessions[2].Name}; !reflect.DeepEqual(got, []string{"beta", "alpha", "gamma"}) {
		t.Fatalf("authoritative order changed: %v", got)
	}
}

func TestSessionSearchConsumesCommandKeysAndKeepsPane(t *testing.T) {
	state := &tuiState{sessions: []ducklord.RemoteSession{{Client: "host", Name: "alpha", Kind: "agent"}}, outputForKey: "pane-key", activeAttachKey: "pane-key"}
	state.beginSearch()
	for _, input := range []string{"q", "g", "o", "/", "y", "R"} {
		state.handleSearchInput([]byte(input))
	}
	if state.searchQuery != "qgo/yR" || state.outputForKey != "pane-key" || state.activeAttachKey != "pane-key" {
		t.Fatalf("query=%q output=%q active=%q", state.searchQuery, state.outputForKey, state.activeAttachKey)
	}
	if action := state.handleSearchInput([]byte("\x1b")); action != "cancel" {
		t.Fatalf("escape action=%q", action)
	}
}

func TestSessionSearchArrowNavigationTracksIdentityAcrossReorder(t *testing.T) {
	state := &tuiState{sessions: []ducklord.RemoteSession{{Client: "host", Name: "a"}, {Client: "host", Name: "b"}, {Client: "host", Name: "c"}}}
	state.beginSearch()
	state.handleSearchInput([]byte("\x1b[B"))
	state.handleSearchInput([]byte("\x1b[B"))
	state.handleSearchInput([]byte("\x1b[A"))
	if state.searchSelected != 1 || state.searchSelectedKey != sessionKey(state.sessions[1]) {
		t.Fatalf("selected=%d key=%q", state.searchSelected, state.searchSelectedKey)
	}
	selectedKey := state.searchSelectedKey
	state.sessions[0], state.sessions[1] = state.sessions[1], state.sessions[0]
	state.syncSearchSelection()
	if state.searchSelected != 0 || state.searchSelectedKey != selectedKey {
		t.Fatalf("reordered selected=%d key=%q want=%q", state.searchSelected, state.searchSelectedKey, selectedKey)
	}
}

func TestSessionSearchActivationRejectsRestartedGeneration(t *testing.T) {
	session := ducklord.RemoteSession{Client: "host", InstanceID: "instance", SessionID: "ABC123", RuntimeGeneration: 2}
	key := sessionKey(session)
	state := &tuiState{searchRevision: 4, searchActivatedRevision: 4, searchActivatedKey: key, searchActivatedGeneration: 1, outputForKey: key, outputFresh: true}
	if state.searchResultIsActivated(session) {
		t.Fatal("old-generation framebuffer armed second Enter")
	}
	state.searchActivatedGeneration = 2
	if !state.searchResultIsActivated(session) {
		t.Fatal("exact-generation framebuffer was not recognized")
	}
	state.searchRevision++
	if state.searchResultIsActivated(session) {
		t.Fatal("edited query retained activation")
	}
}

func TestSessionSearchModalIsCenteredColoredAndShowsEmptyResult(t *testing.T) {
	state := &tuiState{searchMode: true, searchQuery: "不存在", sessions: []ducklord.RemoteSession{{Client: "host", Name: "alpha"}}}
	var output bytes.Buffer
	state.renderSearchModal(&output, 80, 24)
	rendered := output.String()
	for _, want := range []string{"\033[9;5H", modalBorder, modalTitle, modalInput, modalStatus, "No matching sessions"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("search modal missing %q: %q", want, rendered)
		}
	}
}

func TestSessionSearchModalKeepsOnlyMatchingGroups(t *testing.T) {
	state := &tuiState{searchMode: true, searchQuery: "beta", sessions: []ducklord.RemoteSession{
		{Client: "host-a", Group: "group-a", Name: "alpha"},
		{Client: "host-b", Group: "group-b", Name: "beta"},
	}}
	state.activityState = ducklord.NewActivityState()
	state.activityState.Organization.Mode = ducklord.OrganizationHost
	var output bytes.Buffer
	state.renderSearchModal(&output, 80, 24)
	rendered := output.String()
	if !strings.Contains(rendered, "[host-b]") || strings.Contains(rendered, "[host-a]") {
		t.Fatalf("filtered group projection=%q", rendered)
	}
}

func TestNextInputEventKeepsUnicodeRuneIntactAcrossReads(t *testing.T) {
	input := []byte("中文🦆")
	for split := 1; split < len(input); split++ {
		pending := append([]byte(nil), input[:split]...)
		var decoded strings.Builder
		event, rest, ok := nextInputEvent(pending)
		if ok {
			decoded.Write(event)
		}
		if !ok {
			pending = append(rest, input[split:]...)
		} else {
			pending = append(rest, input[split:]...)
		}
		for len(pending) > 0 {
			event, pending, ok = nextInputEvent(pending)
			if !ok {
				t.Fatalf("split %d left incomplete input %x", split, pending)
			}
			decoded.Write(event)
		}
		if got := decoded.String(); got != string(input) || !utf8.ValidString(got) {
			t.Fatalf("split %d decoded invalid suffix %q", split, got)
		}
	}
}

func TestCreateModalHandlesTinyTerminal(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionStep: "kind"}
	var output bytes.Buffer
	state.renderCreateModal(&output, 12, 4)
	if strings.Contains(output.String(), "\033[-") || !utf8.ValidString(output.String()) {
		t.Fatalf("invalid tiny modal output: %q", output.String())
	}
	if !strings.Contains(output.String(), "╭") || !strings.Contains(output.String(), "╯") || !strings.Contains(output.String(), "Ag") || !strings.Contains(output.String(), "type") {
		t.Fatalf("tiny modal omitted selection/input: %q", output.String())
	}
}

func TestCreateModalScrollsToSelectedChoice(t *testing.T) {
	state := &tuiState{newSessionMode: true, newSessionStep: "project", newSessionSelected: 5}
	for i := 1; i <= 10; i++ {
		state.newSessionProjects = append(state.newSessionProjects, ducklord.RemoteProject{Name: fmt.Sprintf("project-%d", i), Path: fmt.Sprintf("/p/%d", i)})
	}
	var output bytes.Buffer
	state.renderCreateModal(&output, 50, 8)
	if !strings.Contains(output.String(), "project-6") || !strings.Contains(output.String(), modalSelected) {
		t.Fatalf("selected choice was not visible: %q", output.String())
	}
}

func TestSecondaryMenusRenderAsCenteredColoredModals(t *testing.T) {
	target := ducklord.RemoteSession{Client: "host-a", InstanceID: string(model.NewInstanceID()), SessionID: "ABC123", Name: "中文 agent", Kind: string(model.KindAgent), RuntimeGeneration: 3}
	tests := []struct {
		name   string
		render func(io.Writer)
		want   string
	}{
		{name: "add host", want: "Add Ducklion host", render: func(out io.Writer) {
			(&tuiState{addClientMode: true, addClientHosts: []ducklord.SSHHost{{Name: "client-a"}}}).renderAddClientModal(out, 80, 24)
		}},
		{name: "notifications", want: "Notifications", render: func(out io.Writer) {
			(&tuiState{notificationMode: true, notificationTarget: target, notificationStaged: map[model.NotificationCategory]bool{model.NotificationTerminalAttention: true}}).renderNotificationModal(out, 80, 24)
		}},
		{name: "lifecycle", want: "DESTROY SESSION", render: func(out io.Writer) {
			(&tuiState{lifecycleConfirm: protocol.SessionLifecycleDestroy, lifecycleTarget: target}).renderLifecycleModal(out, 80, 24)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			test.render(&output)
			rendered := output.String()
			if !strings.Contains(rendered, test.want) || !strings.Contains(rendered, modalBorder) || !strings.Contains(rendered, "\033[") || strings.Count(rendered, "╭") != 1 || strings.Count(rendered, "╯") != 1 {
				t.Fatalf("menu is not a centered colored modal: %q", rendered)
			}
		})
	}
}

func TestSharedModalBoxFitsTinyTerminal(t *testing.T) {
	var output bytes.Buffer
	renderModalBox(&output, 12, 4, []modalRenderLine{{modalTitle, "中文 title"}, {modalSelected, "› selected"}, {modalMuted, "hidden"}})
	rendered := output.String()
	if strings.Contains(rendered, "\033[-") || !utf8.ValidString(rendered) || strings.Count(rendered, "╭") != 1 || strings.Count(rendered, "╯") != 1 || !strings.Contains(rendered, modalSelected) {
		t.Fatalf("tiny modal is invalid: %q", rendered)
	}
}

func TestNotificationSettingsRemainBoundToCapturedSession(t *testing.T) {
	instance := string(model.NewInstanceID())
	a := ducklord.RemoteSession{Client: "host-a", InstanceID: instance, SessionID: "AAA111", Name: "a"}
	b := ducklord.RemoteSession{Client: "host-a", InstanceID: instance, SessionID: "BBB222", Name: "b"}
	state := &tuiState{activityState: ducklord.NewActivityState(), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}, sessions: []ducklord.RemoteSession{a, b}}
	state.beginNotificationSettings()
	state.notificationStaged[model.NotificationTerminalAttention] = false
	state.sessions = []ducklord.RemoteSession{b}
	state.selected = 0
	state.saveNotificationSettings()
	if !state.notificationMode || !strings.Contains(state.outputErr, "target changed") {
		t.Fatalf("stale notification target was not rejected: mode=%v err=%q", state.notificationMode, state.outputErr)
	}
	if !state.activity().Enabled(instance, b.SessionID, model.NotificationTerminalAttention) {
		t.Fatal("stale modal changed the replacement session")
	}
}

func TestAddHostModalArrowSelectionFeedsSubmission(t *testing.T) {
	state := &tuiState{addClientMode: true, addClientLine: "stale", addClientHosts: []ducklord.SSHHost{{Name: "a"}, {Name: "b"}}}
	if action := state.handleAddClientInput([]byte("\x1b[B")); action != "" || state.addClientSelected != 1 || state.addClientLine != "" {
		t.Fatalf("arrow action=%q selected=%d line=%q", action, state.addClientSelected, state.addClientLine)
	}
	if action := state.handleAddClientInput([]byte("\r")); action != "submit" || state.addClientLine != "2" {
		t.Fatalf("submit action=%q line=%q", action, state.addClientLine)
	}
}

func TestTUILeftClickContentPaneKeepsSelectionButAttaches(t *testing.T) {
	state := &tuiState{
		selected: 1,
		sessions: []ducklord.RemoteSession{
			{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell", Group: "lab"},
			{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell", Group: "lab"},
		},
	}
	if action := state.handleInput([]byte("\x1b[<0;70;7M")); action != "attach" {
		t.Fatalf("left-click content action = %q", action)
	}
	if state.selected != 1 {
		t.Fatalf("selected changed to %d", state.selected)
	}
	if action := state.handleInput([]byte("\x1b[<0;70;7m")); action != "" {
		t.Fatalf("mouse release action = %q", action)
	}
}

func TestTUIMouseReleaseNeverTriggersSessionAction(t *testing.T) {
	state := &tuiState{sessions: []ducklord.RemoteSession{
		{Client: "client-a", Name: "alpha", Status: "running", AgentType: "shell", Group: "lab"},
		{Client: "client-b", Name: "beta", Status: "running", AgentType: "shell", Group: "lab"},
	}}
	for _, release := range []string{"\x1b[<0;2;7m", "\x1b[<2;2;7m", "\x1b[<2;70;7m"} {
		state.selected = 0
		if action := state.handleInput([]byte(release)); action != "" || state.selected != 0 {
			t.Fatalf("release=%q action=%q selected=%d", release, action, state.selected)
		}
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

func TestNextInputEventParsesAllArrowKeysAtomically(t *testing.T) {
	for _, sequence := range []string{"\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D"} {
		event, rest, ok := nextInputEvent([]byte(sequence + "x"))
		if !ok || string(event) != sequence || string(rest) != "x" {
			t.Fatalf("sequence=%q event=%q rest=%q ok=%v", sequence, event, rest, ok)
		}
		for split := 1; split < len(sequence); split++ {
			_, pending, early := nextInputEvent([]byte(sequence[:split]))
			if early {
				t.Fatalf("sequence=%q split=%d emitted ambiguous escape prefix", sequence, split)
			}
			event, rest, ok = nextInputEvent(append(pending, sequence[split:]...))
			if !ok || string(event) != sequence || len(rest) != 0 {
				t.Fatalf("sequence=%q split=%d event=%q rest=%q ok=%v", sequence, split, event, rest, ok)
			}
		}
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
func (f fakeRunner) LifecycleSelected(_ context.Context, _ ducklord.Client, session ducklord.RemoteSession, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error) {
	return protocol.SessionLifecycleResult{SessionID: session.SessionID, Operation: operation, Mode: mode, State: protocol.SessionLifecycleCompleted, OwnershipEpoch: session.OwnershipEpoch, RuntimeGeneration: session.RuntimeGeneration}, nil
}
func (f fakeRunner) Yield(context.Context, ducklord.Client, string, bool) (protocol.SessionYieldResult, error) {
	return protocol.SessionYieldResult{}, nil
}
func (f fakeRunner) YieldSelected(context.Context, ducklord.Client, ducklord.RemoteSession, bool) (protocol.SessionYieldResult, error) {
	return protocol.SessionYieldResult{}, nil
}
func (f fakeRunner) Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error) {
	return f.projects, nil
}
func (f fakeRunner) SuggestProjectPaths(context.Context, ducklord.Client, string) ([]string, error) {
	return nil, nil
}
func (f fakeRunner) EnsureDirectory(context.Context, ducklord.Client, string, bool) (ducklord.RemoteDirectoryStatus, error) {
	return ducklord.RemoteDirectoryStatus{Path: "/tmp", Exists: true}, nil
}
func (f fakeRunner) AddProject(_ context.Context, _ ducklord.Client, path, name string) (ducklord.RemoteProject, error) {
	return ducklord.RemoteProject{Name: name, Path: path, Source: "duckway-client"}, nil
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

func (r *recordingRunner) SuggestProjectPaths(context.Context, ducklord.Client, string) ([]string, error) {
	return nil, nil
}
func (r *recordingRunner) EnsureDirectory(_ context.Context, _ ducklord.Client, path string, create bool) (ducklord.RemoteDirectoryStatus, error) {
	return ducklord.RemoteDirectoryStatus{Path: path, Exists: true, Created: create}, nil
}
func (r *recordingRunner) AddProject(_ context.Context, _ ducklord.Client, path, name string) (ducklord.RemoteProject, error) {
	return ducklord.RemoteProject{Name: name, Path: path, Source: "duckway-client"}, nil
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
func (r *recordingRunner) LifecycleSelected(_ context.Context, client ducklord.Client, selected ducklord.RemoteSession, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error) {
	r.lifecycleClient, r.lifecycleSession, r.lifecycleOp, r.lifecycleMode = client.Name, selected.SessionID, operation, mode
	return protocol.SessionLifecycleResult{SessionID: selected.SessionID, Operation: operation, Mode: mode, State: protocol.SessionLifecycleCompleted, OwnershipEpoch: selected.OwnershipEpoch, RuntimeGeneration: selected.RuntimeGeneration}, nil
}
func (r *recordingRunner) Yield(_ context.Context, client ducklord.Client, session string, wait bool) (protocol.SessionYieldResult, error) {
	r.yieldClient = client.Name
	r.yieldSession = session
	r.yieldWait = wait
	return r.yieldResult, nil
}
func (r *recordingRunner) YieldSelected(_ context.Context, client ducklord.Client, session ducklord.RemoteSession, wait bool) (protocol.SessionYieldResult, error) {
	r.yieldClient, r.yieldSession, r.yieldWait = client.Name, session.SessionID, wait
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
