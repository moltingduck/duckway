package main

import (
	"bytes"
	"context"
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

func TestDucklordRejectsUnknownTUIFlag(t *testing.T) {
	if _, _, err := parseTUIFlags([]string{"--bad"}); err == nil {
		t.Fatal("unknown tui flag accepted")
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
func (f fakeRunner) Attach(ducklord.Client, string) error                        { return nil }

type recordingRunner struct {
	readText    string
	readClient  string
	readSession string
}

func (r *recordingRunner) Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error) {
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
func (r *recordingRunner) Start(context.Context, ducklord.Client, []string) error { return nil }
func (r *recordingRunner) Stop(context.Context, ducklord.Client, string) error    { return nil }
func (r *recordingRunner) Attach(ducklord.Client, string) error                   { return nil }
