package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/client"
)

func TestParseSessionStartOptions(t *testing.T) {
	opts, err := parseSessionStartOptions([]string{"--name", "review", "--agent", "codex", "--cwd", "/repo", "--", "codex", "exec"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "review" || opts.AgentType != "codex" || opts.Cwd != "/repo" || strings.Join(opts.Command, " ") != "codex exec" {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestRunSessionListMissingState(t *testing.T) {
	var out bytes.Buffer
	manager := &fakeSessionManager{}
	if err := runSessionCommand(manager, []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No local terminal sessions") {
		t.Fatalf("list output = %q", out.String())
	}
}

func TestRunSessionStartSendReadStop(t *testing.T) {
	manager := &fakeSessionManager{readText: "pane output\n"}
	var out bytes.Buffer
	if err := runSessionCommand(manager, []string{"start", "--name", "review", "--agent", "codex", "--cwd", "/repo", "--", "codex", "exec"}, &out); err != nil {
		t.Fatal(err)
	}
	if manager.started.Name != "review" || manager.started.AgentType != "codex" || strings.Join(manager.started.Command, " ") != "codex exec" {
		t.Fatalf("started = %+v", manager.started)
	}
	if err := runSessionCommand(manager, []string{"send", "review", "hello", "agent"}, &out); err != nil {
		t.Fatal(err)
	}
	if manager.sentName != "review" || manager.sentText != "hello agent" {
		t.Fatalf("send = (%q,%q)", manager.sentName, manager.sentText)
	}
	out.Reset()
	if err := runSessionCommand(manager, []string{"read", "review", "--lines", "42"}, &out); err != nil {
		t.Fatal(err)
	}
	if manager.readName != "review" || manager.readLines != 42 || out.String() != "pane output\n" {
		t.Fatalf("read name=%q lines=%d out=%q", manager.readName, manager.readLines, out.String())
	}
	if err := runSessionCommand(manager, []string{"stop", "review"}, &out); err != nil {
		t.Fatal(err)
	}
	if manager.stoppedName != "review" {
		t.Fatalf("stopped = %q", manager.stoppedName)
	}
}

func TestRunSessionAttachUsesManagerTarget(t *testing.T) {
	manager := &fakeSessionManager{}
	var got []string
	old := sessionAttachExec
	sessionAttachExec = func(args []string) error {
		got = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { sessionAttachExec = old })

	if err := runSessionCommand(manager, []string{"attach", "review"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "attach -t duckway-term-review" {
		t.Fatalf("attach args = %q", strings.Join(got, " "))
	}
}

type fakeSessionManager struct {
	sessions    []client.SessionRecord
	started     client.SessionStartOptions
	sentName    string
	sentText    string
	readName    string
	readLines   int
	readText    string
	stoppedName string
}

func (f *fakeSessionManager) List() ([]client.SessionRecord, error) {
	return f.sessions, nil
}

func (f *fakeSessionManager) Start(opts client.SessionStartOptions) (*client.SessionRecord, error) {
	f.started = opts
	return &client.SessionRecord{Name: opts.Name, AgentType: opts.AgentType, TmuxSession: "duckway-term-" + opts.Name}, nil
}

func (f *fakeSessionManager) Send(name, text string) error {
	f.sentName = name
	f.sentText = text
	return nil
}

func (f *fakeSessionManager) Read(name string, lines int) (string, error) {
	f.readName = name
	f.readLines = lines
	return f.readText, nil
}

func (f *fakeSessionManager) Stop(name string) error {
	f.stoppedName = name
	return nil
}

func (f *fakeSessionManager) AttachArgs(name string) ([]string, error) {
	return []string{"attach", "-t", "duckway-term-" + name}, nil
}
