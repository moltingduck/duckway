package ducklord

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/service"
	"github.com/hackerduck/duckway/internal/ducklion/store"
	"github.com/hackerduck/duckway/internal/ducklion/supervisor"
)

func TestMain(m *testing.M) {
	if os.Getenv("DUCKLORD_TEST_BRIDGE_HELPER") == "1" {
		err := daemon.BridgeStdio(context.Background(), os.Getenv("DUCKLORD_TEST_SOCKET"), os.Stdin, os.Stdout)
		if err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunnerAttachUsesMultiplexedBridgeForOutputAndInput(t *testing.T) {
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
		Command: []string{"sh", "-c", `IFS= read -r value; printf 'received:%s\n' "$value"`}, OutputCapacity: 1 << 20})
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
	deadline := time.Now().Add(time.Second)
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
	if err := ptySession.Wait(); err != nil {
		t.Fatal(err)
	}
	var captured []byte
	deadline = time.Now().Add(time.Second)
	for !bytes.Contains(captured, []byte("received:hello")) {
		select {
		case chunk := <-output:
			captured = append(captured, chunk...)
		case <-time.After(time.Until(deadline)):
			t.Fatalf("attach output timed out: %q", captured)
		}
	}
	cancel()
	_ = attach.Stdin.Close()
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
