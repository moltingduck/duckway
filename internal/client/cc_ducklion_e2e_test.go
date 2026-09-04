package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
		AgentType: "fixture", CWD: configDir, Command: []string{"sh", "-c", `while IFS= read -r value; do printf 'managed:%s\n' "$value"; printf '%s\n' '{"kind":"progress","summary":"fixture working"}' >&3; printf '%s\n' '{"kind":"completed","response":"fixture done"}' >&3; done`}})
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
	if handled := watch.preflightBoundDucklionPrompt("dwch_task", session.SessionID, "inbox-41", "must not reach another process", func(success bool, _ string) { delivered = success }, nil); !handled || !delivered {
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
		if submitErr != nil || (state.Status != "running" && state.Status != "completed") {
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
	deadline = time.Now().Add(5 * time.Second)
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
	// Restart only the Ducklion daemon after the supervisor has accepted and
	// completed the turn. The PTY/supervisor remains alive and must replay its
	// unacknowledged structured events into the reopened durable state.
	_ = taskCC.Close()
	_ = terminal.Close()
	_ = command.Process.Kill()
	_ = command.Wait()
	command = exec.CommandContext(ctx, binary, "daemon")
	command.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir)
	var restartLog bytes.Buffer
	command.Stdout = &restartLog
	command.Stderr = &restartLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		taskCC, err = duckliondaemon.DialCC(socket, "dwch_task")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("reconnect CC after daemon restart: %v; daemon=%s", err, restartLog.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer taskCC.Close()
	var terminalSequence uint64
	var lastPollErr error
	var lastEvents protocol.AgentTaskEventsResult
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, pollErr := taskCC.AgentTaskEvents(context.Background(), current.SessionID, task.TaskID, 0)
		lastPollErr, lastEvents = pollErr, events
		if pollErr == nil && len(events.Events) == 2 && events.Events[1].Kind == "completed" {
			terminalSequence = events.Events[1].Sequence
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if terminalSequence == 0 {
		summary, _ := taskCC.ListSessions()
		logs, _ := filepath.Glob(filepath.Join(configDir, "ducklion", "sessions", "*", "*.log"))
		var supervisorLog []byte
		if len(logs) > 0 {
			supervisorLog, _ = os.ReadFile(logs[0])
		}
		t.Fatalf("fixture adapter did not report completion: events=%+v err=%v sessions=%+v restart=%s supervisor=%s", lastEvents, lastPollErr, summary, restartLog.String(), supervisorLog)
	}
	if err := taskCC.AckAgentTaskEvent(context.Background(), current.SessionID, task.TaskID, terminalSequence); err != nil {
		t.Fatal(err)
	}
	_ = taskCC.Close()

	messageSnowflake := "1783330000000000043"
	payload, _ := json.Marshal(map[string]interface{}{"id": messageSnowflake, "content": "second managed turn", "author": map[string]interface{}{"id": "U1", "bot": false}})
	envelope, _ := json.Marshal(sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload,
		InboxID: 43, SessionID: current.SessionID, ClaimToken: "claim-43", AttemptCount: 1})
	watch.handleMessageCreate(envelope)
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		finishes := fake.snapshotFinishes()
		if len(finishes) > 0 && finishes[len(finishes)-1]["status"] == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finishes := fake.snapshotFinishes(); len(finishes) == 0 || finishes[len(finishes)-1]["status"] != "completed" {
		t.Fatal("managed durable inbox was not completed")
	}
	if edits := fake.snapshotEdits(); len(edits) == 0 || edits[len(edits)-1] != "✅ Done" {
		t.Fatalf("terminal preview was not finalized: %v", edits)
	}
	messages = fake.snapshotMessages()
	if got := messages[len(messages)-1]; got["content"] != "fixture done" || got["delivery_key"] == "" || got["reply_to_message_id"] != messageSnowflake {
		t.Fatalf("managed final=%v", got)
	}
	messageCount := len(messages)
	envelope, _ = json.Marshal(sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload,
		InboxID: 43, SessionID: current.SessionID, ClaimToken: "claim-43-retry", AttemptCount: 2})
	watch.handleMessageCreate(envelope)
	time.Sleep(time.Second)
	if got := len(fake.snapshotMessages()); got != messageCount {
		t.Fatalf("acked inbox replay posted duplicate messages: before=%d after=%d", messageCount, got)
	}
	terminal, err = duckliondaemon.Dial(socket, "e2e-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	// Return ownership and stop the real PTY so the test leaves no supervisor.
	if _, err := terminal.YieldSession(context.Background(), current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, false); err != nil {
		t.Fatal(err)
	}
	if err := terminal.StopSession(context.Background(), current.SessionID, current.OwnershipEpoch+1, current.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
}
