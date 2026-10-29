package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/clientplugin"
	"github.com/hackerduck/duckway/internal/controlplane"
)

func TestExistingControlHeartbeatSkipsEmptyPluginReportForLegacyServer(t *testing.T) {
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Duckway-Token") != "synthetic-token" {
			t.Error("missing authenticated report identity")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/client/control/heartbeat":
			_ = json.NewEncoder(w).Encode(controlplane.HeartbeatResponse{NextHeartbeatSeconds: 300})
		case "/client/plugins/report":
			reports.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := filepath.Join(t.TempDir(), "config")
	cfg := &Config{ServerURL: server.URL, Token: "synthetic-token"}
	store := &controlStateStore{path: filepath.Join(dir, "control-state.json")}
	next, err := controlHeartbeatOnce(context.Background(), dir, cfg, "proxy", "boot", NewAPIClient(cfg.ServerURL, cfg.Token), store, false)
	if err != nil || next.Seconds() != 300 || reports.Load() != 0 {
		t.Fatalf("heartbeat integration: next=%v err=%v reports=%d", next, err, reports.Load())
	}
}

func TestPluginReportFailureDoesNotFailCoreHeartbeat(t *testing.T) {
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/control/heartbeat" {
			_ = json.NewEncoder(w).Encode(controlplane.HeartbeatResponse{NextHeartbeatSeconds: 300})
			return
		}
		if r.URL.Path == "/client/plugins/report" {
			reports.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	dir := filepath.Join(t.TempDir(), "config")
	cfg := &Config{ServerURL: server.URL, Token: "synthetic-token"}
	host := clientplugin.Host{ConfigDir: dir, ServerURL: cfg.ServerURL, Token: cfg.Token}
	if err := host.RecordOperation(context.Background(), "duckllo", "install", errors.New("offline")); err != nil {
		t.Fatal(err)
	}
	_, err := controlHeartbeatOnce(context.Background(), dir, cfg, "proxy", "boot", NewAPIClient(cfg.ServerURL, cfg.Token), &controlStateStore{path: filepath.Join(dir, "control-state.json")}, false)
	if err != nil || reports.Load() != 1 {
		t.Fatalf("best-effort managed report: err=%v reports=%d", err, reports.Load())
	}
}
