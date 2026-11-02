package events

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

func ev(topic, data string) Event { return Event{Topic: topic, Data: json.RawMessage(data)} }

func TestHubSubscribeUnsubscribeAndActiveCallback(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	h := NewHub(Limits{}, func(a bool) { mu.Lock(); calls = append(calls, a); mu.Unlock() })
	a, err := h.Subscribe("s1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.Subscribe("s2")
	if h.Subscribers() != 2 {
		t.Fatalf("subs=%d", h.Subscribers())
	}
	h.Publish(ev("logs", `{"max_id":1}`))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, ok := a.Next(ctx)
	if !ok || len(got) != 1 || got[0].Topic != "logs" {
		t.Fatalf("a got %v %v", got, ok)
	}
	a.Close()
	a.Close() // idempotent
	b.Close()
	if h.Subscribers() != 0 {
		t.Fatal("subscribers leaked")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("callbacks=%v", calls)
	}
}

func TestHubCoalescesSlowSubscriberWithoutBlocking(t *testing.T) {
	h := NewHub(Limits{}, nil)
	s, _ := h.Subscribe("x")
	defer s.Close()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			h.Publish(ev("logs", `{"max_id":`+string(rune('0'+i%10))+`}`))
			h.Publish(ev("badge", `{"pending":1}`))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher blocked by slow subscriber")
	}
	got, _ := s.Next(context.Background())
	if len(got) != 2 || got[0].Topic != "logs" || got[1].Topic != "badge" {
		t.Fatalf("expected one coalesced event per topic, got %v", got)
	}
}

