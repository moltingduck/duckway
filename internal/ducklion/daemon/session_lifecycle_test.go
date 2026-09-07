package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
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
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery key remains: %v", err)
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

func TestCanonicalLifecycleForceCancelsAndFencesAgentTask(t *testing.T) {
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
	request := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleEnd, Mode: protocol.SessionLifecycleForce}
	result, err := cc.LifecycleSessionWithID(context.Background(), "force-end", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
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
	// delivery acknowledgement, so a forced stop cannot silently lose notice.
	if sessions, listErr := cc.ListSessions(); listErr != nil || len(sessions) != 1 || sessions[0].TaskState != model.TaskReplying {
		t.Fatalf("pre-ack sessions=%+v err=%v", sessions, listErr)
	}
	if err := cc.AckAgentTaskEvent(context.Background(), created.SessionID, task.TaskID, cancelled.Sequence); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for result.State != protocol.SessionLifecycleCompleted && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		result, err = cc.LifecycleSessionWithID(context.Background(), "force-end", created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("force lifecycle result=%+v", result)
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
