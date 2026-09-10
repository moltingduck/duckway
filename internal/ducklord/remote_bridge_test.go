package ducklord

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklion/service"
	"github.com/hackerduck/duckway/internal/ducklion/store"
	"github.com/hackerduck/duckway/internal/ducklion/supervisor"
)

func TestNormalizeNativeShellLifecycleMode(t *testing.T) {
	tests := []struct {
		name      string
		kind      model.SessionKind
		operation protocol.SessionLifecycleOperation
		mode      protocol.SessionLifecycleMode
		want      protocol.SessionLifecycleMode
		wantErr   bool
	}{
		{name: "shell restart default", kind: model.KindShell, operation: protocol.SessionLifecycleRestart, mode: protocol.SessionLifecycleWait, want: protocol.SessionLifecycleImmediate},
		{name: "shell end immediate", kind: model.KindShell, operation: protocol.SessionLifecycleEnd, mode: protocol.SessionLifecycleImmediate, want: protocol.SessionLifecycleImmediate},
		{name: "shell destroy wait rejected", kind: model.KindShell, operation: protocol.SessionLifecycleDestroy, mode: protocol.SessionLifecycleWait, wantErr: true},
		{name: "shell restart force rejected", kind: model.KindShell, operation: protocol.SessionLifecycleRestart, mode: protocol.SessionLifecycleForce, wantErr: true},
		{name: "agent restart wait preserved", kind: model.KindAgent, operation: protocol.SessionLifecycleRestart, mode: protocol.SessionLifecycleWait, want: protocol.SessionLifecycleWait},
		{name: "agent restart force preserved", kind: model.KindAgent, operation: protocol.SessionLifecycleRestart, mode: protocol.SessionLifecycleForce, want: protocol.SessionLifecycleForce},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeLifecycleMode(test.kind, test.operation, test.mode)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("mode=%q err=%v", got, err)
			}
		})
	}
}

func TestRunnerProjectBrowserRejectsUnsafeInputBeforeSSH(t *testing.T) {
	runner := NewRunner()
	client := Client{Name: "host", Host: "unused", SSH: "/definitely/not/ssh"}
	for _, path := range []string{"relative", "/tmp/line\nbreak", "/tmp/hidden\u202ename", strings.Repeat("x", 4097)} {
		if _, err := runner.AddProject(context.Background(), client, path, "name"); err == nil || strings.Contains(err.Error(), "fork/exec") {
			t.Fatalf("path %q reached SSH or was accepted: %v", path, err)
		}
	}
	for _, query := range []string{"line\nbreak", "hidden\u202ename", strings.Repeat("x", 4097)} {
		if _, err := runner.SuggestProjectPaths(context.Background(), client, query); err == nil || strings.Contains(err.Error(), "fork/exec") {
			t.Fatalf("query %q reached SSH or was accepted: %v", query, err)
		}
	}
}