func TestHubLimits(t *testing.T) {
	h := NewHub(Limits{MaxSubscribers: 3, MaxPerSession: 2}, nil)
	a, _ := h.Subscribe("a")
	if _, err := h.Subscribe("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("a"); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("want session limit, got %v", err)
	}
	if _, err := h.Subscribe("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("c"); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("want global limit, got %v", err)
	}
	a.Close()
	if _, err := h.Subscribe("a"); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestHubClose(t *testing.T) {
	h := NewHub(Limits{}, nil)
	s, _ := h.Subscribe("a")
	res := make(chan bool)
	go func() { _, ok := s.Next(context.Background()); res <- ok }()
	time.Sleep(20 * time.Millisecond)
	h.Close()
	select {
	case ok := <-res:
		if ok {
			t.Fatal("Next should report closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Next not released by Close")
	}
	if _, err := h.Subscribe("a"); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	h.Close()
}

type fakeSrc struct {
	mu   sync.Mutex
	snap queries.WatchSnapshot
	n    atomic.Int64
}

func (f *fakeSrc) Snapshot(context.Context) (queries.WatchSnapshot, error) {
	f.n.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, nil
}
func (f *fakeSrc) set(fn func(*queries.WatchSnapshot)) { f.mu.Lock(); fn(&f.snap); f.mu.Unlock() }

type capture struct {
	mu  sync.Mutex
	evs []Event
}

func (c *capture) Publish(e Event) { c.mu.Lock(); c.evs = append(c.evs, e); c.mu.Unlock() }
func (c *capture) topics() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.evs {
		out = append(out, e.Topic)
	}
	return out
}

func TestWatcherPublishesPerTopicOnlyOnChange(t *testing.T) {
	src := &fakeSrc{}
	pub := &capture{}
	tick := make(chan time.Time)
	w := &Watcher{Src: src, Pub: pub, Tick: func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	step := func() { tick <- time.Now() }
	// Each step blocks until the loop receives the tick, so the snapshot of the
	// previous step has been processed; a trailing sync step flushes it.
	step() // baseline processed, snapshot #2 pending
	step()
	if got := pub.topics(); len(got) != 0 {
		t.Fatalf("unchanged: %v", got)
	}
	src.set(func(s *queries.WatchSnapshot) { s.LogMaxID = 5 })
	step()
	step()
	if got := strings.Join(pub.topics(), ","); got != "logs" {
		t.Fatalf("logs change: %q", got)
	}
	src.set(func(s *queries.WatchSnapshot) { s.ApprovalsSig = "x"; s.ApprovalsPend = 2; s.ApprovalsTotal = 3 })
	step()
	step()
	if got := strings.Join(pub.topics(), ","); got != "logs,approvals,badge" {
		t.Fatalf("approvals change: %q", got)
	}
	src.set(func(s *queries.WatchSnapshot) { s.ApprovalsSig = "y" }) // decision without pending change
	step()
	step()
	src.set(func(s *queries.WatchSnapshot) { s.RolloutsSig = "r1" })
	step()
	step()
	if got := strings.Join(pub.topics(), ","); got != "logs,approvals,badge,approvals,rollouts" {
		t.Fatalf("sequence: %q", got)
	}
	pub.mu.Lock()
	if string(pub.evs[2].Data) != `{"pending":2}` || string(pub.evs[0].Data) != `{"max_id":5}` {
		t.Fatalf("payloads: %s %s", pub.evs[2].Data, pub.evs[0].Data)
	}
	pub.mu.Unlock()
	cancel()
	<-done
}

func TestServiceWatcherRunsOnlyWithSubscribers(t *testing.T) {
	src := &fakeSrc{}
	svc := NewService(src, ServiceOptions{Interval: 5 * time.Millisecond})
	defer svc.Stop()
	if svc.Running() {
		t.Fatal("running without subscribers")
	}
	sub, _ := svc.Hub.Subscribe("a")
	if !svc.Running() {
		t.Fatal("not running with subscriber")
	}
	src.set(func(s *queries.WatchSnapshot) { s.LogMaxID = 1 })
	time.Sleep(30 * time.Millisecond)
	src.set(func(s *queries.WatchSnapshot) { s.LogMaxID = 2 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, ok := sub.Next(ctx)
	if !ok || got[0].Topic != TopicLogs {
		t.Fatalf("no logs event: %v %v", got, ok)
	}
	sub.Close()
	if svc.Running() {
		t.Fatal("still running after last unsubscribe")
	}
	time.Sleep(20 * time.Millisecond)
	n := src.n.Load()
	time.Sleep(40 * time.Millisecond)
	if src.n.Load() != n {
		t.Fatal("watcher kept polling after stop")
	}
}

func newTestService(t *testing.T, limits Limits) (*Service, *fakeSrc) {
	t.Helper()
	src := &fakeSrc{}
	svc := NewService(src, ServiceOptions{Interval: 10 * time.Millisecond, Limits: limits})
	t.Cleanup(svc.Stop)
	return svc, src
}

func TestHandlerHeadersHelloHeartbeatEventAndDisconnect(t *testing.T) {
	before := runtime.NumGoroutine()
	svc, src := newTestService(t, Limits{})
	srv := httptest.NewServer(svc.Handler(HandlerConfig{Heartbeat: 30 * time.Millisecond}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	req.AddCookie(&http.Cookie{Name: "duckway_session", Value: "leak"})
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	for k, want := range map[string]string{"Content-Type": "text/event-stream", "X-Accel-Buffering": "no"} {
		if resp.Header.Get(k) != want {
			t.Errorf("%s=%q", k, resp.Header.Get(k))
		}
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "no-cache") {
		t.Errorf("Cache-Control=%q", resp.Header.Get("Cache-Control"))
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitFor := func(pred func(string) bool) bool {
		timeout := time.After(3 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return false
				}
				if pred(l) {
					return true
				}
			case <-timeout:
				return false
			}
		}
	}
	if !waitFor(func(l string) bool { return strings.HasPrefix(l, "retry: ") }) {
		t.Fatal("no retry")
	}
	if !waitFor(func(l string) bool { return l == "event: hello" }) {
		t.Fatal("no hello")
	}
	if !waitFor(func(l string) bool { return l == ": hb" }) {
		t.Fatal("no heartbeat")
	}
	src.set(func(s *queries.WatchSnapshot) { s.LogMaxID = 9 })
	if !waitFor(func(l string) bool { return l == "event: logs" }) {
		t.Fatal("no logs event")
	}
	if !waitFor(func(l string) bool { return l == `data: {"max_id":9}` }) {
		t.Fatal("bad logs data")
	}

	cancel()
	resp.Body.Close()
	tr.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for svc.Hub.Subscribers() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.Hub.Subscribers() != 0 {
		t.Fatal("handler did not unsubscribe on disconnect")
	}
	srv.Close()
	tr.CloseIdleConnections()
	svc.Stop()
	deadline = time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutine leak: before=%d after=%d\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}

func TestHandlerLimitStatusCodes(t *testing.T) {
	svc, _ := newTestService(t, Limits{MaxSubscribers: 2, MaxPerSession: 1})
	srv := httptest.NewServer(svc.Handler(HandlerConfig{}))
	t.Cleanup(srv.Close)
	open := func(cookie string) *http.Response {
		r, err := getWithSession(t, srv.URL, cookie)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Body.Close() })
		return r
	}
	if r := open("a"); r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
	if r := open("a"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("per-session: %s", r.Status)
	}
	if r := open("b"); r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
	if r := open("c"); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("global: %s", r.Status)
	}
}

func TestHandlerEndsOnServiceStop(t *testing.T) {
	svc, _ := newTestService(t, Limits{})
	srv := httptest.NewServer(svc.Handler(HandlerConfig{}))
	t.Cleanup(srv.Close)
	resp, err := getWithSession(t, srv.URL, "stop")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	done := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(resp.Body).ReadString('~') // reads until EOF
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	svc.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream not closed by Stop")
	}
}

func getWithSession(t *testing.T, url, session string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "duckway_session", Value: session})
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return (&http.Client{Transport: tr}).Do(req)
}

