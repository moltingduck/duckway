package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

func TestOldOutputTeardownCannotDowngradeReconnectedControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server, err := Open(ctx, Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveDone }()
	publicKey, privateKey, err := model.NewRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: "ABC123", Handle: "teardown", Kind: model.KindShell, CWD: t.TempDir(), Status: model.StatusRecovering,
		OwnershipEpoch: 1, RuntimeGeneration: 1, TaskState: model.TaskIdle, AdapterState: model.AdapterUnavailable,
		RecoveryPublicKey: publicKey, CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	if _, _, err := server.service.CreateSession(ctx, "terminal:teardown-owner", "create", session); err != nil {
		t.Fatal(err)
	}
	old, err := RegisterSupervisor(server.SocketPath(), session.ID, 1, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	identity := duckruntime.RuntimeIdentity{SessionID: session.ID, Generation: 1, LeaseID: old.Identity().LeaseID}
	if err := server.state.MarkRuntimeConnected(ctx, session.ID, 1); err != nil {
		t.Fatal(err)
	}

	// Pause the actual teardown implementation after removing its registry
	// lease but before its durable status write. A control disconnect can
	// independently make recovery registration eligible during this window.
	server.foregroundMu.Lock()
	locked := true
	teardownDone := make(chan struct{})
	defer func() {
		if locked {
			server.foregroundMu.Unlock()
		}
		<-teardownDone
	}()
	go func() { server.disconnectSupervisor(identity); close(teardownDone) }()
	for server.registry.IsCurrent(identity) {
		select {
		case <-ctx.Done():
			t.Fatal("old lease was not removed")
		case <-time.After(time.Millisecond):
		}
	}
	if err := server.state.MarkRuntimeDisconnected(ctx, session.ID, 1); err != nil {
		t.Fatal(err)
	}
	replacement, err := RegisterSupervisor(server.SocketPath(), session.ID, 1, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if replacement.Identity().LeaseID == identity.LeaseID {
		t.Fatal("replacement reused old lease")
	}
	controlCtx, stopControl := context.WithCancel(ctx)
	controlDone := make(chan error, 1)
	go func() { controlDone <- replacement.ServeControl(controlCtx, &fakeRuntimeController{}) }()
	defer func() { stopControl(); <-controlDone }()
	select {
	case <-replacement.controlReady:
	case <-ctx.Done():
		t.Fatal("replacement control did not register")
	}
	server.foregroundMu.Unlock()
	locked = false
	select {
	case <-teardownDone:
	case <-ctx.Done():
		t.Fatal("old teardown did not finish")
	}
	current, err := server.state.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.StatusRunning || current.RuntimeGeneration != 1 || current.OwnershipEpoch != 1 {
		t.Fatalf("old output teardown downgraded replacement: %+v", current)
	}
}
