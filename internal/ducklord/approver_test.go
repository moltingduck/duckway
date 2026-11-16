package ducklord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestApproverCredentialsPermissionsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "approver.json")
	if _, err := LoadApproverCredentials(path); !errors.Is(err, ErrApproverNotLoggedIn) {
		t.Fatalf("missing file err = %v", err)
	}
	if err := SaveApproverCredentials(path, ApproverCredentials{Server: "https://duck.example.com/", Token: "dwa_secret"}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
	if dinfo, _ := os.Stat(filepath.Dir(path)); dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o", dinfo.Mode().Perm())
	}
	got, err := LoadApproverCredentials(path)
	if err != nil || got.Server != "https://duck.example.com" || got.Token != "dwa_secret" {
		t.Fatalf("load = %v, %v", got, err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", got, *got, *got), "dwa_secret") {
		t.Fatal("formatting leaks the token")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApproverCredentials(path); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("loose mode err = %v", err)
	}
	_ = os.Chmod(path, 0o600)
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	if err := DeleteApproverCredentials(path); err != nil {
		t.Fatal(err)
	}
	if err := DeleteApproverCredentials(path); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestApproverCredentialsValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.json")
	for _, c := range []ApproverCredentials{
		{Server: "ftp://x", Token: "dwa_x"}, {Server: "", Token: "dwa_x"}, {Server: "https://u:p@x", Token: "dwa_x"},
		{Server: "https://x", Token: "client_token"},
	} {
		if err := SaveApproverCredentials(p, c); err == nil {
			t.Fatalf("%+v accepted", c)
		}
	}
}

func approverServer(t *testing.T, h http.HandlerFunc) *ApproverClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewApproverClient(srv.URL, "dwa_tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestApproverClientEndpoints(t *testing.T) {
	var seen []string
	c := approverServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dwa_tok" {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/approver/approvals" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode([]ApproverApproval{{ID: "a1", Status: "pending", ClientName: "c", Service: "s", Method: "POST", Path: "/p"}})
		case r.URL.Path == "/approver/approvals/a1" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(ApproverApproval{ID: "a1"})
		case strings.HasSuffix(r.URL.Path, "/approve"):
			var b map[string]int
			_ = json.NewDecoder(r.Body).Decode(&b)
			_ = json.NewEncoder(w).Encode(ApproverDecision{ID: "a1", Status: "approved", DecidedBy: fmt.Sprint("approver:x", b["duration_minutes"])})
		case strings.HasSuffix(r.URL.Path, "/reject"):
			_ = json.NewEncoder(w).Encode(ApproverDecision{ID: "a1", Status: "rejected"})
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	list, err := c.List(ctx, []string{"approved", "rejected"}, 50)
	if err != nil || len(list) != 1 || list[0].ID != "a1" {
		t.Fatalf("list = %v, %v", list, err)
	}
	if _, err := c.Get(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	d, err := c.Approve(ctx, "a1", 60)
	if err != nil || d.DecidedBy != "approver:x60" {
		t.Fatalf("approve = %v, %v", d, err)
	}
	if _, err := c.Reject(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "zzz"); !errors.Is(err, ErrApproverNotFound) {
		t.Fatalf("404 err = %v", err)
	}
	if seen[0] != "GET /approver/approvals?limit=50&status=approved%2Crejected" {
		t.Fatalf("list request = %q", seen[0])
	}
}

func TestApproverClientConflictAndUnauthorized(t *testing.T) {
	c := approverServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/approve") {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"approval already decided","status":"approved","decided_by":"discord:42","decided_via":"discord"}`))
			return
		}
		http.Error(w, `{"error":"bad"}`, http.StatusUnauthorized)
	})
	_, err := c.Approve(context.Background(), "a1", 0)
	var ad *AlreadyDecidedError
	if !errors.As(err, &ad) || err.Error() != "already decided by discord:42 via discord" {
		t.Fatalf("conflict err = %v", err)
	}
	if _, err := c.List(context.Background(), nil, 0); !errors.Is(err, ErrApproverUnauthorized) {
		t.Fatalf("401 err = %v", err)
	}
}

func TestApproverWatchEventsReconnectsWithBackoff(t *testing.T) {
	var conns atomic.Int32
	c := approverServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		if n == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "retry: 3000\nevent: hello\ndata: {\"v\":1}\n\n: hb\n\nevent: approvals\ndata: {\"pending\":3,\"total\":9}\n\n")
		fl.Flush()
		if n == 2 {
			return // abrupt end: reconnect
		}
		fmt.Fprint(w, "event: bye\ndata: {\"reason\":\"lifetime\"}\n\n")
		fl.Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := c.WatchEvents(ctx, ApproverBackoff{Min: 10 * time.Millisecond, Max: 40 * time.Millisecond})
	var kinds []ApproverEventKind
	var pending int64
	for ev := range ch {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == ApproverEventApprovals {
			pending = ev.Pending
		}
		if conns.Load() >= 4 {
			cancel()
		}
	}
	if pending != 3 || conns.Load() < 3 {
		t.Fatalf("pending=%d conns=%d kinds=%v", pending, conns.Load(), kinds)
	}
	if kinds[0] != ApproverEventDisconnected {
		t.Fatalf("first event should be the failed first connection: %v", kinds)
	}
}

func TestApproverWatchEventsStopsOnUnauthorized(t *testing.T) {
	c := approverServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got []ApproverEventKind
	for ev := range c.WatchEvents(ctx, ApproverBackoff{Min: time.Millisecond}) {
		got = append(got, ev.Kind)
	}
	if len(got) != 1 || got[0] != ApproverEventUnauthorized {
		t.Fatalf("events = %v", got)
	}
}
