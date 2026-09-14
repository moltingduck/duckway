package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

func TestForegroundVisibilityIsAdvisoryAndGenerationFenced(t *testing.T) {
	server, err := Open(context.Background(), Options{Root: t.TempDir()})
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
	now := time.Now().UnixMilli()
	session := model.Session{ID: "ABC123", Handle: "shell", Kind: model.KindShell, CWD: t.TempDir(), Status: model.StatusRecovering,
		RuntimeGeneration: 1, TaskState: model.TaskIdle, AdapterState: model.AdapterUnavailable, RecoveryPublicKey: publicKey,
		CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := server.service.CreateSession(context.Background(), "terminal:desk", "create-foreground", session); err != nil {
		t.Fatal(err)
	}
	runtimeClient, err := RegisterSupervisor(server.SocketPath(), session.ID, 1, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeClient.Close()
	controller := &fakeRuntimeController{inputs: make(chan duckruntime.InputFrame, 1), resize: make(chan [4]uint64, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlDone := make(chan error, 1)
	go func() { controlDone <- runtimeClient.ServeControl(ctx, controller) }()
	select {
	case <-runtimeClient.controlReady:
	case <-time.After(3 * time.Second):
		t.Fatal("control bridge did not become ready")
	}
	baseline, err := server.state.SessionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeClient.ReportForeground("shell"); err != nil {
		t.Fatal(err)
	}
	unchanged, err := server.state.SessionSnapshot(context.Background())
	if err != nil || unchanged.Revision != baseline.Revision {
		t.Fatalf("initial shell changed revision: %d -> %d, err=%v", baseline.Revision, unchanged.Revision, err)
	}
	if err := runtimeClient.ReportForeground("codex"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := server.state.SessionSnapshot(context.Background())
	if err != nil || snapshot.Revision != baseline.Revision+1 {
		t.Fatalf("codex revision=%d want=%d err=%v", snapshot.Revision, baseline.Revision+1, err)
	}
	projected := server.summariesFor(snapshot.Sessions)
	if len(projected) != 1 || projected[0].DetectedForeground != "codex" || projected[0].Kind != model.KindShell || projected[0].TaskState != model.TaskIdle {
		t.Fatalf("foreground projection changed canonical shell state: %+v", projected)
	}
	if err := runtimeClient.ReportForeground("codex"); err != nil {
		t.Fatal(err)
	}
	duplicate, err := server.state.SessionSnapshot(context.Background())
	if err != nil || duplicate.Revision != snapshot.Revision {
		t.Fatalf("duplicate changed revision: %d -> %d, err=%v", snapshot.Revision, duplicate.Revision, err)
	}
	if err := runtimeClient.ReportForeground("claude-invalid"); err == nil {
		t.Fatal("unknown foreground agent accepted")
	}
	if err := runtimeClient.ReportForeground("other_agent"); err != nil {
		t.Fatal(err)
	}
	other, err := server.state.SessionSnapshot(context.Background())
	if err != nil || other.Revision != baseline.Revision+2 {
		t.Fatalf("other agent revision=%d want=%d err=%v", other.Revision, baseline.Revision+2, err)
	}
	if projected := server.summariesFor(other.Sessions); len(projected) != 1 || projected[0].DetectedForeground != "other_agent" || projected[0].Kind != model.KindShell || projected[0].TaskState != model.TaskIdle {
		t.Fatalf("other agent changed canonical shell state: %+v", projected)
	}
	if err := runtimeClient.ReportForeground("shell"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = server.state.SessionSnapshot(context.Background())
	if err != nil || snapshot.Revision != baseline.Revision+3 {
		t.Fatalf("shell revision=%d want=%d err=%v", snapshot.Revision, baseline.Revision+3, err)
	}
	if got := server.summariesFor(snapshot.Sessions)[0].DetectedForeground; got != "" {
		t.Fatalf("shell fallback retained foreground label %q", got)
	}
	identity, ok := server.registry.Current(session.ID, 1)
	if !ok {
		t.Fatal("current supervisor disappeared")
	}
	stale := identity
	stale.Generation++
	if err := server.applyForeground(stale, "codex"); err != errForegroundStale {
		t.Fatalf("stale generation result=%v", err)
	}
	if got := server.summariesFor(snapshot.Sessions)[0].DetectedForeground; got != "" {
		t.Fatalf("stale report changed projection to %q", got)
	}
	cancel()
	_ = runtimeClient.Close()
	select {
	case <-controlDone:
	case <-time.After(3 * time.Second):
		t.Fatal("control bridge did not stop")
	}
}
