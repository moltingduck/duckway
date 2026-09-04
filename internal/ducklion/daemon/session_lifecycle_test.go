package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
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
	keyPath := filepath.Join(root, "sessions", created.SessionID, "recovery.key")
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("key mode=%v", keyInfo.Mode())
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
