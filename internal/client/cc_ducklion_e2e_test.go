package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	duckliondaemon "github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func TestDiscordYieldCommandUsesDurableDucklionBindingE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts the real ducklion daemon")
	}
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	configDir := t.TempDir()
	binary := filepath.Join(t.TempDir(), "ducklion")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ducklion")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ducklion: %v\n%s", err, output)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, binary, "daemon")
	command.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = command.Wait() }()
	socket := filepath.Join(configDir, "ducklion", "ducklion.sock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ducklion daemon socket did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}

	terminal, err := duckliondaemon.Dial(socket, "e2e-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	session, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "discord-e2e", Kind: model.KindAgent,
		AgentType: "fixture", CWD: configDir, Command: []string{"sh", "-c", `while IFS= read -r value; do printf 'managed:%s\n' "$value"; done`}})
	if err != nil || session.Status != model.StatusRunning {
		t.Fatalf("create session=%+v err=%v", session, err)
	}
	management, err := duckliondaemon.DialCC(socket, "dwch_mgmt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := management.BindDiscordSession(context.Background(), "bind-e2e", session.SessionID, "dwch_task"); err != nil {
		t.Fatal(err)
	}
	management.Close()

	fake := newFakeServer(t)
	watch := stubWatch(t, configDir, fake)
	watch.configDir = configDir
	var delivered bool
	if handled := watch.preflightBoundDucklionPrompt("dwch_task", session.SessionID, "must not reach another process", func(success bool, _ string) { delivered = success }); !handled || !delivered {
		t.Fatalf("terminal-owned prompt handled=%v delivered=%v", handled, delivered)
	}
	preflightMessages := fake.snapshotMessages()
	if len(preflightMessages) != 1 || !strings.Contains(preflightMessages[0]["content"], "controlled by `terminal:e2e-terminal`") {
		t.Fatalf("ownership rejection=%v", preflightMessages)
	}
	sendClientCommand(t, watch, "dwch_task", "!yield", nil)
	messages := fake.snapshotMessages()
	if len(messages) != 2 || !strings.Contains(messages[1]["content"], "Discord now owns session") {
		t.Fatalf("yield reply=%v", messages)
	}
	sessions, err := terminal.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].Writer == nil || sessions[0].Writer.Kind != model.OwnerCC || sessions[0].Writer.ID != "dwch_task" {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	current := sessions[0]
	output, err := terminal.SubscribeOutput(current.SessionID, current.RuntimeGeneration, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	taskCC, err := duckliondaemon.DialCC(socket, "dwch_task")
	if err != nil {
		t.Fatal(err)
	}
	prompt := []byte("same persistent PTY")
	task := protocol.AgentTaskSubmit{TaskID: "inbox/42", Prompt: prompt, PromptDigest: sha256.Sum256(prompt)}
	for i := 0; i < 2; i++ {
		state, submitErr := taskCC.SubmitAgentTask(context.Background(), task.TaskID, current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, task)
		if submitErr != nil || state.Status != "running" {
			t.Fatalf("submit %d state=%+v err=%v", i, state, submitErr)
		}
	}
	frames := make(chan []byte, 8)
	go func() {
		for {
			frame, readErr := output.Read()
			if readErr != nil {
				return
			}
			frames <- frame.Frame.Data
		}
	}()
	var observed bytes.Buffer
	deadline = time.Now().Add(2 * time.Second)
	var firstOutputAt time.Time
	for time.Now().Before(deadline) {
		select {
		case frame := <-frames:
			observed.Write(frame)
		case <-time.After(50 * time.Millisecond):
			if bytes.Contains(observed.Bytes(), []byte("managed:")) && firstOutputAt.IsZero() {
				firstOutputAt = time.Now()
			}
			if !firstOutputAt.IsZero() && time.Since(firstOutputAt) >= 250*time.Millisecond {
				deadline = time.Now()
			}
		}
	}
	if count := bytes.Count(observed.Bytes(), []byte("managed:")); count != 1 {
		t.Fatalf("managed prompt executions=%d output=%q", count, observed.String())
	}
	if _, err := taskCC.CompleteTask(context.Background(), "complete-e2e", current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	_ = taskCC.Close()
	// Return ownership and stop the real PTY so the test leaves no supervisor.
	if _, err := terminal.YieldSession(context.Background(), current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, false); err != nil {
		t.Fatal(err)
	}
	if err := terminal.StopSession(context.Background(), current.SessionID, current.OwnershipEpoch+1, current.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
}