func TestHandlerRejectsMissingSessionCookie(t *testing.T) {
	svc, _ := newTestService(t, Limits{})
	srv := httptest.NewServer(svc.Handler(HandlerConfig{}))
	t.Cleanup(srv.Close)
	r, err := getWithSession(t, srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", r.StatusCode)
	}
	if svc.Hub.Subscribers() != 0 {
		t.Fatal("subscribed without session")
	}
}

func TestHandlerPerUserLimitAcrossSessions(t *testing.T) {
	svc, _ := newTestService(t, Limits{MaxPerUser: 2})
	srv := httptest.NewServer(svc.Handler(HandlerConfig{
		ValidateSession: func(*http.Request) (string, bool) { return "alice", true },
	}))
	t.Cleanup(srv.Close)
	for i, want := range []int{200, 200, http.StatusTooManyRequests} {
		r, err := getWithSession(t, srv.URL, fmt.Sprintf("sess-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Body.Close() })
		if r.StatusCode != want {
			t.Fatalf("stream %d status=%d want %d", i, r.StatusCode, want)
		}
	}
}

func readToEOF(t *testing.T, r *http.Response, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, r.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatal("stream not closed in time")
	}
}

func TestHandlerClosesWhenSessionBecomesInvalid(t *testing.T) {
	svc, _ := newTestService(t, Limits{})
	var valid atomic.Bool
	valid.Store(true)
	srv := httptest.NewServer(svc.Handler(HandlerConfig{
		Heartbeat:       20 * time.Millisecond,
		ValidateSession: func(*http.Request) (string, bool) { return "bob", valid.Load() },
	}))
	t.Cleanup(srv.Close)
	r, err := getWithSession(t, srv.URL, "s")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
	valid.Store(false)
	readToEOF(t, r, 3*time.Second)
	// A fresh connection with the now-invalid session is refused.
	r2, err := getWithSession(t, srv.URL, "s")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", r2.StatusCode)
	}
}

func TestHandlerClosesAtMaxLifetime(t *testing.T) {
	svc, _ := newTestService(t, Limits{})
	srv := httptest.NewServer(svc.Handler(HandlerConfig{MaxLifetime: 100 * time.Millisecond}))
	t.Cleanup(srv.Close)
	r, err := getWithSession(t, srv.URL, "s")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	readToEOF(t, r, 3*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for svc.Hub.Subscribers() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.Hub.Subscribers() != 0 {
		t.Fatal("subscription leaked after lifetime close")
	}
}

func TestHandlerRefusesWithoutWriteDeadlineSupport(t *testing.T) {
	svc, _ := newTestService(t, Limits{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "duckway_session", Value: "s"})
	svc.Handler(HandlerConfig{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHubRejectsEmptySessionAndLimitsUser(t *testing.T) {
	h := NewHub(Limits{MaxPerUser: 1}, nil)
	if _, err := h.Subscribe(""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("empty session: %v", err)
	}
	a, err := h.SubscribeUser("s1", "u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SubscribeUser("s2", "u"); !errors.Is(err, ErrUserLimit) {
		t.Fatalf("user limit: %v", err)
	}
	a.Close()
	if _, err := h.SubscribeUser("s2", "u"); err != nil {
		t.Fatalf("after close: %v", err)
	}
}
