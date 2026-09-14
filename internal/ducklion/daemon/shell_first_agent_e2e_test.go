package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

// This exercises the shell-first contract without contacting either agent's
// service. The fixture commands stand in for short-lived Codex/Claude CLI
// processes launched by the user inside one long-lived interactive shell.
func TestShellFirstAgentCommandsReturnToSamePTYWithoutFalseCompletion(t *testing.T) {
	root := t.TempDir()
	for _, fixture := range []struct{ name, marker string }{
		{"fake-codex", "CODEX_CHILD_EXITED"},
		{"fake-claude", "CLAUDE_CHILD_EXITED"},
	} {
		path := filepath.Join(root, fixture.name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' "+fixture.marker+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}

	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: func(specPath string) error {
		go func() { _ = RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()

	client, err := Dial(server.SocketPath(), "shell-first-e2e")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	created, err := client.CreateSession(context.Background(), protocol.SessionCreate{
		Handle: "shell-first", Kind: model.KindShell, CWD: root, Command: []string{"sh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "sessions", created.SessionID, "output.1.log")
	waitOutput := func(marker string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			output, readErr := os.ReadFile(logPath)
			if readErr == nil && bytes.Contains(output, []byte(marker)) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		output, _ := os.ReadFile(logPath)
		t.Fatalf("PTY never emitted %q; output=%q", marker, output)
	}
	input := func(command string) {
		t.Helper()
		if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte(command+"\n")); err != nil {
			t.Fatal(err)
		}
	}

	input("./fake-codex")
	waitOutput("CODEX_CHILD_EXITED")
	input("./fake-claude")
	waitOutput("CLAUDE_CHILD_EXITED")
	// Keep the complete marker out of the echoed command line, so observing it
	// proves that the shell executed this third command after both child exits.
	input("printf 'SHELL_%s_INTERACTIVE\\n' STILL")
	waitOutput("SHELL_STILL_INTERACTIVE")

	sessions, err := client.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != created.SessionID ||
		sessions[0].RuntimeGeneration != created.RuntimeGeneration || sessions[0].Status != model.StatusRunning {
		t.Fatalf("child process exit replaced or ended shell session: %+v", sessions)
	}
	if sessions[0].ActivitySequences[model.NotificationTaskCompleted] != 0 ||
		sessions[0].ActivitySequences[model.NotificationTaskFailed] != 0 {
		t.Fatalf("agent completion was inferred without a hook: %+v", sessions[0].ActivitySequences)
	}
}
