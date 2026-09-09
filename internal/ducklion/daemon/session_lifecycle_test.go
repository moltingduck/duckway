package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
	"github.com/hackerduck/duckway/internal/ducklion/store"
)

func TestCreateSessionStartsManagedPTYAndAcceptsInput(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	runtimeErrors := make(chan error, 1)
	launches := 0
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: func(specPath string) error {
		launches++
		go func() { runtimeErrors <- RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()

	client, err := Dial(server.SocketPath(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	createRequest := protocol.SessionCreate{Handle: "測試", Kind: model.KindAgent, AgentType: "shell", CWD: root, Command: []string{"sh"}, Rows: 30, Cols: 90}
	created, err := client.CreateSessionWithID(context.Background(), "stable-create", createRequest)
	if err != nil {
		t.Fatal(err)
	}
	if created.SessionID == "" || created.Status != model.StatusRunning || created.Writer == nil || created.Writer.ID != "laptop" {
		t.Fatalf("created=%+v", created)
	}
	replayed, err := client.CreateSessionWithID(context.Background(), "stable-create", createRequest)
	if err != nil || replayed.SessionID != created.SessionID || launches != 1 {
		t.Fatalf("replayed=%+v launches=%d err=%v", replayed, launches, err)
	}
	conflict := createRequest
	conflict.Handle = "different"
	if _, err := client.CreateSessionWithID(context.Background(), "stable-create", conflict); err == nil {
		t.Fatal("idempotency conflict accepted")
	}
	stream, err := client.SubscribeOutputTail(created.SessionID, created.RuntimeGeneration, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf managed-ready\\n\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var output bytes.Buffer
	for !bytes.Contains(output.Bytes(), []byte("managed-ready")) && time.Now().Before(deadline) {
		frame, readErr := stream.Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		output.Write(frame.Frame.Data)
	}
	if !bytes.Contains(output.Bytes(), []byte("managed-ready")) {
		t.Fatalf("output=%q", output.String())
	}
	if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf '\\a'\n")); err != nil {
		t.Fatal(err)
	}
	attentionDeadline := time.Now().Add(5 * time.Second)
	for {
		sessions, listErr := client.ListSessions()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(sessions) == 1 && sessions[0].ActivitySequences[model.NotificationTerminalAttention] == 1 {
			break
		}
		if time.Now().After(attentionDeadline) {
			t.Fatalf("terminal attention was not projected: %+v", sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
	keyPath := filepath.Join(root, "sessions", created.SessionID, "recovery.key")
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("key mode=%v", keyInfo.Mode())
	}
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "laptop"}
	if _, _, err := server.state.ReserveLifecycle(context.Background(), store.PendingLifecycle{SessionID: model.SessionID(created.SessionID), Operation: store.LifecycleEnd,
		Mode: store.LifecycleWait, Requester: owner, SourceEpoch: created.OwnershipEpoch, SourceGeneration: created.RuntimeGeneration, RequestID: "end-wait"}); err != nil {
		t.Fatal(err)
	}
	if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("must-not-arrive\n")); err == nil {
		t.Fatal("PTY input crossed lifecycle barrier")
	} else if remote, ok := err.(*RemoteError); !ok || remote.Detail.Code != protocol.ErrDraining {
		t.Fatalf("input barrier error=%v", err)
	}
	if err := client.Resize(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, 31, 91); err == nil {
		t.Fatal("PTY resize crossed lifecycle barrier")
	} else if remote, ok := err.(*RemoteError); !ok || remote.Detail.Code != protocol.ErrDraining {
		t.Fatalf("resize barrier error=%v", err)
	}
	if err := server.state.DeletePendingLifecycle(context.Background(), model.SessionID(created.SessionID)); err != nil {
		t.Fatal(err)
	}
	if err := client.StopSession(context.Background(), created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	sessions, err := client.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].Status != model.StatusStopped {
		t.Fatalf("stopped sessions=%+v err=%v", sessions, err)
	}
	if sessions[0].ExitSuccess == nil || *sessions[0].ExitSuccess || sessions[0].ExitReason == "" {
		t.Fatalf("missing forced-stop outcome: %+v", sessions[0])
	}
	if sessions[0].RetainedOutputBytes == 0 || sessions[0].RetainedOutputUntilMS <= time.Now().UnixMilli() {
		t.Fatalf("missing retained output summary: %+v", sessions[0])
	}
	retained, err := client.SubscribeOutputTail(created.SessionID, created.RuntimeGeneration, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var retainedBytes bytes.Buffer
	for {
		event, readErr := retained.Read()
		if readErr != nil {
			break
		}
		retainedBytes.Write(event.Frame.Data)
	}
	_ = retained.Close()
	if !bytes.Contains(retainedBytes.Bytes(), []byte("managed-ready")) {
		t.Fatalf("stopped retained output=%q", retainedBytes.String())
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery key remains: %v", err)
	}
	if err := server.cleanupExpiredRetainedOutput(context.Background(), time.Now().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubscribeOutputTail(created.SessionID, created.RuntimeGeneration, 1<<20); err == nil {
		t.Fatal("expired retained output remained subscribable")
	} else if !strings.Contains(err.Error(), string(protocol.ErrOutputUnavailable)) {
		t.Fatalf("expired output error=%v", err)
	}
}

func TestDuckwayCCCreatesAgentWithCCInitialOwner(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	cleanerCalls := 0
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: func(specPath string) error {
		go func() { _ = RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}, SessionCleaner: func(path string) error {
		cleanerCalls++
		if cleanerCalls == 1 {
			return errors.New("injected post-commit cleanup failure")
		}
		return os.RemoveAll(path)
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()

	cc, err := DialCC(server.SocketPath(), "dwch_task")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	request := protocol.SessionCreate{Handle: "專案", Kind: model.KindAgent, AgentType: "fixture", CWD: root,
		Command: []string{"sh", "-c", "while IFS= read -r line; do :; done"}}
	created, err := cc.CreateSessionWithID(context.Background(), "cc-create-1", request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != model.StatusRunning || created.Writer == nil || created.Writer.Kind != model.OwnerCC || created.Writer.ID != "dwch_task" {
		t.Fatalf("created=%+v", created)
	}
	if _, err := cc.BindDiscordSession(context.Background(), "cc-bind-1", created.SessionID, "dwch_task"); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.CreateSession(context.Background(), protocol.SessionCreate{Handle: "forbidden", Kind: model.KindShell, CWD: root, Command: []string{"sh"}}); err == nil {
		t.Fatal("Duckway CC created a shell session")
	}
	if _, err := cc.BeginTask(context.Background(), "cc-task-busy", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	if err := cc.StopSessionWithID(context.Background(), "cc-stop-busy", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err == nil {
		t.Fatal("plain stop interrupted an active task")
	} else if remote, ok := err.(*RemoteError); !ok || remote.Detail.Code != protocol.ErrTaskActive {
		t.Fatalf("busy stop error=%v, want task_active", err)
	}
	if sessions, err := cc.ListSessions(); err != nil || len(sessions) != 1 || sessions[0].Status != model.StatusRunning || sessions[0].TaskState != model.TaskRunning {
		t.Fatalf("busy stop changed session: sessions=%+v err=%v", sessions, err)
	}
	if _, err := cc.CompleteTask(context.Background(), "cc-task-complete", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	other, err := DialCC(server.SocketPath(), "dwch_other")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.StopSession(context.Background(), created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err == nil {
		t.Fatal("non-owner CC stopped agent session")
	}
	_ = other.Close()
	if err := cc.StopSessionWithID(context.Background(), "cc-stop-1", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	other, err = DialCC(server.SocketPath(), "dwch_other")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.DestroySessionWithID(context.Background(), "other-destroy", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err == nil {
		t.Fatal("non-owner CC destroyed stopped agent session")
	}
	_ = other.Close()
	if err := cc.UnbindDiscordWithID(context.Background(), "cc-unbind-1", created.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := cc.UnbindDiscordWithID(context.Background(), "cc-unbind-1", created.SessionID); err != nil {
		t.Fatalf("unbind replay: %v", err)
	}
	if err := cc.DestroySessionWithID(context.Background(), "cc-destroy-1", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err == nil {
		t.Fatal("post-commit cleanup failure was hidden")
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", created.SessionID)); err != nil {
		t.Fatalf("fixture did not preserve files across injected cleanup failure: %v", err)
	}
	if err := cc.ReplayDestroySessionWithID(context.Background(), "cc-destroy-1", created.SessionID); err != nil {
		t.Fatalf("fenceless post-delete cleanup replay: %v", err)
	}
	sessions, err := cc.ListSessions()
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions after destroy=%+v err=%v", sessions, err)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", created.SessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroyed runtime directory remains: %v", err)
	}
}

func TestCanonicalLifecycleWaitDrainsBeforeEnd(t *testing.T) {
	root := t.TempDir()
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
	cc, err := DialCC(server.SocketPath(), "dwch_lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	created, err := cc.CreateSession(context.Background(), protocol.SessionCreate{Handle: "drain", Kind: model.KindAgent, AgentType: "fixture", CWD: root,
		Command: []string{"sh", "-c", "while :; do sleep 1; done"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.BindDiscordSession(context.Background(), "bind-drain", created.SessionID, "dwch_lifecycle"); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.BeginTask(context.Background(), "active-turn", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	immediate := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleImmediate}
	if _, err := cc.LifecycleSessionWithID(context.Background(), "end-now", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, immediate); err == nil {
		t.Fatal("immediate lifecycle accepted an active task")
	} else if remote, ok := err.(*RemoteError); !ok || remote.Detail.Code != protocol.ErrTaskActive {
		t.Fatalf("immediate lifecycle error=%v", err)
	}
	if pending, err := server.state.GetPendingLifecycle(context.Background(), model.SessionID(created.SessionID)); err != nil || pending != nil {
		t.Fatalf("immediate rejection left barrier=%+v err=%v", pending, err)
	}
	waitRequest := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleWait}
	result, err := cc.LifecycleSessionWithID(context.Background(), "end-wait", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, waitRequest)
	if err != nil || result.State != protocol.SessionLifecycleWaiting {
		t.Fatalf("wait lifecycle=%+v err=%v", result, err)
	}
	if _, err := cc.BeginTask(context.Background(), "overtake", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err == nil {
		t.Fatal("task crossed lifecycle drain")
	} else if remote, ok := err.(*RemoteError); !ok || remote.Detail.Code != protocol.ErrDraining {
		t.Fatalf("drain rejection=%v", err)
	}
	if _, err := cc.CompleteTask(context.Background(), "finish-active", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		result, err = cc.LifecycleSessionWithID(context.Background(), "end-wait", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, waitRequest)
		if err == nil && result.State == protocol.SessionLifecycleCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || result.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("wait lifecycle did not complete: result=%+v err=%v", result, err)
	}
	sessions, err := cc.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].Status != model.StatusStopped {
		t.Fatalf("ended session=%+v err=%v", sessions, err)
	}
	// Completion releases the lifecycle barrier while retaining an immutable
	// receipt, so the same stopped session can subsequently be destroyed.
	destroy := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleDestroy, Mode: protocol.SessionLifecycleImmediate}
	destroyResult, err := cc.LifecycleSessionWithID(context.Background(), "destroy-after-end", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, destroy)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for destroyResult.State != protocol.SessionLifecycleCompleted && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		destroyResult, err = cc.LifecycleSessionWithID(context.Background(), "destroy-after-end", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, destroy)
		if err != nil {
			t.Fatal(err)
		}
	}
	if destroyResult.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("destroy after end=%+v", destroyResult)
	}
	replayedEnd, err := cc.LifecycleSessionWithID(context.Background(), "end-wait", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, waitRequest)
	if err != nil || replayedEnd.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("end receipt after destroy=%+v err=%v", replayedEnd, err)
	}
}

func TestCanonicalLifecycleDestroyReplaysAfterSessionDeletion(t *testing.T) {
	root := t.TempDir()
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
	cc, err := DialCC(server.SocketPath(), "dwch_destroy")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	created, err := cc.CreateSession(context.Background(), protocol.SessionCreate{Handle: "destroy", Kind: model.KindAgent, AgentType: "fixture", CWD: root,
		Command: []string{"sh", "-c", "while :; do sleep 1; done"}})
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleDestroy, Mode: protocol.SessionLifecycleImmediate}
	result, err := cc.LifecycleSessionWithID(context.Background(), "destroy-canonical", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for result.State != protocol.SessionLifecycleCompleted && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		result, err = cc.LifecycleSessionWithID(context.Background(), "destroy-canonical", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("destroy result=%+v", result)
	}
	if sessions, err := cc.ListSessions(); err != nil || len(sessions) != 0 {
		t.Fatalf("destroyed sessions=%+v err=%v", sessions, err)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", created.SessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroyed runtime directory remains: %v", err)
	}
	replayed, err := cc.LifecycleSessionWithID(context.Background(), "destroy-canonical", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
	if err != nil || replayed.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("destroy replay=%+v err=%v", replayed, err)
	}
}

func TestCanonicalLifecycleForceRestartCancelsAndFencesOldRuntimeEvent(t *testing.T) {
	root := t.TempDir()
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
	cc, err := DialCC(server.SocketPath(), "dwch_force")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	created, err := cc.CreateSession(context.Background(), protocol.SessionCreate{Handle: "force", Kind: model.KindAgent, AgentType: "fixture", CWD: root,
		Command: []string{"sh", "-c", "while IFS= read -r line; do sleep 30; done"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.BindDiscordSession(context.Background(), "bind-force", created.SessionID, "dwch_force"); err != nil {
		t.Fatal(err)
	}
	prompt := []byte("long task")
	task := protocol.AgentTaskSubmit{TaskID: "force-task", Prompt: prompt, PromptDigest: sha256.Sum256(prompt)}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = cc.SubmitAgentTask(context.Background(), task.TaskID, created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, task)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("submit active task: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	request := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleRestart, Mode: protocol.SessionLifecycleForce}
	result, err := cc.LifecycleSessionWithID(context.Background(), "force-restart", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
	if err != nil {
		t.Fatal(err)
	}
	var cancelled protocol.SupervisorAgentEvent
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		events, pollErr := cc.AgentTaskEvents(context.Background(), created.SessionID, task.TaskID, 0)
		if pollErr == nil && len(events.Events) == 1 {
			cancelled = events.Events[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cancelled.Kind != "failed" || cancelled.Summary != "Task cancelled by forced session lifecycle operation" {
		t.Fatalf("forced terminal event=%+v", cancelled)
	}
	// The runtime exit and lifecycle completion remain behind Discord's
	// delivery acknowledgement, so a forced restart cannot silently lose notice.
	if sessions, listErr := cc.ListSessions(); listErr != nil || len(sessions) != 1 || sessions[0].TaskState != model.TaskReplying {
		t.Fatalf("pre-ack sessions=%+v err=%v", sessions, listErr)
	}
	if err := cc.AckAgentTaskEvent(context.Background(), created.SessionID, task.TaskID, cancelled.Sequence); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for result.State != protocol.SessionLifecycleCompleted && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		result, err = cc.LifecycleSessionWithID(context.Background(), "force-restart", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("force lifecycle result=%+v", result)
	}
	if result.RuntimeGeneration != created.RuntimeGeneration+1 {
		t.Fatalf("force restart generation=%d", result.RuntimeGeneration)
	}
	beforeLateEvent, err := server.state.SessionSnapshot(context.Background())
	if err != nil || len(beforeLateEvent.Sessions) != 1 {
		t.Fatalf("pre-stale-event snapshot=%+v err=%v", beforeLateEvent, err)
	}
	oldIdentity := duckruntime.RuntimeIdentity{SessionID: model.SessionID(created.SessionID), Generation: created.RuntimeGeneration, LeaseID: "delayed-old-runtime"}
	stale := server.applySupervisorAgentEvent(oldIdentity, protocol.SupervisorAgentEvent{TaskID: "delayed-old-task", Sequence: 1, Kind: "failed", Summary: "must be fenced"})
	if stale == nil || stale.Code != protocol.ErrStaleGeneration {
		t.Fatalf("late generation-%d event result=%+v", created.RuntimeGeneration, stale)
	}
	afterLateEvent, err := server.state.SessionSnapshot(context.Background())
	writerChanged := len(afterLateEvent.Sessions) == 1 && ((afterLateEvent.Sessions[0].Session.Writer == nil) != (beforeLateEvent.Sessions[0].Session.Writer == nil) ||
		afterLateEvent.Sessions[0].Session.Writer != nil && *afterLateEvent.Sessions[0].Session.Writer != *beforeLateEvent.Sessions[0].Session.Writer)
	if err != nil || afterLateEvent.Revision != beforeLateEvent.Revision || len(afterLateEvent.Sessions) != 1 ||
		afterLateEvent.Sessions[0].Session.RuntimeGeneration != created.RuntimeGeneration+1 ||
		afterLateEvent.Sessions[0].Session.TaskState != beforeLateEvent.Sessions[0].Session.TaskState ||
		writerChanged {
		t.Fatalf("stale event mutated replacement: before=%+v after=%+v err=%v", beforeLateEvent, afterLateEvent, err)
	}
}

func TestCanonicalLifecycleWaitResumesAfterDucklionRestart(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	launcher := func(specPath string) error {
		go func() { _ = RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	cc, err := DialCC(server.SocketPath(), "dwch_restart")
	if err != nil {
		t.Fatal(err)
	}
	created, err := cc.CreateSession(context.Background(), protocol.SessionCreate{Handle: "restart", Kind: model.KindAgent, AgentType: "fixture", CWD: root,
		Command: []string{"sh", "-c", "while :; do sleep 1; done"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.BindDiscordSession(context.Background(), "bind-restart", created.SessionID, "dwch_restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.BeginTask(context.Background(), "busy-before-restart", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	request := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleWait}
	if result, err := cc.LifecycleSessionWithID(context.Background(), "wait-across-restart", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request); err != nil || result.State != protocol.SessionLifecycleWaiting {
		t.Fatalf("initial lifecycle=%+v err=%v", result, err)
	}
	_ = cc.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}

	server, err = Open(context.Background(), Options{Root: root, RuntimeLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	serveDone = make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	cc, err = DialCC(server.SocketPath(), "dwch_restart")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	reconnectDeadline := time.Now().Add(5 * time.Second)
	for {
		sessions, listErr := cc.ListSessions()
		if listErr == nil && len(sessions) == 1 && sessions[0].Status == model.StatusRunning {
			break
		}
		if time.Now().After(reconnectDeadline) {
			t.Fatalf("supervisor did not reconnect: sessions=%+v err=%v", sessions, listErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := cc.CompleteTask(context.Background(), "complete-after-restart", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	var result protocol.SessionLifecycleResult
	for time.Now().Before(deadline) {
		result, err = cc.LifecycleSessionWithID(context.Background(), "wait-across-restart", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
		if err == nil && result.State == protocol.SessionLifecycleCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || result.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("recovered lifecycle=%+v err=%v", result, err)
	}
}

func TestCanonicalLifecycleRestartPreservesBindingAndAdvancesGeneration(t *testing.T) {
	root := t.TempDir()
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
	cc, err := DialCC(server.SocketPath(), "dwch_restart_agent")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	created, err := cc.CreateSession(context.Background(), protocol.SessionCreate{Handle: "restart-agent", Kind: model.KindAgent, AgentType: "fixture", CWD: root,
		Command: []string{os.Args[0], "-test.run=^TestAgentHookFixtureProcess$", "--", "agent-hook-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.BindDiscordSession(context.Background(), "bind-restart-agent", created.SessionID, "dwch_restart_agent"); err != nil {
		t.Fatal(err)
	}
	request := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleRestart, Mode: protocol.SessionLifecycleWait}
	result, err := cc.LifecycleSessionWithID(context.Background(), "restart-agent-once", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for result.State != protocol.SessionLifecycleCompleted && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		result, err = cc.LifecycleSessionWithID(context.Background(), "restart-agent-once", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.State != protocol.SessionLifecycleCompleted || result.RuntimeGeneration != created.RuntimeGeneration+1 {
		t.Fatalf("restart result=%+v", result)
	}
	sessions, err := cc.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].Status != model.StatusRunning || sessions[0].RuntimeGeneration != created.RuntimeGeneration+1 ||
		sessions[0].Writer == nil || sessions[0].Writer.ID != "dwch_restart_agent" {
		t.Fatalf("restarted sessions=%+v err=%v", sessions, err)
	}
	binding, err := cc.DiscordBindingForSession(context.Background(), created.SessionID)
	if err != nil || binding.ChannelHandle != "dwch_restart_agent" {
		t.Fatalf("restart binding=%+v err=%v", binding, err)
	}
	prompt := []byte("prove replacement runtime")
	task := protocol.AgentTaskSubmit{TaskID: "after-restart", Prompt: prompt, PromptDigest: sha256.Sum256(prompt)}
	if _, err := cc.SubmitAgentTask(context.Background(), task.TaskID, created.SessionID, created.OwnershipEpoch, result.RuntimeGeneration, task); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, pollErr := cc.AgentTaskEvents(context.Background(), created.SessionID, task.TaskID, 0)
		if pollErr == nil && len(events.Events) == 1 && events.Events[0].Response == "after restart" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("replacement runtime did not execute an agent task")
}

func TestAgentHookFixtureProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "agent-hook-fixture" {
		return
	}
	reader := bufio.NewScanner(os.Stdin)
	for reader.Scan() {
		conn, err := net.DialTimeout("unix", os.Getenv("DUCKLION_AGENT_EVENT_SOCKET"), time.Second)
		if err != nil {
			os.Exit(2)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		envelope := map[string]any{"token": os.Getenv("DUCKLION_AGENT_EVENT_TOKEN"), "event": map[string]string{"kind": "completed", "response": "after restart"}}
		if json.NewEncoder(conn).Encode(envelope) != nil {
			_ = conn.Close()
			os.Exit(2)
		}
		ack := make([]byte, 3)
		if _, err = io.ReadFull(conn, ack); err != nil || string(ack) != "ok\n" {
			_ = conn.Close()
			os.Exit(2)
		}
		_ = conn.Close()
	}
	os.Exit(0)
}

func TestShellLifecycleRestartEndAndDestroy(t *testing.T) {
	root := t.TempDir()
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
	terminal, err := Dial(server.SocketPath(), "shell-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if _, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "shell-with-args", Kind: model.KindShell, CWD: root, Command: []string{"sh", "-c", "echo must-not-persist"}}); err == nil || !strings.Contains(err.Error(), "exactly one shell executable") {
		t.Fatalf("shell argv admission err=%v", err)
	}
	created, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "shell-lifecycle", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := terminal.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf 'generation-one-only\\n'\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := terminal.LifecycleSessionWithID(context.Background(), "shell-wait-rejected", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration,
		protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleWait}); err == nil || !strings.Contains(err.Error(), "immediate") {
		t.Fatalf("shell wait lifecycle err=%v", err)
	}
	restart := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleRestart, Mode: protocol.SessionLifecycleImmediate}
	restarted := awaitLifecycleTestResult(t, terminal, "shell-restart", created, restart, 10*time.Second)
	if restarted.RuntimeGeneration != created.RuntimeGeneration+1 {
		t.Fatalf("shell restart=%+v", restarted)
	}
	current := created
	current.RuntimeGeneration = restarted.RuntimeGeneration
	if err := terminal.SendInput(current.SessionID, current.OwnershipEpoch, current.RuntimeGeneration, []byte("printf 'generation-two-only\\n'\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	end := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleImmediate}
	ended := awaitLifecycleTestResult(t, terminal, "shell-end", current, end, 8*time.Second)
	if ended.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("shell end=%+v", ended)
	}
	retained, err := terminal.SubscribeOutputTail(current.SessionID, current.RuntimeGeneration, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for {
		event, readErr := retained.Read()
		if readErr != nil {
			break
		}
		output.Write(event.Frame.Data)
	}
	_ = retained.Close()
	if !bytes.Contains(output.Bytes(), []byte("generation-two-only")) || bytes.Contains(output.Bytes(), []byte("generation-one-only")) {
		t.Fatalf("current-generation retained output=%q", output.String())
	}
	sessionDir := filepath.Join(root, "sessions", current.SessionID)
	if _, err := os.Stat(filepath.Join(sessionDir, "output.1.log")); err != nil {
		t.Fatalf("generation-one diagnostic output missing: %v", err)
	}
	destroy := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleDestroy, Mode: protocol.SessionLifecycleImmediate}
	destroyed := awaitLifecycleTestResult(t, terminal, "shell-destroy", current, destroy, 8*time.Second)
	if destroyed.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("shell destroy=%+v", destroyed)
	}
	if sessions, err := terminal.ListSessions(); err != nil || len(sessions) != 0 {
		t.Fatalf("shell sessions after destroy=%+v err=%v", sessions, err)
	}
	if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroy retained session directory: %v", err)
	}
}

func TestShellConcurrentDucklordAttachmentsAreWritable(t *testing.T) {
	root := t.TempDir()
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

	first, err := Dial(server.SocketPath(), "desk-a")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	created, err := first.CreateSession(context.Background(), protocol.SessionCreate{Handle: "shared-shell", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Dial(server.SocketPath(), "desk-b")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	firstOutput, err := first.SubscribeOutput(created.SessionID, created.RuntimeGeneration, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer firstOutput.Close()
	secondOutput, err := second.SubscribeOutput(created.SessionID, created.RuntimeGeneration, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer secondOutput.Close()

	start := make(chan struct{})
	writeErrors := make(chan error, 2)
	go func() {
		<-start
		writeErrors <- first.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf 'from-desk-a\\n'\n"))
	}()
	go func() {
		<-start
		writeErrors <- second.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf 'from-desk-b\\n'\n"))
	}()
	close(start)
	for range 2 {
		if err := <-writeErrors; err != nil {
			t.Fatalf("concurrent shell input: %v", err)
		}
	}

	readBoth := func(label string, stream *OutputSubscription) <-chan error {
		done := make(chan error, 1)
		go func() {
			var output bytes.Buffer
			for !bytes.Contains(output.Bytes(), []byte("from-desk-a")) || !bytes.Contains(output.Bytes(), []byte("from-desk-b")) {
				event, readErr := stream.Read()
				if readErr != nil {
					done <- fmt.Errorf("%s output %q: %w", label, output.String(), readErr)
					return
				}
				output.Write(event.Frame.Data)
			}
			done <- nil
		}()
		return done
	}
	for label, done := range map[string]<-chan error{"desk-a": readBoth("desk-a", firstOutput), "desk-b": readBoth("desk-b", secondOutput)} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s attachment did not observe both writers", label)
		}
	}

	end := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleImmediate}
	ended := awaitLifecycleTestResult(t, second, "shared-shell-end", created, end, 8*time.Second)
	if ended.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("shell end from second writer=%+v", ended)
	}
}

func TestShellLifecycleRestartLaunchFailureStopsAndReleasesBarrier(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	var launches atomic.Int32
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: func(specPath string) error {
		attempt := launches.Add(1)
		if attempt == 2 {
			return errors.New("injected definitive restart launch failure")
		}
		go func() { _ = RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	terminal, err := Dial(server.SocketPath(), "restart-retry")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	created, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "restart-retry", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleRestart, Mode: protocol.SessionLifecycleImmediate}
	result, err := terminal.LifecycleSessionWithID(context.Background(), "restart-fails", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
	if err != nil || result.State != protocol.SessionLifecycleWaiting {
		t.Fatalf("initial restart result=%+v err=%v", result, err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, err = terminal.LifecycleSessionWithID(context.Background(), "restart-fails", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
		if err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err == nil || !strings.Contains(err.Error(), "definitive restart launch failure") {
		t.Fatalf("restart failure receipt err=%v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if launches.Load() != 2 {
		t.Fatalf("definitive failure retried: launches=%d", launches.Load())
	}
	sessions, err := terminal.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].Status != model.StatusStopped || sessions[0].RuntimeGeneration != 2 {
		t.Fatalf("failed replacement session=%+v err=%v", sessions, err)
	}
	destroy := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleDestroy, Mode: protocol.SessionLifecycleImmediate}
	destroyed := awaitLifecycleTestResult(t, terminal, "destroy-after-restart-failure", sessions[0], destroy, 8*time.Second)
	if destroyed.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("destroy after failure=%+v", destroyed)
	}
}

func TestRealSupervisorReportsMissingShellOnRestart(t *testing.T) {
	root := t.TempDir()
	shellPath := filepath.Join(root, "test-shell")
	if err := os.WriteFile(shellPath, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	var launches atomic.Int32
	runtimeErrors := make(chan error, 16)
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: func(specPath string) error {
		launches.Add(1)
		go func() { runtimeErrors <- RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	terminal, err := Dial(server.SocketPath(), "missing-shell")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	created, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "missing-shell", Kind: model.KindShell, CWD: root, Command: []string{shellPath}})
	if err != nil {
		t.Fatal(err)
	}
	end := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleImmediate}
	ended := awaitLifecycleTestResult(t, terminal, "missing-shell-end", created, end, 8*time.Second)
	// The stopped generation may finish its wrapper just after Ducklion commits
	// the exit receipt. Wait for its process lock so this test isolates the
	// replacement executable failure rather than the normal overlap retry.
	lockPath := filepath.Join(root, "sessions", created.SessionID, "runtime.lock")
	lockDeadline := time.Now().Add(8 * time.Second)
	for {
		lock, lockErr := acquireRuntimeLock(lockPath)
		if lockErr == nil {
			_ = lock.Close()
			break
		}
		if time.Now().After(lockDeadline) {
			t.Fatalf("stopped runtime retained process lock: %v", lockErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	for len(runtimeErrors) != 0 {
		<-runtimeErrors
	}
	if err := os.Remove(shellPath); err != nil {
		t.Fatal(err)
	}
	restartSession := created
	restartSession.RuntimeGeneration = ended.RuntimeGeneration
	restart := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleRestart, Mode: protocol.SessionLifecycleImmediate}
	result, err := terminal.LifecycleSessionWithID(context.Background(), "missing-shell-restart", restartSession.SessionID, restartSession.OwnershipEpoch, restartSession.RuntimeGeneration, restart)
	if err != nil || result.State != protocol.SessionLifecycleWaiting {
		t.Fatalf("initial restart result=%+v err=%v", result, err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, err = terminal.LifecycleSessionWithID(context.Background(), "missing-shell-restart", restartSession.SessionID, restartSession.OwnershipEpoch, restartSession.RuntimeGeneration, restart)
		if err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err == nil || !strings.Contains(err.Error(), "runtime launch failed") {
		debugSessions, _ := terminal.ListSessions()
		pending, _ := server.state.GetPendingLifecycle(context.Background(), model.SessionID(created.SessionID))
		specDebug, _ := os.ReadFile(filepath.Join(root, "sessions", created.SessionID, "runtime.json"))
		var runtimeErrs []string
		for len(runtimeErrors) != 0 {
			runtimeErrs = append(runtimeErrs, (<-runtimeErrors).Error())
		}
		t.Fatalf("missing shell failure receipt err=%v sessions=%+v pending=%+v launches=%d runtimeErrs=%v spec=%s", err, debugSessions, pending, launches.Load(), runtimeErrs, specDebug)
	}
	time.Sleep(1200 * time.Millisecond)
	if launches.Load() != 2 {
		t.Fatalf("missing shell caused restart storm: launches=%d", launches.Load())
	}
	sessions, err := terminal.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].Status != model.StatusStopped || sessions[0].RuntimeGeneration != 2 {
		t.Fatalf("missing shell session=%+v err=%v", sessions, err)
	}
	destroy := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleDestroy, Mode: protocol.SessionLifecycleImmediate}
	_ = awaitLifecycleTestResult(t, terminal, "missing-shell-destroy", sessions[0], destroy, 8*time.Second)
}

func awaitLifecycleTestResult(t *testing.T, client *Client, requestID string, session protocol.SessionSummary, request protocol.SessionLifecycleRequest, timeout time.Duration) protocol.SessionLifecycleResult {
	t.Helper()
	result, err := client.LifecycleSessionWithID(context.Background(), requestID, session.SessionID, session.OwnershipEpoch, session.RuntimeGeneration, request)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(timeout)
	for result.State != protocol.SessionLifecycleCompleted && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		result, err = client.LifecycleSessionWithID(context.Background(), requestID, session.SessionID, session.OwnershipEpoch, session.RuntimeGeneration, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("lifecycle %s timed out: %+v", requestID, result)
	}
	return result
}

func TestManagedPTYDrainsFinalAttentionBeforeExit(t *testing.T) {
	root := t.TempDir()
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
	client, err := Dial(server.SocketPath(), "desk")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	created, err := client.CreateSession(context.Background(), protocol.SessionCreate{Handle: "final-attention", Kind: model.KindAgent, AgentType: "shell", CWD: root,
		Command: []string{"sh", "-c", `printf '\a'; sleep 0.2`}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != model.StatusRunning && created.Status != model.StatusStopped {
		t.Fatalf("created=%+v", created)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		sessions, listErr := client.ListSessions()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(sessions) == 1 && sessions[0].Status == model.StatusStopped {
			if sessions[0].ActivitySequences[model.NotificationTerminalAttention] != 1 {
				t.Fatalf("final attention missing before stopped snapshot: %+v", sessions)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not stop: %+v", sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRetainedOutputPeriodicCleanupRemovesCrashOrphanWithoutBlockingDaemon(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	server, err := Open(context.Background(), Options{Root: root, RetainedOutputTTL: time.Second, RuntimeLauncher: func(specPath string) error {
		go func() { _ = RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	client, err := Dial(server.SocketPath(), "retention-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	created, err := client.CreateSession(context.Background(), protocol.SessionCreate{Handle: "retention", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf retained-crash-orphan; exit\n")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", created.SessionID, "output.1.log")
	metaPath := path + ".json"
	deadline := time.Now().Add(5 * time.Second)
	for {
		sessions, listErr := client.ListSessions()
		if listErr == nil && len(sessions) == 1 && sessions[0].Status == model.StatusStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime did not stop: %+v err=%v", sessions, listErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Model the sidecar state left by a supervisor crash. Cleanup must fall
	// back to the securely opened log's mtime instead of retaining it forever.
	if err := os.WriteFile(path, []byte("crash-tail"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".output-compact-crash", ".output-meta-crash"} {
		tempPath := filepath.Join(filepath.Dir(path), name)
		if err := os.WriteFile(tempPath, []byte("temporary-sensitive-output"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(tempPath, old, old); err != nil {
			t.Fatal(err)
		}
	}
	server.requestRetainedOutputSweep()
	for {
		_, logErr := os.Stat(path)
		_, metaErr := os.Stat(metaPath)
		_, compactErr := os.Stat(filepath.Join(filepath.Dir(path), ".output-compact-crash"))
		_, metaTempErr := os.Stat(filepath.Join(filepath.Dir(path), ".output-meta-crash"))
		if errors.Is(logErr, os.ErrNotExist) && errors.Is(metaErr, os.ErrNotExist) && errors.Is(compactErr, os.ErrNotExist) && errors.Is(metaTempErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic cleanup left orphan log=%v meta=%v compact=%v meta-temp=%v", logErr, metaErr, compactErr, metaTempErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := client.ListSessions(); err != nil {
		t.Fatalf("cleanup made daemon unavailable: %v", err)
	}
}

func TestManagedPTYPersistsAcrossDaemonRestart(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	launcher := func(specPath string) error {
		go func() { _ = RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	client, err := Dial(server.SocketPath(), "desk")
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateSession(context.Background(), protocol.SessionCreate{Handle: "survivor", Kind: model.KindAgent, AgentType: "shell", CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}

	server, err = Open(context.Background(), Options{Root: root, RuntimeLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	serveDone = make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		client, err = Dial(server.SocketPath(), "desk")
		if err == nil {
			sessions, listErr := client.ListSessions()
			if listErr == nil && len(sessions) == 1 && sessions[0].Status == model.StatusRunning {
				created = sessions[0]
				break
			}
			_ = client.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime did not recover: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer client.Close()
	stream, err := client.SubscribeOutputTail(created.SessionID, created.RuntimeGeneration, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("printf survived-restart\\n\nexit\n")); err != nil {
		t.Fatal(err)
	}
	for {
		event, readErr := stream.Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(event.Frame.Data, []byte("survived-restart")) {
			break
		}
	}
}

func TestManagedPTYReportsExitAfterDaemonReturns(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	launcher := func(specPath string) error {
		go func() { _ = RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}
	server, err := Open(context.Background(), Options{Root: root, RuntimeLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	client, err := Dial(server.SocketPath(), "desk")
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateSession(context.Background(), protocol.SessionCreate{Handle: "short", Kind: model.KindAgent, AgentType: "shell", CWD: root,
		Command: []string{"sh", "-c", "sleep 0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	_ = server.Close()
	<-serveDone
	time.Sleep(350 * time.Millisecond)
	server, err = Open(context.Background(), Options{Root: root, RuntimeLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	serveDone = make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		viewer, dialErr := Dial(server.SocketPath(), "viewer")
		if dialErr == nil {
			sessions, listErr := viewer.ListSessions()
			_ = viewer.Close()
			if listErr == nil && len(sessions) == 1 && sessions[0].Status == model.StatusStopped {
				if sessions[0].ExitSuccess == nil || !*sessions[0].ExitSuccess || sessions[0].ExitReason != "" {
					t.Fatalf("exit outcome=%+v", sessions[0])
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s exit was not recovered", created.SessionID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	keyPath := filepath.Join(root, "sessions", created.SessionID, "recovery.key")
	for {
		_, err := os.Stat(keyPath)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery key remains: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
