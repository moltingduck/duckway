package ducklord

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/service"
	"github.com/hackerduck/duckway/internal/ducklion/store"
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
