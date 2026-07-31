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

func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"clients":[{"name":"client-a","host":"client-a","user":"duck","group":"demo"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakeRunner struct {
	sessions []ducklord.RemoteSession
	readText string
}

func (f fakeRunner) Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error) {
	return f.sessions, nil
}
func (f fakeRunner) Read(context.Context, ducklord.Client, string, int) (string, error) {
	return f.readText, nil
}
func (f fakeRunner) Send(context.Context, ducklord.Client, string, string) error { return nil }
func (f fakeRunner) Start(context.Context, ducklord.Client, []string) error      { return nil }
func (f fakeRunner) Stop(context.Context, ducklord.Client, string) error         { return nil }
func (f fakeRunner) Attach(ducklord.Client, string) error                        { return nil }
