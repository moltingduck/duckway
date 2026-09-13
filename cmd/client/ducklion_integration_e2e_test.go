package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/client"
	"github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/management"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func TestDucklionStandaloneIntegrationE2E(t *testing.T) {
	if os.Getenv("DUCKWAY_INTEGRATION_E2E") != "1" {
		t.Skip("set DUCKWAY_INTEGRATION_E2E=1 to build real CLI binaries and run daemon handoff")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	build := func(name, pkg string) string {
		t.Helper()
		path := filepath.Join(root, name)
		cmd := exec.Command("go", "build", "-o", path, pkg)
		cmd.Dir = filepath.Join("..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
		return path
	}
	ducklion := build("ducklion", "./cmd/ducklion")
	duckway := build("duckway", "./cmd/client")
	run := func(exe string, args ...string) string {
		t.Helper()
		cmd := exec.Command(exe, args...)
		cmd.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", exe, args, err, out)
		}
		return string(out)
	}
	if err := client.SaveConfig(configDir, &client.Config{ServerURL: "http://127.0.0.1:1", ClientName: "fixture", Token: "fixture", ProxyPort: 18080}); err != nil {
		t.Fatal(err)
	}
	run(ducklion, "daemon", "start")
	defer func() {
		if record, err := management.Read(filepath.Join(configDir, "ducklion")); err == nil {
			command := exec.Command(duckway, "stop")
			if record.Mode == management.Integrated {
				command.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir)
				_ = command.Run()
			} else {
				command = exec.Command(ducklion, "daemon", "stop")
				command.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir)
				_ = command.Run()
			}
		}
	}()
	stateRoot := filepath.Join(configDir, "ducklion")
	standalonePID, standaloneAlive := management.ReadPID(management.StandalonePID(stateRoot))
	if !standaloneAlive {
		t.Fatal("standalone daemon did not start")
	}
	if out := run(duckway, "start"); !strings.Contains(out, "skipped") || !strings.Contains(out, "integrate ducklion") {
		t.Fatalf("Duckway did not skip standalone Ducklion: %q", out)
	}
	run(duckway, "stop")
	if pid, alive := management.ReadPID(management.StandalonePID(stateRoot)); !alive || pid != standalonePID {
		t.Fatalf("Duckway stop affected standalone Ducklion: pid=%d alive=%t", pid, alive)
	}
	conn, err := daemon.Dial(filepath.Join(stateRoot, "ducklion.sock"), "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	created, err := conn.CreateSession(context.Background(), protocol.SessionCreate{Handle: "survive", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		sessions, listErr := conn.ListSessions()
		if listErr == nil && len(sessions) == 1 && sessions[0].Status == model.StatusRunning {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatalf("shell did not become ready before integration: %+v %v", sessions, listErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cc, err := daemon.DialCC(filepath.Join(stateRoot, "ducklion.sock"), "dwch_integration")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := cc.CreateSession(context.Background(), protocol.SessionCreate{Handle: "busy-agent", Kind: model.KindAgent, AgentType: "fixture", CWD: root,
		Command: []string{"sh", "-c", "while IFS= read -r line; do :; done"}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = cc.BeginTask(context.Background(), "busy-before-integrate", agent.SessionID, agent.OwnershipEpoch, agent.RuntimeGeneration)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not accept task: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	busy := exec.Command(duckway, "integrate", "ducklion")
	busy.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir)
	if out, err := busy.CombinedOutput(); err == nil || !strings.Contains(string(out), "agent task is active") {
		t.Fatalf("busy integration result=%v output=%q", err, out)
	}
	if record, err := management.Read(stateRoot); err != nil || record.Mode != management.Standalone {
		t.Fatalf("busy conversion changed manager: %+v %v", record, err)
	}
	if pid, alive := management.ReadPID(management.StandalonePID(stateRoot)); !alive || pid != standalonePID {
		t.Fatalf("busy conversion restarted standalone daemon: pid=%d alive=%t", pid, alive)
	}
	if _, err := cc.CompleteTask(context.Background(), "complete-before-integrate", agent.SessionID, agent.OwnershipEpoch, agent.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	_ = cc.Close()
	_ = conn.Close()
	if out := run(duckway, "integrate", "ducklion"); !strings.Contains(out, "integrated with Duckway") {
		t.Fatalf("integration output=%q", out)
	}
	record, err := management.Read(stateRoot)
	if err != nil || record.Mode != management.Integrated {
		t.Fatalf("manager=%+v err=%v", record, err)
	}
	conn, err = daemon.Dial(filepath.Join(stateRoot, "ducklion.sock"), "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sessions, err := conn.ListSessions()
	if err != nil || len(sessions) != 2 || sessions[0].SessionID != created.SessionID || sessions[0].RuntimeGeneration != created.RuntimeGeneration {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if err := conn.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf 'survived integration\\n'\nexit\n")); err != nil {
		t.Fatal(err)
	}
	cc, err = daemon.DialCC(filepath.Join(stateRoot, "ducklion.sock"), "dwch_integration")
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.StopSession(context.Background(), agent.SessionID, agent.OwnershipEpoch, agent.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	_ = cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("shell did not exit after integration")
		default:
		}
		sessions, err := conn.ListSessions()
		if err == nil {
			for _, session := range sessions {
				if session.SessionID == created.SessionID && session.Status == model.StatusStopped {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}
