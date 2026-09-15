package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklion/supervisor"
)

func TestHostRetentionRPCSweepsImmediatelyWithoutRestartingShell(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	defer stopRuntime()
	var runtimes sync.WaitGroup
	runtimeDone := make(chan struct{}, 2)
	server, err := Open(ctx, Options{Root: root, RuntimeLauncher: func(specPath string) error {
		runtimes.Add(1)
		go func() {
			defer runtimes.Done()
			_ = RunManagedSupervisor(runtimeCtx, specPath)
			runtimeDone <- struct{}{}
		}()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() {
		stopRuntime()
		runtimes.Wait()
		_ = server.Close()
		<-done
	}()
	client, err := DialDucklord(server.SocketPath(), "retention-live-owner", uuid.NewString(), uuid.NewString(), protocol.ConnectionControl)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	retired, err := client.CreateSession(ctx, protocol.SessionCreate{Handle: "retention-expired-fixture", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendInput(retired.SessionID, retired.OwnershipEpoch, retired.RuntimeGeneration, []byte("printf 'retention fixture\\n'\nexit\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtimeDone:
	case <-ctx.Done():
		t.Fatal("fixture shell did not exit")
	}
	waitFor := func(description string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatal(description)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("fixture shell was not retained", func() bool {
		_, err := server.state.GetRetainedShellSession(ctx, model.SessionID(retired.SessionID), retired.RuntimeGeneration)
		return err == nil
	})
	// Age the real, now-closed log between the old and new retention limits.
	logPath := supervisor.RetainedOutputPath(filepath.Join(root, "sessions", retired.SessionID), retired.RuntimeGeneration)
	metadata, err := os.ReadFile(logPath + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &fields); err != nil {
		t.Fatal(err)
	}
	fields["updated_at_ms"] = json.RawMessage(strconv.FormatInt(time.Now().Add(-4*24*time.Hour).UnixMilli(), 10))
	metadata, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath+".json", metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if server.retainedTTL() != 7*24*time.Hour {
		t.Fatalf("unexpected initial retention: %s", server.retainedTTL())
	}
	if err := server.cleanupExpiredRetainedOutput(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{logPath, logPath + ".json"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("seven-day retention removed fixture %s: %v", path, err)
		}
	}
	active, err := client.CreateSession(ctx, protocol.SessionCreate{Handle: "retention-surviving-shell", Kind: model.KindShell, CWD: root, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	readShellState := func(name, command string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := client.SendInput(active.SessionID, active.OwnershipEpoch, active.RuntimeGeneration, []byte(command+" > "+shellQuoteHook(path)+"\n")); err != nil {
			t.Fatal(err)
		}
		var state string
		waitFor("active shell did not respond: "+name, func() bool {
			data, err := os.ReadFile(path)
			state = strings.TrimSpace(string(data))
			return err == nil && strings.HasSuffix(state, ":retention-live-marker")
		})
		return state
	}
	before := readShellState("retention-before.pid", "RETENTION_TEST_MARKER=retention-live-marker; printf '%s:%s\\n' \"$$\" \"$RETENTION_TEST_MARKER\"")
	if pid, err := strconv.Atoi(strings.SplitN(before, ":", 2)[0]); err != nil || pid <= 0 {
		t.Fatalf("invalid shell PID: %q", before)
	}
	if err := client.SetHostLogRetention(ctx, 3); err != nil {
		t.Fatal(err)
	}
	// No manual sweep here: the RPC must wake the normally hourly sweeper.
	waitFor("retention RPC did not immediately sweep the expired log and metadata", func() bool {
		_, logErr := os.Stat(logPath)
		_, metaErr := os.Stat(logPath + ".json")
		return os.IsNotExist(logErr) && os.IsNotExist(metaErr)
	})
	after := readShellState("retention-after.pid", "printf '%s:%s\\n' \"$$\" \"$RETENTION_TEST_MARKER\"")
	if after != before {
		t.Fatalf("retention update replaced shell or lost shell state: before=%q after=%q", before, after)
	}
	sessions, err := client.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != active.SessionID || sessions[0].RuntimeGeneration != active.RuntimeGeneration || sessions[0].OwnershipEpoch != active.OwnershipEpoch || sessions[0].Status != model.StatusRunning {
		t.Fatalf("retention update changed active shell identity/state: created=%+v sessions=%+v", active, sessions)
	}
}
