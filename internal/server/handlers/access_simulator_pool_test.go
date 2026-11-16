package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/server/handlers"
)

func (e *brEnv) exec(t *testing.T, q string, args ...interface{}) {
	t.Helper()
	if _, err := e.f.rawDB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// Pool members: the simulator says their own rules are not evaluated (the proxy
// does not apply them), and the failures of key resolution are the ones the
// proxy reports, byte for byte.
func TestSimulatorKeyPoolAndResolutionFailures(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	f := e.f
	sim := newSimFor(f)
	clone := map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "clone"}

	// An empty key group: the proxy cannot resolve a key and answers 500.
	e.exec(t, `INSERT INTO api_key_groups (id, service_id, name, strategy) VALUES ('grp-e', ?, 'empty', 'failover')`, f.serviceID)
	e.exec(t, `UPDATE placeholder_keys SET api_key_id = NULL, group_id = 'grp-e'`)
	_, s := simCall(t, sim, f.placeholderID, clone)
	if s.Verdict != "refused" || s.Kind != "credential" || s.Reason != "key resolution failed" {
		t.Fatalf("empty group: %+v", s)
	}
	code, body := e.replay(t, s, nil)
	if code != http.StatusInternalServerError || string(body) != s.Sees {
		t.Fatalf("proxy answered %d %q, simulator says %q", code, body, s.Sees)
	}

	// (The schema forbids a token with neither key nor group, so that
	// resolver error cannot occur.)

	// A pool with one member: Credential rules says the member's rules are not evaluated.
	e.exec(t, `INSERT INTO api_key_group_members (group_id, api_key_id, priority) VALUES ('grp-e', ?, 1)`, f.apiKeyID)
	_, s = simCall(t, sim, f.placeholderID, clone)
	if s.Verdict != "allowed" {
		t.Fatalf("pool: %+v", s)
	}
	found := false
	for _, st := range s.Steps {
		if st.Label == "Credential rules" {
			found = true
			if st.Status != "skip" || !strings.Contains(st.Detail, "pool key's own rules are not evaluated") {
				t.Errorf("credential step: %+v", st)
			}
		}
	}
	if !found {
		t.Fatalf("no credential step: %+v", s.Steps)
	}
}

// A failed default-branch lookup is remembered for a short time by the
// simulator and the access summary, so a down upstream is not hit on every
// page load; the summary stops looking up once its overall deadline passed.
func TestSimulatorAndSummaryNegativeCacheAndDeadline(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	var calls int32
	sim := newSimFor(e.f).WithGitHubDefaultBranchFetcher(func(context.Context, handlers.GitHubDefaultBranchRequest) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "", fmt.Errorf("upstream down")
	})
	summary := func(ctx context.Context) map[string]interface{} {
		r := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
		r.SetPathValue("id", e.f.placeholderID)
		w := httptest.NewRecorder()
		sim.AccessSummary(w, r)
		var out map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	for i := 0; i < 3; i++ {
		out := summary(context.Background())
		if r := out["repos"].([]interface{})[0].(map[string]interface{}); r["default_branch"] != nil {
			t.Fatalf("a failed lookup must leave default_branch null: %v", r["default_branch"])
		}
	}
	for i := 0; i < 3; i++ {
		simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "agent/x"})
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("the failing upstream was called %d times, want 1 (negative cache)", n)
	}

	// Deadline: with the overall deadline already exceeded no lookup is issued.
	e2 := newBrEnv(t, simPolicyConfig)
	var calls2 int32
	sim2 := newSimFor(e2.f).WithGitHubDefaultBranchFetcher(func(context.Context, handlers.GitHubDefaultBranchRequest) (string, error) {
		atomic.AddInt32(&calls2, 1)
		return "main", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	r := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	r.SetPathValue("id", e2.f.placeholderID)
	w := httptest.NewRecorder()
	sim2.AccessSummary(w, r)
	if atomic.LoadInt32(&calls2) != 0 {
		t.Fatalf("lookups issued after the deadline: %d", calls2)
	}
}
