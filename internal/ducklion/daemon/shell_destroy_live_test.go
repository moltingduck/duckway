package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklion/store"
)

func TestRunningShellDestroyTerminatesProcessAndBothViewers(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	defer stopRuntime()
	var runtimes sync.WaitGroup
	runtimeDone := make(chan error, 1)
	server, err := Open(ctx, Options{Root: root, RuntimeLauncher: func(specPath string) error {
		runtimes.Add(1)
		go func() {
			defer runtimes.Done()
			runtimeDone <- RunManagedSupervisor(runtimeCtx, specPath)
		}()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() {
		stopRuntime()
		runtimes.Wait()
		_ = server.Close()
		<-serveDone
	}()
	first, err := Dial(server.SocketPath(), "destroy-viewer-a")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Dial(server.SocketPath(), "destroy-viewer-b")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	created, err := first.CreateSession(ctx, protocol.SessionCreate{Handle: "destroy-live-fixture", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	streams := make([]*OutputSubscription, 0, 2)
	for _, client := range []*Client{first, second} {
		stream, err := client.SubscribeOutput(created.SessionID, created.RuntimeGeneration, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		streams = append(streams, stream)
	}
	pidPath := filepath.Join(root, "destroy-shell.pid")
	// The complete marker does not occur in input, so PTY echo cannot prove
	// either that the shell executed the command or both viewers saw output.
	const marker = "destroy-live-shell-ready"
	command := "printf '%s\\n' \"$$\" > " + shellQuoteHook(pidPath) + "; printf '%s%s\\n' 'destroy-live-' 'shell-ready'\n"
	if err := first.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte(command)); err != nil {
		t.Fatal(err)
	}
	for i, stream := range streams {
		var output bytes.Buffer
		for !bytes.Contains(output.Bytes(), []byte(marker)) {
			event, err := stream.ReadContext(ctx)
			if err != nil {
				t.Fatalf("viewer %d did not observe live shell output: %v", i, err)
			}
			output.Write(event.Frame.Data)
		}
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid shell PID %q: %v", pidBytes, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("shell was not alive before destroy: %v", err)
	}
	sessionDir := filepath.Join(root, "sessions", created.SessionID)
	logPath := filepath.Join(sessionDir, fmt.Sprintf("output.%d.log", created.RuntimeGeneration))
	if data, err := os.ReadFile(logPath); err != nil || !bytes.Contains(data, []byte(marker)) {
		t.Fatalf("live shell log missing before destroy: %q, %v", data, err)
	}
	destroy := protocol.SessionLifecycleRequest{Operation: protocol.SessionLifecycleDestroy, Mode: protocol.SessionLifecycleImmediate}
	result := awaitLifecycleTestResult(t, first, "destroy-live-shell", created, destroy, 8*time.Second)
	if result.State != protocol.SessionLifecycleCompleted {
		t.Fatalf("destroy did not complete: %+v", result)
	}
	select {
	case <-runtimeDone:
	case <-ctx.Done():
		t.Fatal("destroy did not stop the managed shell supervisor")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("destroyed root shell PID %d still exists: %v", pid, err)
	}
	for i, stream := range streams {
		for {
			_, err := stream.ReadContext(ctx)
			if err == nil {
				continue
			}
			var ended *OutputStreamEnded
			if !errors.As(err, &ended) {
				t.Fatalf("viewer %d did not receive explicit stream termination: %v", i, err)
			}
			if ended.Reason != "runtime_disconnected" {
				t.Fatalf("viewer %d ended for an unrelated reason: %s", i, ended.Reason)
			}
			break
		}
	}
	for i, client := range []*Client{first, second} {
		if sessions, err := client.ListSessions(); err != nil || len(sessions) != 0 {
			t.Fatalf("viewer %d inventory after destroy: %+v, %v", i, sessions, err)
		}
		if err := client.SendInput(created.SessionID, created.OwnershipEpoch, created.RuntimeGeneration, []byte("echo stale\n")); err == nil {
			t.Fatalf("viewer %d could write destroyed shell", i)
		}
		if stream, err := client.SubscribeOutput(created.SessionID, created.RuntimeGeneration, 0); err == nil {
			_ = stream.Close()
			t.Fatalf("viewer %d could resubscribe destroyed shell", i)
		}
	}
	// Unlike End or natural shell exit, explicit Destroy deletes logs too.
	if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroy retained runtime/log directory: %v", err)
	}
	if _, err := server.state.GetRetainedShellSession(ctx, model.SessionID(created.SessionID), created.RuntimeGeneration); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("destroy retained a diagnostic shell identity: %v", err)
	}
	if retained, err := first.ListRetainedShellsContext(ctx); err != nil || len(retained) != 0 {
		t.Fatalf("destroyed shell remains in retained RPC inventory: %+v, %v", retained, err)
	}
}
