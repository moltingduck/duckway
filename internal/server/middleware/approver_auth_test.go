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
	rec := do("Bearer "+tok, "198.51.100.7:4001") // even a valid token is refused while throttled
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
