package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
)

func approveExpiry(t *testing.T, f *proxyFixture, body string) time.Duration {
	t.Helper()
	id, err := handlers.CreatePendingApproval(f.approvalQ, f.placeholderID, "POST", "/x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	h := handlers.NewApprovalHandler(f.approvalQ, f.placeholderQ)
	req := httptest.NewRequest(http.MethodPost, "/api/approvals/"+id+"/approve", strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.Approve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	a, err := f.approvalQ.GetByID(id)
	if err != nil || a.ExpiresAt == nil {
		t.Fatalf("approval: %v", err)
	}
	exp, err := time.Parse(queries.ApprovalTimeLayout, *a.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return time.Until(exp.UTC())
}

// The console's Approve (no explicit duration) honours the token's
// approval_ttl_minutes; 24 hours is only the default.
func TestConsoleApproveUsesTokenApprovalTTL(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	f := newProxyFixture(t, up.URL)
	near := func(got, want time.Duration) bool { return got > want-2*time.Minute && got <= want+time.Minute }

	if got := approveExpiry(t, f, `{}`); !near(got, 24*time.Hour) {
		t.Fatalf("default token TTL: expiry in %v, want ~24h", got)
	}
	ph, _ := f.placeholderQ.GetByID(f.placeholderID)
	ph.ApprovalTTLMinutes = 90
	if err := f.placeholderQ.Update(ph); err != nil {
		t.Fatal(err)
	}
	if got := approveExpiry(t, f, `{}`); !near(got, 90*time.Minute) {
		t.Fatalf("token TTL 90: expiry in %v, want ~90m", got)
	}
	if got := approveExpiry(t, f, ``); !near(got, 90*time.Minute) {
		t.Fatalf("empty body: expiry in %v, want ~90m", got)
	}
	if got := approveExpiry(t, f, `{"duration_minutes":10}`); !near(got, 10*time.Minute) {
		t.Fatalf("explicit duration wins: expiry in %v, want ~10m", got)
	}
}

// The list data carries the effective TTL the page renders.
func TestApprovalListExposesApprovalTTL(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	f := newProxyFixture(t, up.URL)
	ph, _ := f.placeholderQ.GetByID(f.placeholderID)
	ph.ApprovalTTLMinutes = 120
	if err := f.placeholderQ.Update(ph); err != nil {
		t.Fatal(err)
	}
	if _, err := handlers.CreatePendingApproval(f.approvalQ, f.placeholderID, "GET", "/y", "", ""); err != nil {
		t.Fatal(err)
	}
	list, err := f.approvalQ.ListEnriched([]string{"pending"}, 10)
	if err != nil || len(list) != 1 || list[0].ApprovalTTLMinutes != 120 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if f.approvalQ.PlaceholderApprovalMinutes("no-such-token") != queries.DefaultApprovalMinutes {
		t.Fatal("unknown token falls back to 24 hours")
	}
}
