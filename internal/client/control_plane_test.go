package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/controlplane"
	"github.com/hackerduck/duckway/internal/version"
)

func TestControlHeartbeatPersistsAndStartsUpdateOnce(t *testing.T) {
	var starts atomic.Int32
	var statusCalls atomic.Int32
	command := controlplane.Command{
		ID: "job-1", Type: controlplane.CommandUpdateRestart, TargetVersion: "future-version",
		Binary: "duckway-client-" + runtime.GOOS + "-" + runtime.GOARCH, SHA256: strings.Repeat("a", 64),
		Size: 2 << 20, LeaseToken: "lease", LeaseExpiresAt: "later", Attempt: 1,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/control/heartbeat":
			_ = json.NewEncoder(w).Encode(controlplane.HeartbeatResponse{NextHeartbeatSeconds: 30, Command: &command})
		case "/client/control/jobs/job-1/status":
			statusCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldStart := startManagedUpdateProcess
	startManagedUpdateProcess = func(_ string, _ string, got controlplane.Command, _ *controlStateStore) error {
		if got.ID != command.ID {
			t.Fatalf("command = %+v", got)
		}
		starts.Add(1)
		return nil
	}
	t.Cleanup(func() { startManagedUpdateProcess = oldStart })

	dir := t.TempDir()
	cfg := &Config{ServerURL: server.URL, Token: "token"}
	store := &controlStateStore{path: filepath.Join(dir, "control-state.json")}
	api := NewAPIClient(server.URL, "token")
	if _, err := controlHeartbeatOnce(context.Background(), dir, cfg, "proxy", "boot", api, store, true); err != nil {
		t.Fatal(err)
	}
	if _, err := controlHeartbeatOnce(context.Background(), dir, cfg, "proxy", "boot", api, store, true); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || statusCalls.Load() != 1 {
		t.Fatalf("starts=%d status=%d", starts.Load(), statusCalls.Load())
	}
	state, err := store.load()
	if err != nil || state == nil || state.Command.ID != "job-1" || state.Status != controlplane.JobRunning {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestControlCommandRejectsArbitraryBinary(t *testing.T) {
	err := validateControlCommand(controlplane.Command{ID: "j", Type: controlplane.CommandUpdateRestart,
		TargetVersion: "v", Binary: "sh", SHA256: strings.Repeat("a", 64), Size: 2 << 20, LeaseToken: "l", Attempt: 1})
	if err == nil {
		t.Fatal("arbitrary binary was accepted")
	}
}

func TestControlHeartbeatRejectsTrailingJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_heartbeat_seconds":30}{}`))
	}))
	defer server.Close()
	api := NewAPIClient(server.URL, "token")
	_, err := api.ControlHeartbeat(context.Background(), &controlplane.HeartbeatRequest{})
	if err == nil || !strings.Contains(err.Error(), "one JSON value") {
		t.Fatalf("trailing JSON error = %v", err)
	}
}

func TestControlHeartbeatReportsHealthyAfterDaemonRestart(t *testing.T) {
	var got controlplane.HeartbeatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/control/heartbeat" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(controlplane.HeartbeatResponse{NextHeartbeatSeconds: 300})
	}))
	defer server.Close()
	dir := t.TempDir()
	store := &controlStateStore{path: filepath.Join(dir, "control-state.json")}
	state := &managedControlState{Command: controlplane.Command{ID: "job-restart", Type: controlplane.CommandUpdateRestart,
		TargetVersion: version.Get(), Binary: "duckway-client-" + runtime.GOOS + "-" + runtime.GOARCH, SHA256: strings.Repeat("b", 64), Size: 2 << 20, LeaseToken: "lease", Attempt: 1}, Status: controlplane.JobRunning}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ServerURL: server.URL, Token: "token"}
	api := NewAPIClient(server.URL, "token")
	if _, err := controlHeartbeatOnce(context.Background(), dir, cfg, "proxy", "new-boot", api, store, true); err != nil {
		t.Fatal(err)
	}
	if got.CurrentJob == nil || got.CurrentJob.Status != controlplane.JobHealthy || got.CurrentJob.RunningVersion != version.Get() {
		t.Fatalf("heartbeat current job=%+v", got.CurrentJob)
	}
	loaded, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil {
		t.Fatalf("terminal state was not cleared: %+v", loaded)
	}
}
