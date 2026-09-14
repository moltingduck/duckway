package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

func TestShellRootExitRetiresSelectableSessionAndKeepsDiagnosticIndex(t *testing.T) {
	ctx := context.Background()
	server, err := Open(ctx, Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() { _ = server.Close(); <-done }()
	publicKey, privateKey, err := model.NewRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	session := model.Session{ID: "ABC123", Handle: "shell", Kind: model.KindShell, CWD: t.TempDir(), Status: model.StatusRecovering,
		RuntimeGeneration: 1, TaskState: model.TaskIdle, AdapterState: model.AdapterUnavailable, RecoveryPublicKey: publicKey,
		CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := server.service.CreateSession(ctx, "terminal:desk", "create-shell-exit", session); err != nil {
		t.Fatal(err)
	}
	client, err := RegisterSupervisor(server.SocketPath(), session.ID, 1, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	controller := &fakeRuntimeController{inputs: make(chan duckruntime.InputFrame, 1), resize: make(chan [4]uint64, 1)}
	controlCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = client.ServeControl(controlCtx, controller) }()
	select {
	case <-client.controlReady:
	case <-time.After(3 * time.Second):
		t.Fatal("control bridge not ready")
	}
	before, err := server.state.SessionSnapshot(ctx)
	if err != nil || len(before.Sessions) != 1 {
		t.Fatalf("before exit snapshot=%+v err=%v", before, err)
	}
	if terminal, err := runtimeExitTerminal(ctx, runtimeSpec{SocketPath: server.SocketPath(), SessionID: session.ID, RuntimeGeneration: 1}); err != nil || terminal {
		t.Fatalf("live runtime wrongly terminal=%t err=%v", terminal, err)
	}
	if err := client.ReportExit(true, "root shell exited"); err != nil {
		t.Fatal(err)
	}
	after, err := server.state.SessionSnapshot(ctx)
	if err != nil || len(after.Sessions) != 0 || after.Revision <= before.Revision {
		t.Fatalf("retired shell remains selectable: %+v err=%v", after, err)
	}
	retained, err := server.state.GetRetainedShellSession(ctx, session.ID, 1)
	if err != nil || retained.Handle != session.Handle || !retained.ExitSuccess {
		t.Fatalf("diagnostic record=%+v err=%v", retained, err)
	}
	if terminal, err := runtimeExitTerminal(ctx, runtimeSpec{SocketPath: server.SocketPath(), SessionID: session.ID, RuntimeGeneration: 1}); err != nil || !terminal {
		t.Fatalf("retired runtime not terminal=%t err=%v", terminal, err)
	}
	observer, err := DialDucklord(server.SocketPath(), "diagnostics", "01996f85-a769-7c16-9ab0-4127531fd001", "01996f85-a769-7c16-9ab0-4127531fd002", protocol.ConnectionObserver)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	items, err := observer.ListRetainedShellsContext(ctx)
	if err != nil || len(items) != 1 || items[0].SessionID != string(session.ID) {
		t.Fatalf("Ducklord retained list=%+v err=%v", items, err)
	}
	cc, err := DialCC(server.SocketPath(), "cc-channel")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	response, err := cc.Call(protocol.Request{ID: "retained-cc-denied", Type: "retained.list"})
	if err != nil || response.Error == nil {
		t.Fatalf("Discord CC accessed retained diagnostics: %+v err=%v", response, err)
	}
	if err := client.ReportExit(true, "root shell exited"); err != nil {
		t.Fatalf("duplicate exit receipt failed: %v", err)
	}
	replayed, err := server.state.SessionSnapshot(ctx)
	if err != nil || replayed.Revision != after.Revision {
		t.Fatalf("duplicate exit mutated inventory: %+v err=%v", replayed, err)
	}
}
