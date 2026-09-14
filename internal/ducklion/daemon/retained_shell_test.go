package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklion/store"
)

func TestRetiredShellOutputIsFencedAndSwept(t *testing.T) {
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
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() { _ = server.Close(); <-done }()
	client, err := Dial(server.SocketPath(), "retired-log-reader")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	created, err := client.CreateSession(context.Background(), protocol.SessionCreate{Handle: "retired-shell", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration,
		[]byte("printf 'RETIRED_SHELL_OUTPUT\\n'\nexit\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		sessions, listErr := client.ListSessions()
		if listErr == nil && len(sessions) == 0 {
			if _, err := server.state.GetRetainedShellSession(context.Background(), model.SessionID(created.SessionID), created.RuntimeGeneration); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("root shell did not retire: sessions=%+v err=%v", sessions, listErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := client.SubscribeOutputTail(created.SessionID, created.RuntimeGeneration+1, 1<<20); err == nil || !strings.Contains(err.Error(), string(protocol.ErrNotFound)) {
		t.Fatalf("stale generation read retired output: %v", err)
	}
	stream, err := client.SubscribeOutputTail(created.SessionID, created.RuntimeGeneration, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for {
		frame, readErr := stream.Read()
		if readErr != nil {
			break
		}
		output.Write(frame.Frame.Data)
	}
	_ = stream.Close()
	if !bytes.Contains(output.Bytes(), []byte("RETIRED_SHELL_OUTPUT")) {
		t.Fatalf("retired log missing marker: %q", output.String())
	}
	if err := server.cleanupExpiredRetainedOutput(context.Background(), time.Now().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubscribeOutputTail(created.SessionID, created.RuntimeGeneration, 1<<20); err == nil {
		t.Fatal("expired retired output remained subscribable")
	}
	if _, err := server.state.GetRetainedShellSession(context.Background(), model.SessionID(created.SessionID), created.RuntimeGeneration); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired retired shell index survived: %v", err)
	}
}
