package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeterministicJitter(t *testing.T) {
	window := 30 * time.Minute
	a := deterministicJitter("client-a", window)
	b := deterministicJitter("client-a", window)
	if a != b {
		t.Fatalf("jitter is not deterministic: %s != %s", a, b)
	}
	if a < 0 || a >= window {
		t.Fatalf("jitter out of range: %s", a)
	}
	if got := deterministicJitter("client-a", 0); got != 0 {
		t.Fatalf("zero window jitter = %s", got)
	}
}

func TestJitterSeedSeparatesComponents(t *testing.T) {
	cfg := &Config{ServerURL: "https://duckway.example", ClientName: "agent-1", Token: "tok"}
	proxy := deterministicJitter(jitterSeed(cfg, "proxy", "bucket"), 30*time.Minute)
	watch := deterministicJitter(jitterSeed(cfg, "cc-watch", "bucket"), 30*time.Minute)
	if proxy == watch {
		t.Fatalf("expected different component jitter offsets, both were %s", proxy)
	}
}

func TestCheckAndLogUpdatePostsToManagementChannelOnce(t *testing.T) {
	var posts int32
	var postedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/client/update-info":
			if r.URL.Query().Get("version") == "" || r.URL.Query().Get("os") != runtime.GOOS || r.URL.Query().Get("arch") != runtime.GOARCH {
				t.Fatalf("unexpected update query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"server_version":             "v2",
				"client_current_version":     r.URL.Query().Get("version"),
				"client_recommended_version": "v2",
				"update_required":            true,
				"update_recommended":         true,
				"reason":                     "security rollout",
				"os":                         runtime.GOOS,
				"arch":                       runtime.GOARCH,
				"binary":                     "duckway-client-" + runtime.GOOS + "-" + runtime.GOARCH,
				"download_url":               "/download/duckway-client-" + runtime.GOOS + "-" + runtime.GOARCH,
				"sha256":                     strings.Repeat("a", 64),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/client/cc":
			if r.Header.Get("X-Duckway-Token") != "tok" {
				t.Fatalf("missing token")
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"assigned":          true,
				"cc_id":             "cc1",
				"cc_name":           "main",
				"agent_type":        "codex",
				"management_handle": "dwch_mgmt",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/client/cc/channels/dwch_mgmt/messages":
			atomic.AddInt32(&posts, 1)
			var req struct {
				Content string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			postedBody = req.Content
			_ = json.NewEncoder(w).Encode(map[string]string{"message_id": "m1"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := &Config{ServerURL: srv.URL, ClientName: "agent-1", Token: "tok"}
	logger := func(string, ...interface{}) {}
	checkAndLogUpdate(context.Background(), t.TempDir(), cfg, "cc-watch", logger)
	checkAndLogUpdate(context.Background(), t.TempDir(), cfg, "proxy", logger)
	if got := atomic.LoadInt32(&posts); got != 2 {
		t.Fatalf("posts with different config dirs = %d, want 2", got)
	}

	dir := t.TempDir()
	atomic.StoreInt32(&posts, 0)
	checkAndLogUpdate(context.Background(), dir, cfg, "cc-watch", logger)
	checkAndLogUpdate(context.Background(), dir, cfg, "proxy", logger)
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("posts with shared config dir = %d, want 1", got)
	}
	if !strings.Contains(postedBody, "Duckway client update REQUIRED") ||
		!strings.Contains(postedBody, "!duckway-update --restart") ||
		!strings.Contains(postedBody, "duckway update --server "+srv.URL+" && duckway restart") ||
		!strings.Contains(postedBody, "sudo") {
		t.Fatalf("unexpected notification body:\n%s", postedBody)
	}
}

func TestCheckAndLogUpdateRetriesFailedControlChannelPost(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/client/update-info":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"server_version":             "v2",
				"client_current_version":     r.URL.Query().Get("version"),
				"client_recommended_version": "v2",
				"update_required":            true,
				"update_recommended":         true,
				"os":                         runtime.GOOS,
				"arch":                       runtime.GOARCH,
				"binary":                     "duckway-client-" + runtime.GOOS + "-" + runtime.GOARCH,
				"download_url":               "/download/duckway-client-" + runtime.GOOS + "-" + runtime.GOARCH,
				"sha256":                     strings.Repeat("b", 64),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/client/cc":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"assigned":          true,
				"cc_id":             "cc1",
				"cc_name":           "main",
				"agent_type":        "codex",
				"management_handle": "dwch_mgmt",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/client/cc/channels/dwch_mgmt/messages":
			n := atomic.AddInt32(&posts, 1)
			if n == 1 {
				http.Error(w, "temporary discord failure", http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"message_id": "m2"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := &Config{ServerURL: srv.URL, ClientName: "agent-1", Token: "tok"}
	dir := t.TempDir()
	logger := func(string, ...interface{}) {}
	checkAndLogUpdate(context.Background(), dir, cfg, "cc-watch", logger)
	checkAndLogUpdate(context.Background(), dir, cfg, "proxy", logger)
	if got := atomic.LoadInt32(&posts); got != 2 {
		t.Fatalf("posts = %d, want retry after failed post", got)
	}
}