func TestRunnerSelectedMutationsRejectChangedDucklionInstance(t *testing.T) {
	root := t.TempDir()
	server, err := daemon.Open(context.Background(), daemon.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	t.Setenv("DUCKLORD_TEST_BRIDGE_HELPER", "1")
	t.Setenv("DUCKLORD_TEST_SOCKET", server.SocketPath())
	runner := NewRunner()
	defer runner.Close()
	runner.SetOwner("desk-a")
	client := Client{Name: "local", Host: "ignored", SSH: os.Args[0], Ducklion: "ignored"}
	selected := RemoteSession{Client: "local", InstanceID: "different-instance", SessionID: "ABC123", Kind: string(model.KindAgent), OwnershipEpoch: 1, RuntimeGeneration: 1}

	if _, err := runner.LifecycleSelected(context.Background(), client, selected, protocol.SessionLifecycleDestroy, protocol.SessionLifecycleImmediate); err == nil || !strings.Contains(err.Error(), "host instance changed") {
		t.Fatalf("lifecycle instance fence error=%v", err)
	}
	if _, err := runner.YieldSelected(context.Background(), client, selected, false); err == nil || !strings.Contains(err.Error(), "host instance changed") {
		t.Fatalf("yield instance fence error=%v", err)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("DUCKLORD_TEST_BRIDGE_HELPER") == "1" {
		var output io.Writer = os.Stdout
		if marker := os.Getenv("DUCKLORD_TEST_DROP_CREATE_RESPONSE"); marker != "" {
			output = &dropCommittedCreateResponse{output: os.Stdout, marker: marker}
		}
		err := daemon.BridgeStdio(context.Background(), os.Getenv("DUCKLORD_TEST_SOCKET"), os.Stdin, output)
		if err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// dropCommittedCreateResponse is a subprocess-only bridge fault injector. It
// forwards complete protocol frames until the first session.create result,
// then consumes that response and terminates the stdio bridge. The marker's
// O_EXCL creation makes the fault happen once across reconnecting helpers.
type dropCommittedCreateResponse struct {
	output io.Writer
	marker string
	buffer []byte
}

func (w *dropCommittedCreateResponse) Write(data []byte) (int, error) {
	w.buffer = append(w.buffer, data...)
	for len(w.buffer) >= 4 {
		size := int(binary.BigEndian.Uint32(w.buffer[:4]))
		if size <= 0 || size > 1<<20 {
			return 0, errors.New("invalid injected bridge frame")
		}
		if len(w.buffer) < 4+size {
			break
		}
		frame, payload := w.buffer[:4+size], w.buffer[4:4+size]
		var response struct {
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(payload, &response)
		var created struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(response.Result, &created)
		if created.SessionID != "" {
			marker, err := os.OpenFile(w.marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err == nil {
				_ = marker.Close()
				w.buffer = w.buffer[4+size:]
				return len(data), io.ErrClosedPipe
			}
		}
		if _, err := w.output.Write(frame); err != nil {
			return 0, err
		}
		w.buffer = w.buffer[4+size:]
	}
	return len(data), nil
}

func TestRunnerOutputSubscriptionLimitIsProcessWide(t *testing.T) {
	runner := NewRunner()
	defer runner.Close()
	if err := runner.SetOutputSubscriptionLimit(1); err != nil {
		t.Fatal(err)
	}
	release, err := runner.acquireOutputSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.SetOutputSubscriptionLimit(2); err == nil {
		t.Fatal("changed output limit while a slot was active")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := runner.acquireOutputSlot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second slot error=%v", err)
	}
	release()
	if err := runner.SetOutputSubscriptionLimit(2); err == nil {
		t.Fatal("changed output limit after subscriptions had started")
	}
	second, err := runner.acquireOutputSlot(context.Background())
	if err != nil {
		t.Fatalf("released slot was not reusable: %v", err)
	}
	second()
}

func TestRunnerStartReplaysCommittedMutationAfterBridgeDisconnect(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	var launches atomic.Int32
	server, err := daemon.Open(context.Background(), daemon.Options{Root: root, RuntimeLauncher: func(specPath string) error {
		launches.Add(1)
		go func() { _ = daemon.RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()

	marker := filepath.Join(t.TempDir(), "drop-once")
	t.Setenv("DUCKLORD_TEST_BRIDGE_HELPER", "1")
	t.Setenv("DUCKLORD_TEST_SOCKET", server.SocketPath())
	t.Setenv("DUCKLORD_TEST_DROP_CREATE_RESPONSE", marker)
	runner := NewRunner()
	defer runner.Close()
	runner.SetOwner("desk-a")
	client := Client{Name: "local", Host: "ignored", SSH: os.Args[0], Ducklion: "ignored"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessionID, err := runner.Start(ctx, client, []string{"--name", "response-loss", "--agent", "fixture", "--cwd", root, "--", "sh", "-c", "while IFS= read -r line; do :; done"})
	if err != nil {
		t.Fatalf("replay create after response loss: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("bridge fault was not exercised: %v", err)
	}
	if launches.Load() != 1 {
		t.Fatalf("supervisor launches=%d, want exactly one", launches.Load())
	}
	sessions, err := runner.Sessions(ctx, client, 8)
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != sessionID || sessions[0].Status != string(model.StatusRunning) {
		t.Fatalf("sessions=%+v id=%q err=%v", sessions, sessionID, err)
	}
}

func TestRunnerYieldTransfersCCSessionAndWaitsThroughBridge(t *testing.T) {
	root := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	server, err := daemon.Open(context.Background(), daemon.Options{Root: root, RuntimeLauncher: func(specPath string) error {
		go func() { _ = daemon.RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()

	cc, err := daemon.DialCC(server.SocketPath(), "task-channel")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	created, err := cc.CreateSessionWithID(context.Background(), "cross-surface-create", protocol.SessionCreate{Handle: "cross-surface", Kind: model.KindAgent,
		AgentType: "fixture", CWD: root, Command: []string{"sh", "-c", "while IFS= read -r line; do printf 'agent:%s\\n' \"$line\"; done"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("DUCKLORD_TEST_BRIDGE_HELPER", "1")
	t.Setenv("DUCKLORD_TEST_SOCKET", server.SocketPath())
	runner := NewRunner()
	defer runner.Close()
	runner.SetOwner("desk-a")
	remote := Client{Name: "local", Host: "ignored", SSH: os.Args[0], Ducklion: "ignored"}
	attachCtx, cancelAttach := context.WithCancel(context.Background())
	defer cancelAttach()
	attachment, err := runner.AttachStream(attachCtx, remote, created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Stdout.Close()
	outputChunks := make(chan []byte, 32)
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, readErr := attachment.Stdout.Read(buffer)
			if n > 0 {
				outputChunks <- append([]byte(nil), buffer[:n]...)
			}
			if readErr != nil {
				close(outputChunks)
				return
			}
		}
	}()
	if err := runner.Send(context.Background(), remote, created.SessionID, "must-not-reach-agent"); err == nil || !strings.Contains(err.Error(), string(protocol.ErrNotOwner)) {
		t.Fatalf("non-owner bridge input error=%v", err)
	}
	if control, err := runner.OpenControlSession(context.Background(), remote, created.SessionID); control != nil || err == nil || !strings.Contains(err.Error(), "read-only session") {
		t.Fatalf("non-owner control=%v error=%v", control, err)
	}
	transferred, err := runner.Yield(context.Background(), remote, created.SessionID, false)
	if err != nil || transferred.Decision != model.YieldTransferred || transferred.Writer == nil || transferred.Writer.ID != "desk-a" || transferred.OwnershipEpoch != 2 {
		t.Fatalf("Ducklord immediate yield=%+v err=%v", transferred, err)
	}
	returned, err := cc.YieldSessionWithID(context.Background(), "cc-return", created.SessionID, 2, created.RuntimeGeneration, false)
	if err != nil || returned.Decision != model.YieldTransferred || returned.Writer == nil || returned.Writer.Kind != model.OwnerCC || returned.OwnershipEpoch != 3 {
		t.Fatalf("CC return yield=%+v err=%v", returned, err)
	}
	if _, err := cc.BeginTask(context.Background(), "cross-surface-task", created.SessionID, 3, created.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	waiting, err := runner.Yield(context.Background(), remote, created.SessionID, true)
	if err != nil || waiting.Decision != model.YieldWaiting || waiting.OwnershipEpoch != 3 || waiting.Writer == nil || waiting.Writer.Kind != model.OwnerCC {
		t.Fatalf("Ducklord waiting yield=%+v err=%v", waiting, err)
	}
	completed, err := cc.CompleteTask(context.Background(), "cross-surface-complete", created.SessionID, 3, created.RuntimeGeneration)
	if err != nil || completed.OwnershipEpoch != 4 || completed.Writer == nil || completed.Writer.ID != "desk-a" || completed.TaskState != model.TaskIdle {
		t.Fatalf("waiting yield completion=%+v err=%v", completed, err)
	}
	control, err := runner.OpenControlSession(context.Background(), remote, created.SessionID)
	if err != nil {
		t.Fatalf("new owner control: %v", err)
	}
	if _, err := control.Stdin.Write([]byte("accepted-after-wait\r")); err != nil {
		t.Fatalf("new owner input: %v", err)
	}
	defer control.Stdin.Close()
	var observed bytes.Buffer
	deadline := time.After(5 * time.Second)
	for !bytes.Contains(observed.Bytes(), []byte("agent:accepted-after-wait")) {
		select {
		case chunk, ok := <-outputChunks:
			if !ok {
				t.Fatalf("attachment ended before allowed input: %q", observed.String())
			}
			observed.Write(chunk)
		case <-deadline:
			t.Fatalf("attachment did not observe allowed input: %q", observed.String())
		}
	}
	if bytes.Contains(observed.Bytes(), []byte("must-not-reach-agent")) {
		t.Fatalf("non-owner input reached agent PTY: %q", observed.String())
	}
}

func TestTerminalSubmitLineUsesPTYEnter(t *testing.T) {
	for _, test := range []struct {
		text string
		want string
	}{{"prompt", "prompt\r"}, {"", "\r"}} {
		if got := string(terminalSubmitLine(test.text)); got != test.want {
			t.Fatalf("terminalSubmitLine(%q) = %q, want %q", test.text, got, test.want)
		}
	}
}

func TestRunnerSplitOutputAndControlUseMultiplexedBridge(t *testing.T) {
	root := t.TempDir()
	database, err := store.Open(context.Background(), filepath.Join(root, "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := model.NewRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk-a"}
	sessionModel := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", CWD: root, Status: model.StatusRecovering,
		Writer: &owner, OwnershipEpoch: 1, RuntimeGeneration: 1, TaskState: model.TaskIdle, AdapterState: model.AdapterRecovering,
		RecoveryPublicKey: publicKey, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := service.New(database).CreateSession(context.Background(), "terminal:desk-a", "seed-attach", sessionModel); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	server, err := daemon.Open(context.Background(), daemon.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	ptySession, err := supervisor.Start(supervisor.Options{SessionID: sessionModel.ID, RuntimeGeneration: 1, OwnershipEpoch: 1, CWD: root,
		Command: []string{"sh", "-c", `IFS= read -r value; printf '\033[32mreceived:%s\033[0m\n' "$value"; IFS= read -r value; printf '\033[36mresumed:%s\033[0m\n' "$value"`}, OutputCapacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	runtimeClient, err := daemon.RegisterSupervisor(server.SocketPath(), sessionModel.ID, 1, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	forwardDone := make(chan error, 1)
	controlDone := make(chan error, 1)
	go func() { forwardDone <- runtimeClient.ForwardOutput(ctx, ptySession.Output()) }()
	go func() { controlDone <- runtimeClient.ServeControl(ctx, ptySession) }()
	t.Setenv("DUCKLORD_TEST_BRIDGE_HELPER", "1")
	t.Setenv("DUCKLORD_TEST_SOCKET", server.SocketPath())
	runner := NewRunner()
	runner.SetOwner("desk-a")
	clientConfig := Client{Name: "local", Host: "ignored", SSH: os.Args[0], Ducklion: "ignored"}
	var attach *AttachSession
	deadline := time.Now().Add(5 * time.Second)
	for {
		attach, err = runner.AttachStream(ctx, clientConfig, "ABC123")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attach.Stdin.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	output := make(chan []byte, 16)
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, readErr := attach.Stdout.Read(buffer)
			if n > 0 {
				output <- append([]byte(nil), buffer[:n]...)
			}
			if readErr != nil {
				close(output)
				return
			}
		}
	}()
	var captured []byte
	deadline = time.Now().Add(5 * time.Second)
	for !bytes.Contains(captured, []byte("received:hello")) {
		select {
		case chunk := <-output:
			captured = append(captured, chunk...)
		case <-time.After(time.Until(deadline)):
			t.Fatalf("attach output timed out: %q", captured)
		}
	}
	resumeOffset := attach.OutputOffset()
	_ = attach.Stdin.Close()
	_ = attach.Stdout.Close()
	select {
	case <-attach.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("first attach did not close")
	}
	resumed, err := runner.AttachStreamFrom(ctx, clientConfig, "ABC123", AttachResume{RuntimeGeneration: 1, OutputOffset: resumeOffset})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.ExactResume || resumed.StartOffset != resumeOffset || resumed.ReplayEndOffset != resumeOffset {
		t.Fatalf("resume exact=%v offsets=%d..%d want=%d", resumed.ExactResume, resumed.StartOffset, resumed.ReplayEndOffset, resumeOffset)
	}
	control, err := runner.OpenControlSession(ctx, clientConfig, "ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if control.InstanceID != string(server.InstanceID()) || control.SessionID != "ABC123" || control.OwnershipEpoch != 1 || control.RuntimeGeneration != 1 {
		t.Fatalf("control identity=%+v", control)
	}
	pool, err := NewOutputPool(1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	outputKey := OutputKey{ClientKey: "local", InstanceID: string(server.InstanceID()), SessionID: "ABC123"}
	outputRevision := OutputRevision{RuntimeGeneration: 1}
	snapshotStore := SnapshotStore{Root: filepath.Join(t.TempDir(), "snapshots")}
	var pooled *PooledTerminal
	if _, err := pool.Activate(ctx, outputKey, outputRevision, func(openCtx context.Context, key OutputKey, revision OutputRevision, _ uint64) (OutputResource, error) {
		outputOnly, openErr := runner.OpenOutputStreamFrom(openCtx, clientConfig, "ABC123", AttachResume{RuntimeGeneration: 1, OutputOffset: 0})
		if openErr != nil {
			return nil, openErr
		}
		pooled, openErr = NewPooledTerminal(openCtx, outputOnly, PooledTerminalOptions{ExpectedKey: key, ExpectedRevision: revision,
			Rows: 4, Cols: 80, Scrollback: 10, Store: snapshotStore})
		return pooled, openErr
	}); err != nil {
		t.Fatal(err)
	}
	initialView := pooled.View()
	initialTerminal, ok := NewTerminalFromState(initialView.Framebuffer, 10)
	initialRendered := ""
	if ok {
		initialRendered = strings.Join(initialTerminal.RenderLines(4, 80), "\n")
	}
	if !ok || initialView.OutputOffset != resumeOffset || !strings.Contains(initialRendered, "\x1b[0;32mreceived:hello") {
		t.Fatalf("pooled initial replay offset=%d want=%d rendered=%q", initialView.OutputOffset, resumeOffset, initialRendered)
	}
	if _, err := control.Stdin.Write([]byte("again\r")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		view := pooled.View()
		framebuffer, valid := NewTerminalFromState(view.Framebuffer, 10)
		rendered := ""
		if valid {
			rendered = strings.Join(framebuffer.RenderLines(4, 80), "\n")
		}
		if strings.Contains(rendered, "\x1b[0;36mresumed:again") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pooled live framebuffer offset=%d rendered=%q", view.OutputOffset, rendered)
		}
		time.Sleep(time.Millisecond)
	}
	if err := pooled.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := snapshotStore.Load(outputKey.InstanceID, outputKey.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	savedState, err := DecodeTerminalRenderState(saved.Payload)
	if err != nil || savedState.Framebuffer == nil {
		t.Fatalf("saved pooled framebuffer err=%v state=%+v", err, savedState)
	}
	savedTerminal, valid := NewTerminalFromState(*savedState.Framebuffer, 10)
	savedRendered := ""
	if valid {
		savedRendered = strings.Join(savedTerminal.RenderLines(4, 80), "\n")
	}
	if !valid || savedState.OutputOffset != pooled.View().OutputOffset || !strings.Contains(savedRendered, "\x1b[0;36mresumed:again") {
		t.Fatalf("saved pooled state offset=%d rendered=%q", savedState.OutputOffset, savedRendered)
	}
	var resumedOutput bytes.Buffer
	readBuffer := make([]byte, 4096)
	deadline = time.Now().Add(5 * time.Second)
	for !bytes.Contains(resumedOutput.Bytes(), []byte("resumed:again")) {
		n, readErr := resumed.Stdout.Read(readBuffer)
		if n > 0 {
			_, _ = resumedOutput.Write(readBuffer[:n])
		}
		if readErr != nil && readErr != io.EOF {
			t.Fatal(readErr)
		}
		if readErr == io.EOF || time.Now().After(deadline) {
			t.Fatalf("resumed output=%q", resumedOutput.Bytes())
		}
	}
	if err := ptySession.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := pooled.Close(); err != nil {
		t.Fatal(err)
	}
	_ = resumed.Stdin.Close()
	_ = resumed.Stdout.Close()
	_ = control.Stdin.Close()
	select {
	case err := <-control.Done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control session did not close")
	}
	if sessions, listErr := runner.Sessions(context.Background(), clientConfig, 8); listErr != nil || len(sessions) != 1 || sessions[0].SessionID != "ABC123" {
		t.Fatalf("closing isolated observer disrupted control bridge: sessions=%+v err=%v", sessions, listErr)
	}
	cancel()
	_ = runner.Close()
	_ = runtimeClient.Close()
	_ = server.Close()
	<-serveDone
	select {
	case <-forwardDone:
	case <-time.After(time.Second):
		t.Fatal("output forwarder did not stop")
	}
	select {
	case <-controlDone:
	case <-time.After(time.Second):
		t.Fatal("control server did not stop")
	}
}

func TestRunnerListsSessionsThroughStdioBridge(t *testing.T) {
	root := t.TempDir()
	database, err := store.Open(context.Background(), filepath.Join(root, "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk-a"}
	want := model.Session{ID: "ABC123", Handle: "build", Kind: model.KindAgent, AgentType: "codex", CWD: "/work", Status: model.StatusStopped,
		Writer: &owner, OwnershipEpoch: 3, RuntimeGeneration: 7, TaskState: model.TaskIdle, AdapterState: model.AdapterUnavailable, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := service.New(database).CreateSession(context.Background(), "terminal:desk-a", "seed", want); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	server, err := daemon.Open(context.Background(), daemon.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	t.Setenv("DUCKLORD_TEST_BRIDGE_HELPER", "1")
	t.Setenv("DUCKLORD_TEST_SOCKET", server.SocketPath())
	runner := NewRunner()
	defer runner.Close()
	runner.SetOwner("desk-a")
	client := Client{Name: "local", Host: "ignored", SSH: os.Args[0], Ducklion: "ignored"}
	sessions, err := runner.Sessions(context.Background(), client, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "ABC123" || sessions[0].Name != "build" || sessions[0].WriterID != "desk-a" ||
		sessions[0].OwnershipEpoch != 3 || sessions[0].RuntimeGeneration != 7 {
		t.Fatalf("sessions = %+v", sessions)
	}
	if _, err := runner.ReadPreview(context.Background(), client, "ABC123", 10); err == nil || !strings.Contains(err.Error(), string(protocol.ErrOutputUnavailable)) {
		t.Fatalf("observer preview admission/error=%v", err)
	}
	if sessions, err = runner.Sessions(context.Background(), client, 8); err != nil || len(sessions) != 1 {
		t.Fatalf("control bridge after observer preview sessions=%+v err=%v", sessions, err)
	}
	runner.SetOwner("desk-b")
	if _, err := runner.Sessions(context.Background(), client, 8); err != nil {
		t.Fatalf("reconnect with changed owner: %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(server.SocketPath()), "ducklion.db")); err != nil {
		t.Fatal(err)
	}
}
