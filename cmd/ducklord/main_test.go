package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	config := filepath.Join(dir, "ducklord.json")
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
		{"start", "client-a", "--name", "bad/name", "--", "bash", "--config", config},
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

func TestDucklordRejectsUnknownTUIFlag(t *testing.T) {
	if _, _, err := parseTUIFlags([]string{"--bad"}); err == nil {
		t.Fatal("unknown tui flag accepted")
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
	for _, line := range []string{"bad/name", "alpha --bad", "alpha --agent bad/value", `alpha -- sh -lc "unterminated`} {
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
	if got := state.handleInput([]byte("n")); got != "" {
		t.Fatalf("host-scoped new shortcut action = %q", got)
	}
	if got := state.handleInput([]byte("d")); got != "" {
		t.Fatalf("host-scoped remove shortcut action = %q", got)
	}
	if got := state.handleInput([]byte("r")); got != "refresh" {
		t.Fatalf("host-scoped refresh action = %q", got)
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
	if !state.newSessionMode || state.newSessionClient != "client-b" || state.newSessionStep != "agent" {
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
		}},
	}
	state.beginCreate()
	state.newSessionLine = "shell"
	if _, _, _, ready, err := state.submitCreateStep(context.Background()); err != nil || ready || state.newSessionStep != "host" {
		t.Fatalf("agent step ready=%v step=%q err=%v", ready, state.newSessionStep, err)
	}
	state.newSessionLine = "2"
	if _, _, _, ready, err := state.submitCreateStep(context.Background()); err != nil || ready || state.newSessionClient != "client-b" || state.newSessionStep != "project" {
		t.Fatalf("host step ready=%v client=%q step=%q err=%v", ready, state.newSessionClient, state.newSessionStep, err)
	}
	state.newSessionLine = "1"
	name, clientName, args, ready, err := state.submitCreateStep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--name", "shell-duckway", "--agent", "shell", "--cwd", "/home/duck/duckway", "--", "bash"}
	if !ready || name != "shell-duckway" || clientName != "client-b" || strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ready=%v name=%q client=%q args=%#v", ready, name, clientName, args)
	}
}

func TestTUICreateWizardAllowsCustomProjectPath(t *testing.T) {
	cfg := &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a", Host: "client-a"}}}
	state := &tuiState{cfg: cfg, runner: fakeRunner{}}
	state.beginCreate()
	state.newSessionLine = "codex"
	if _, _, _, ready, err := state.submitCreateStep(context.Background()); err != nil || ready {
		t.Fatalf("agent step ready=%v err=%v", ready, err)
	}
	state.newSessionLine = ""
	if _, _, _, ready, err := state.submitCreateStep(context.Background()); err != nil || ready {
		t.Fatalf("host step ready=%v err=%v", ready, err)
	}
	state.newSessionLine = "/work/app"
	name, clientName, args, ready, err := state.submitCreateStep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--name", "codex-app", "--agent", "codex", "--cwd", "/work/app", "--", "codex"}
	if !ready || name != "codex-app" || clientName != "client-a" || strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ready=%v name=%q client=%q args=%#v", ready, name, clientName, args)
	}
}

func TestTUIAddClientFromSSHHostProbesAndSaves(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
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
	config := filepath.Join(t.TempDir(), "config.json")
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
	config := filepath.Join(t.TempDir(), "config.json")
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
	out := make(chan attachOutputEvent, 1)
	completed := make(chan attachDoneEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go superviseAttach(ctx, 7, session, out, completed)
	_ = stdoutW.Close()
	done <- fmt.Errorf("remote failed")
	got := <-completed
	if got.id != 7 || got.err == nil || !strings.Contains(got.err.Error(), "remote failed") {
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
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"clients":[{"name":"client-a","host":"client-a","user":"duck","group":"demo"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakeRunner struct {
	sessions         []ducklord.RemoteSession
	sessionsByClient map[string][]ducklord.RemoteSession
	readText         string
	projects         []ducklord.RemoteProject
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
	return f.readText, nil
}
func (f fakeRunner) Send(context.Context, ducklord.Client, string, string) error { return nil }
func (f fakeRunner) Start(context.Context, ducklord.Client, []string) error      { return nil }
func (f fakeRunner) Stop(context.Context, ducklord.Client, string) error         { return nil }
func (f fakeRunner) Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error) {
	return f.projects, nil
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
func (r *recordingRunner) Start(_ context.Context, client ducklord.Client, args []string) error {
	r.startClient = client.Name
	r.startArgs = append([]string(nil), args...)
	return nil
}
func (r *recordingRunner) Stop(context.Context, ducklord.Client, string) error { return nil }
func (r *recordingRunner) Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error) {
	return nil, nil
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
