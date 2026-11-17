package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/services"
)

func approverAuthFixture(t *testing.T) (*ApproverAuth, *queries.ApproverTokenQueries, http.Handler) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := queries.NewApproverTokenQueries(db)
	a := NewApproverAuth(q)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GetApprover(r) == nil {
			t.Error("handler reached without an approver in context")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return a, q, a.Middleware(next)
}

func approverReq(auth, remote string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/approver/approvals", nil)
	r.RemoteAddr = remote
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestApproverAuthAcceptsOnlyActiveBearerDwaTokens(t *testing.T) {
	_, q, h := approverAuthFixture(t)
	const tok = "dwa_0123456789abcdef"
	if err := q.Create("t1", "alice", services.HashToken(tok)); err != nil {
		t.Fatal(err)
	}
	do := func(r *http.Request) int { rec := httptest.NewRecorder(); h.ServeHTTP(rec, r); return rec.Code }
	if got := do(approverReq("Bearer "+tok, "192.0.2.1:1")); got != http.StatusNoContent {
		t.Fatalf("valid token: %d", got)
	}
	// A client token (any non-dwa_ secret) never authenticates, even if its hash were present.
	_ = q.Create("t2", "sneaky", services.HashToken("client-token"))
	if got := do(approverReq("Bearer client-token", "192.0.2.2:1")); got != http.StatusUnauthorized {
		t.Fatalf("non-dwa token: %d", got)
	}
	r := approverReq("", "192.0.2.3:1")
	r.Header.Set("X-Duckway-Token", tok)
	if got := do(r); got != http.StatusUnauthorized {
		t.Fatalf("X-Duckway-Token must not authenticate: %d", got)
	}
	if got := do(approverReq("Basic "+tok, "192.0.2.4:1")); got != http.StatusUnauthorized {
		t.Fatalf("wrong scheme: %d", got)
	}
	if err := q.Revoke("t1"); err != nil {
		t.Fatal(err)
	}
	if got := do(approverReq("Bearer "+tok, "192.0.2.5:1")); got != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", got)
	}
}

func TestApproverAuthRecordsLastUsed(t *testing.T) {
	_, q, h := approverAuthFixture(t)
	const tok = "dwa_abc"
	_ = q.Create("t1", "alice", services.HashToken(tok))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, approverReq("Bearer "+tok, "192.0.2.1:1"))
	list, _ := q.List()
	if len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("last_used_at not recorded: %+v", list)
	}
}

func TestApproverAuthRateLimitsFailuresPerAddress(t *testing.T) {
	a, q, h := approverAuthFixture(t)
	now := time.Now()
	a.now = func() time.Time { return now }
	const tok = "dwa_valid"
	_ = q.Create("t1", "alice", services.HashToken(tok))
	do := func(auth, remote string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, approverReq(auth, remote))
		return rec
	}
	for i := 0; i < approverMaxFailures; i++ {
		if rec := do("Bearer dwa_wrong", "198.51.100.7:4000"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, rec.Code)
		}
	}
	if rec := do("Bearer "+tok, "198.51.100.7:4001"); rec.Code != http.StatusNoContent { // a valid token is never throttled
		t.Fatalf("valid token while throttled: %d", rec.Code)
	}
	rec := do("Bearer dwa_wrong", "198.51.100.7:4001")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("throttled: %d %v", rec.Code, rec.Header())
	}
	if rec := do("Bearer "+tok, "198.51.100.8:4000"); rec.Code != http.StatusNoContent {
		t.Fatalf("another address is unaffected: %d", rec.Code)
	}
	now = now.Add(approverFailWindow + time.Second)
	if rec := do("Bearer "+tok, "198.51.100.7:4002"); rec.Code != http.StatusNoContent {
		t.Fatalf("after the window: %d", rec.Code)
	}
}

// A valid token is never throttled, even from an address that has used up its
// failures; invalid requests are still counted and get 429.
func TestApproverThrottleNeverBlocksValidToken(t *testing.T) {
	_, q, h := approverAuthFixture(t)
	const tok = "dwa_validtoken123"
	_ = q.Create("t1", "alice", services.HashToken(tok))
	do := func(auth string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, approverReq(auth, "192.0.2.50:1"))
		return rec.Code
	}
	for i := 0; i < approverMaxFailures; i++ {
		do("Bearer dwa_wrong")
	}
	if got := do("Bearer dwa_wrong"); got != http.StatusTooManyRequests {
		t.Fatalf("invalid after limit: %d", got)
	}
	if got := do("Bearer " + tok); got != http.StatusNoContent {
		t.Fatalf("valid token blocked by shared-address throttle: %d", got)
	}
}

// A full table evicts the oldest entry instead of resetting every counter.
func TestApproverThrottleEvictsOldestNotAll(t *testing.T) {
	a, _, _ := approverAuthFixture(t)
	base := time.Now()
	i := 0
	a.now = func() time.Time { i++; return base.Add(time.Duration(i) * time.Millisecond) }
	for n := 0; n < approverMaxFailures; n++ {
		a.recordFailure("victim")
	}
	for n := 0; n < approverMaxTracked+10; n++ {
		a.recordFailure("flood-" + string(rune('a'+n%26)) + time.Duration(n).String())
	}
	a.mu.Lock()
	size := len(a.fails)
	a.mu.Unlock()
	if size > approverMaxTracked {
		t.Fatalf("table grew to %d", size)
	}
	// The victim was oldest, so it is the one evicted; the newest flood entry survives.
	if a.blocked("victim") {
		t.Fatal("oldest entry should have been evicted")
	}
	a.recordFailure("late")
	a.mu.Lock()
	_, ok := a.fails["late"]
	n := len(a.fails)
	a.mu.Unlock()
	if !ok || n > approverMaxTracked || n < approverMaxTracked-1 {
		t.Fatalf("late present=%v size=%d (table must stay near capacity, not reset)", ok, n)
	}
}
