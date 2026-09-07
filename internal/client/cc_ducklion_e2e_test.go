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
	ducklionstore "github.com/hackerduck/duckway/internal/ducklion/store"
)

func TestDiscordBindExistingDucklionSessionE2E(t *testing.T) {
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

	terminal, err := duckliondaemon.Dial(socket, "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	session, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "中文工作階段", Kind: model.KindAgent,
		AgentType: "fixture", CWD: configDir, Command: []string{"sh", "-c", `while IFS= read -r value; do printf '%s\n' "$value"; done`}})
	if err != nil {
		t.Fatal(err)
	}

	fake := newFakeServer(t)
	watch := stubWatch(t, configDir, fake)
	watch.configDir = configDir
	watch.cmdDucklionBind(context.Background(), "dwch_mgmt", "discord-bind-1", []string{strings.ToLower(session.SessionID)})
	creates := fake.snapshotCreates()
	if len(creates) != 1 {
		t.Fatalf("channel creates=%v", creates)
	}
	messages := fake.snapshotMessages()
	if len(messages) != 1 || !strings.Contains(messages[0]["content"], "read-only") || !strings.Contains(messages[0]["content"], "`!yield`") {
		t.Fatalf("bind success reply=%v", messages)
	}

	management, err := duckliondaemon.DialCC(socket, "dwch_mgmt")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := management.DiscordBindingForSession(context.Background(), session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ChannelHandle != "dwch_test1" || binding.ManagementHandle != "dwch_mgmt" {
		t.Fatalf("binding=%+v", binding)
	}
	listed, err := management.ListSessions()
	if err != nil || len(listed) != 1 || listed[0].Writer == nil || listed[0].Writer.Kind != model.OwnerTerminal || listed[0].Writer.ID != "laptop-a" {
		t.Fatalf("bind changed writer: sessions=%+v err=%v", listed, err)
	}
	_ = management.Close()

	watch.cmdDucklionBind(context.Background(), "dwch_mgmt", "discord-bind-1", []string{session.SessionID})
	if creates = fake.snapshotCreates(); len(creates) != 1 {
		t.Fatalf("repeat bind duplicated channel: %v", creates)
	}
	messages = fake.snapshotMessages()
	if len(messages) != 1 {
		t.Fatalf("same request replay was not reply-idempotent: %v", messages)
	}
	watch.cmdDucklionBind(context.Background(), "dwch_other", "discord-bind-2", []string{session.SessionID})
	if creates = fake.snapshotCreates(); len(creates) != 1 {
		t.Fatalf("cross-management bind duplicated channel: %v", creates)
	}
	messages = fake.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1]["content"], "another management channel") {
		t.Fatalf("cross-management reply=%v", messages)
	}

	// Model a crash after Discord channel creation + fail-closed marker, but
	// before Ducklion binding. Reloading the workflow must reuse the channel.
	second, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "replay", Kind: model.KindAgent,
		AgentType: "fixture", CWD: configDir, Command: []string{"sh", "-c", `while IFS= read -r value; do printf '%s\n' "$value"; done`}})
	if err != nil {
		t.Fatal(err)
	}
	precreated, err := watch.api.CreateCCChannelIdempotent(context.Background(), "discord-bind-replay:bind:"+second.SessionID, "replay", "replay", configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := watch.api.SetCCChannelSession(context.Background(), precreated.Handle, second.SessionID, second.CWD); err != nil {
		t.Fatal(err)
	}
	record, err := watch.provisions.Reserve(ccProvisionRecord{RequestID: "discord-bind-replay:bind:" + second.SessionID, Kind: "bind", SessionID: second.SessionID,
		ManagementHandle: "dwch_mgmt", Slug: second.Handle, CWD: second.CWD, Channel: precreated, Session: &second})
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = ccProvisionMarkerSet
	if err := watch.provisions.Save(record); err != nil {
		t.Fatal(err)
	}
	watch.provisions = newCCProvisionStore(filepath.Dir(watch.provisions.path))
	pendingReplay := watch.provisions.Pending()
	if len(pendingReplay) != 1 || pendingReplay[0].Channel == nil || pendingReplay[0].Channel.Handle != precreated.Handle {
		t.Fatalf("persisted bind workflow=%+v", pendingReplay)
	}
	beforeReplayCreates := len(fake.snapshotCreates())
	watch.cmdDucklionBind(context.Background(), "dwch_mgmt", "discord-bind-replay", []string{second.SessionID})
	if got := len(fake.snapshotCreates()); got != beforeReplayCreates {
		t.Fatalf("marker crash replay created another channel: before=%d after=%d", beforeReplayCreates, got)
	}
	management, err = duckliondaemon.DialCC(socket, "dwch_mgmt")
	if err != nil {
		t.Fatal(err)
	}
	replayedBinding, err := management.DiscordBindingForSession(context.Background(), second.SessionID)
	_ = management.Close()
	if err != nil || replayedBinding.ChannelHandle != precreated.Handle {
		t.Fatalf("replayed binding=%+v err=%v", replayedBinding, err)
	}

	var delivered bool
	if handled := watch.preflightBoundDucklionPrompt("dwch_test1", session.SessionID, "inbox-bind", "must stay read-only", func(success bool, _ string) { delivered = success }, nil); !handled || !delivered {
		t.Fatalf("read-only prompt handled=%v delivered=%v", handled, delivered)
	}
	messages = fake.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1]["content"], "controlled by `terminal:laptop-a`") {
		t.Fatalf("read-only reply=%v", messages)
	}
}

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
	terminal, err = duckliondaemon.Dial(socket, "e2e-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	beforeAck, err := terminal.ListSessions()
	if err != nil || len(beforeAck) != 1 || beforeAck[0].TaskState != model.TaskReplying || beforeAck[0].Writer == nil || beforeAck[0].Writer.Kind != model.OwnerCC {
		t.Fatalf("terminal event became idle before delivery ACK: sessions=%+v err=%v", beforeAck, err)
	}
	waiting, err := terminal.YieldSession(context.Background(), current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, true)
	if err != nil || waiting.Decision != model.YieldWaiting {
		t.Fatalf("wait-yield before delivery ACK=%+v err=%v", waiting, err)
	}
	if err := taskCC.AckAgentTaskEvent(context.Background(), current.SessionID, task.TaskID, terminalSequence); err != nil {
		t.Fatal(err)
	}
	afterAck, err := terminal.ListSessions()
	if err != nil || len(afterAck) != 1 || afterAck[0].TaskState != model.TaskIdle || afterAck[0].Writer == nil || afterAck[0].Writer.Kind != model.OwnerTerminal || afterAck[0].OwnershipEpoch != current.OwnershipEpoch+1 {
		t.Fatalf("delivery ACK did not apply waiting yield: sessions=%+v err=%v", afterAck, err)
	}
	returned, err := taskCC.YieldSession(context.Background(), current.SessionID, afterAck[0].OwnershipEpoch, current.RuntimeGeneration, false)
	if err != nil || returned.Decision != model.YieldTransferred || returned.Writer == nil || returned.Writer.Kind != model.OwnerCC {
		t.Fatalf("return control to CC=%+v err=%v", returned, err)
	}
	current.OwnershipEpoch = returned.OwnershipEpoch
	_ = taskCC.Close()

	messageSnowflake := "1783330000000000043"
	payload, _ := json.Marshal(map[string]interface{}{"id": messageSnowflake, "content": "second managed turn", "author": map[string]interface{}{"id": "U1", "bot": false}})
	envelope, _ := json.Marshal(sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload,
		InboxID: 43, SessionID: current.SessionID, ClaimToken: "claim-43", AttemptCount: 1})
	deliveryEntered := make(chan struct{}, 1)
	deliveryRelease := make(chan struct{})
	defer func() {
		select {
		case <-deliveryRelease:
		default:
			close(deliveryRelease)
		}
	}()
	fake.mu.Lock()
	fake.deliveryEntered = deliveryEntered
	fake.deliveryRelease = deliveryRelease
	fake.deliveryBlockContent = "fixture done"
	fake.mu.Unlock()
	watch.handleMessageCreate(envelope)
	select {
	case <-deliveryEntered:
	case <-time.After(5 * time.Second):
		summary, _ := terminal.ListSessions()
		t.Fatalf("managed final Discord delivery was not attempted: sessions=%+v messages=%v edits=%v finishes=%v", summary, fake.snapshotMessages(), fake.snapshotEdits(), fake.snapshotFinishes())
	}
	waiting, err = terminal.YieldSession(context.Background(), current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, true)
	if err != nil || waiting.Decision != model.YieldWaiting {
		t.Fatalf("integrated wait-yield while Discord delivery blocked=%+v err=%v", waiting, err)
	}
	blocked, err := terminal.ListSessions()
	blockedMessages := fake.snapshotMessages()
	finalStored := false
	for _, message := range blockedMessages {
		finalStored = finalStored || message["content"] == "fixture done"
	}
	if err != nil || len(blocked) != 1 || blocked[0].TaskState != model.TaskReplying || blocked[0].Writer == nil || blocked[0].Writer.Kind != model.OwnerCC || finalStored {
		t.Fatalf("blocked Discord delivery advanced state: sessions=%+v messages=%v err=%v", blocked, blockedMessages, err)
	}
	close(deliveryRelease)
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		finishes := fake.snapshotFinishes()
		if len(finishes) > 0 && finishes[len(finishes)-1]["status"] == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finishes := fake.snapshotFinishes(); len(finishes) == 0 || finishes[len(finishes)-1]["status"] != "completed" {
		summary, _ := terminal.ListSessions()
		t.Fatalf("managed durable inbox was not completed: finishes=%v sessions=%+v messages=%v edits=%v", finishes, summary, fake.snapshotMessages(), fake.snapshotEdits())
	}
	afterDelivery, err := terminal.ListSessions()
	if err != nil || len(afterDelivery) != 1 || afterDelivery[0].TaskState != model.TaskIdle || afterDelivery[0].Writer == nil || afterDelivery[0].Writer.Kind != model.OwnerTerminal || afterDelivery[0].OwnershipEpoch != current.OwnershipEpoch+1 {
		t.Fatalf("Discord delivery ACK did not transfer waiting owner: sessions=%+v err=%v", afterDelivery, err)
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
	// Simulate a crash after the delivery ACK transaction but before its
	// ownership-finalization phase. Inbox replay must resend the idempotent ACK
	// and finish the waiting yield rather than leaving the session replying.
	taskCC, err = duckliondaemon.DialCC(socket, "dwch_task")
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := taskCC.YieldSession(context.Background(), current.SessionID, current.OwnershipEpoch+1, current.RuntimeGeneration, false)
	if err != nil || reclaimed.Writer == nil || reclaimed.Writer.Kind != model.OwnerCC {
		t.Fatalf("reclaim CC for ACK recovery=%+v err=%v", reclaimed, err)
	}
	current.OwnershipEpoch = reclaimed.OwnershipEpoch
	recoverySnowflake := "1783330000000000044"
	recoveryPrompt := []byte("recover ACK finalization")
	recoveryTask := protocol.AgentTaskSubmit{TaskID: recoverySnowflake, Prompt: recoveryPrompt, PromptDigest: sha256.Sum256(recoveryPrompt)}
	if _, err := taskCC.SubmitAgentTask(context.Background(), recoveryTask.TaskID, current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, recoveryTask); err != nil {
		t.Fatal(err)
	}
	var recoverySequence uint64
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, pollErr := taskCC.AgentTaskEvents(context.Background(), current.SessionID, recoveryTask.TaskID, 0)
		if pollErr == nil && len(events.Events) != 0 && events.Events[len(events.Events)-1].Kind == "completed" {
			recoverySequence = events.Events[len(events.Events)-1].Sequence
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if recoverySequence == 0 {
		t.Fatal("recovery task did not complete")
	}
	if waiting, err = terminal.YieldSession(context.Background(), current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, true); err != nil || waiting.Decision != model.YieldWaiting {
		t.Fatalf("recovery waiting yield=%+v err=%v", waiting, err)
	}
	database, err := ducklionstore.Open(context.Background(), filepath.Join(configDir, "ducklion", "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AckManagedTaskEvent(context.Background(), model.SessionID(current.SessionID), recoveryTask.TaskID, recoverySequence, model.Owner{Kind: model.OwnerCC, ID: "dwch_task"}); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	_ = database.Close()
	replying, _ := terminal.ListSessions()
	if len(replying) != 1 || replying[0].TaskState != model.TaskReplying {
		t.Fatalf("simulated ACK/finalize crash state=%+v", replying)
	}
	finishCount := len(fake.snapshotFinishes())
	recoveryPayload, _ := json.Marshal(map[string]interface{}{"id": recoverySnowflake, "content": string(recoveryPrompt), "author": map[string]interface{}{"id": "U1", "bot": false}})
	recoveryEnvelope, _ := json.Marshal(sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: recoveryPayload,
		InboxID: 44, SessionID: current.SessionID, ClaimToken: "claim-44", AttemptCount: 2})
	watch.handleMessageCreate(recoveryEnvelope)
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		finishes := fake.snapshotFinishes()
		if len(finishes) > finishCount && finishes[len(finishes)-1]["status"] == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	recovered, err := terminal.ListSessions()
	if err != nil || len(recovered) != 1 || recovered[0].TaskState != model.TaskIdle || recovered[0].Writer == nil || recovered[0].Writer.Kind != model.OwnerTerminal || recovered[0].OwnershipEpoch != current.OwnershipEpoch+1 {
		t.Fatalf("ACK finalization replay did not recover: sessions=%+v finishes=%v err=%v", recovered, fake.snapshotFinishes(), err)
	}
	_ = taskCC.Close()
	// The waiting yield already returned ownership to this terminal; stop the
	// real PTY so the test leaves no supervisor.
	if err := terminal.StopSession(context.Background(), current.SessionID, current.OwnershipEpoch+1, current.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
}
