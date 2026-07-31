package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/client"
)

func TestDucklionListJSONIncludesTailHash(t *testing.T) {
	manager := &fakeManager{
		sessions: []client.SessionRecord{{Name: "alpha", Status: client.SessionStatusRunning, AgentType: "shell", Cwd: "/work", TmuxSession: "duckway-term-alpha"}},
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
	if len(got) != 1 || got[0].LastLine != "[ducklion:done] ok" || got[0].TailHash == "" {
		t.Fatalf("list = %+v", got)
	}
}

func TestDucklionRejectsUnknownStartOption(t *testing.T) {
	if _, err := parseStart([]string{"--bad"}); err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestDucklionAttachUsesSessionManagerTarget(t *testing.T) {
	manager := &fakeManager{}
	var got []string
	old := attachExec
	attachExec = func(args []string) error {
		got = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { attachExec = old })
	if err := run(manager, []string{"attach", "alpha"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "attach -t duckway-term-alpha" {
		t.Fatalf("attach args = %q", strings.Join(got, " "))
	}
}

type fakeManager struct {
	sessions []client.SessionRecord
	started  client.SessionStartOptions
	readText string
	sentName string
	sentText string
}

func (f *fakeManager) List() ([]client.SessionRecord, error) { return f.sessions, nil }

func (f *fakeManager) Start(opts client.SessionStartOptions) (*client.SessionRecord, error) {
	f.started = opts
	return &client.SessionRecord{Name: opts.Name, AgentType: opts.AgentType}, nil
}

func (f *fakeManager) Send(name, text string) error {
	f.sentName = name
	f.sentText = text
	return nil
}

func (f *fakeManager) Read(string, int) (string, error) { return f.readText, nil }
func (f *fakeManager) Stop(string) error                { return nil }
func (f *fakeManager) AttachArgs(name string) ([]string, error) {
	return []string{"attach", "-t", "duckway-term-" + name}, nil
}
