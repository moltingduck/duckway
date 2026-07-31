package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion"
)

func TestDucklionListJSONIncludesTailHash(t *testing.T) {
	manager := &fakeManager{
		sessions: []ducklion.Record{{Name: "alpha", Status: ducklion.StatusRunning, AgentType: "shell", Cwd: "/work", PID: 123}},
		readText: "hello\n[ducklion:done] ok\n",
	}
	var out bytes.Buffer
	if err := run(manager, []string{"list", "--json", "--tail-lines", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	var got []sessionOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Backend != "pty" || got[0].LastLine != "[ducklion:done] ok" || got[0].TailHash == "" {
		t.Fatalf("list = %+v", got)
	}
}

func TestDucklionRejectsUnknownStartOption(t *testing.T) {
	if _, err := parseStart([]string{"--bad"}); err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestDucklionAttachUsesManager(t *testing.T) {
	manager := &fakeManager{}
	if err := run(manager, []string{"attach", "alpha"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if manager.attachedName != "alpha" {
		t.Fatalf("attached = %q", manager.attachedName)
	}
}

type fakeManager struct {
	sessions     []ducklion.Record
	started      ducklion.StartOptions
	readText     string
	sentName     string
	sentText     string
	attachedName string
}

func (f *fakeManager) List() ([]ducklion.Record, error) { return f.sessions, nil }

func (f *fakeManager) Start(opts ducklion.StartOptions) (*ducklion.Record, error) {
	f.started = opts
	return &ducklion.Record{Name: opts.Name, AgentType: opts.AgentType, PID: 123}, nil
}

func (f *fakeManager) Send(name, text string) error {
	f.sentName = name
	f.sentText = text
	return nil
}

func (f *fakeManager) Read(string, int) (string, error) { return f.readText, nil }

func (f *fakeManager) Attach(name string, _ io.Reader, _ io.Writer) error {
	f.attachedName = name
	return nil
}

func (f *fakeManager) Stop(string) error { return nil }

func TestParseStartUsesDucklionOptions(t *testing.T) {
	opts, err := parseStart([]string{"--name", "alpha", "--agent", "shell", "--cwd", "/tmp", "--", "sh", "-lc", "echo ok"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "alpha" || strings.Join(opts.Command, " ") != "sh -lc echo ok" {
		t.Fatalf("opts = %+v", opts)
	}
}
