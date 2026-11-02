package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

func TestEventsEndpointRequiresAdminAuth(t *testing.T) {
	s, _ := newShellTestServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", rec.Code)
	}
}

func TestEventsEndpointStreamsApprovalChangesFromSQLite(t *testing.T) {
	s, shared := newShellTestServer(t)
	t.Cleanup(s.Shutdown)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	expect := func(want string, d time.Duration) {
		t.Helper()
		timeout := time.After(d)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream closed waiting for %q", want)
				}
				if l == want {
					return
				}
			case <-timeout:
				t.Fatalf("timeout waiting for %q", want)
			}
		}
	}
	expect("event: hello", 3*time.Second)
	time.Sleep(300 * time.Millisecond) // let the watcher take its baseline
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if err := queries.NewApprovalQueries(s.db).Create(&models.Approval{ID: "ev1", PlaceholderID: "p", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	expect("event: approvals", 6*time.Second)
	expect("event: badge", 2*time.Second)
	if !strings.Contains(strings.Join(drain(lines), "\n"), `"pending":1`) {
		t.Fatal("badge payload missing pending count")
	}
}

func drain(ch chan string) []string {
	var out []string
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, l)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

func TestHTTPServerShutdownClosesOpenEventStreamPromptly(t *testing.T) {
	s, shared := newShellTestServer(t)
	t.Cleanup(s.Shutdown)
	ts := httptest.NewUnstartedServer(s)
	ts.Config.RegisterOnShutdown(s.CloseStreams)
	ts.Start()
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := ts.Config.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with open stream: %v after %s", err, time.Since(start))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %s", d)
	}
}
