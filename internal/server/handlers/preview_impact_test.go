package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/services"
)

type prevItem struct {
	Bucket        string `json:"bucket"`
	PlaceholderID string `json:"placeholder_id"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Hits          int64  `json:"hits"`
	Before        struct{ Verdict, Kind, Rule string }
	After         struct{ Verdict, Kind, Rule string }
	Reason        string `json:"reason"`
	ReasonCode    string `json:"reason_code"`
	SimulateURL   string `json:"simulate_url"`
	Refs          []struct {
		Ref           string
		Before, After struct{ Verdict, Kind, Rule string }
	} `json:"refs"`
}

type prevResp struct {
	WindowDays    int              `json:"window_days"`
	TotalRequests int64            `json:"total_requests"`
	Distinct      int              `json:"distinct"`
	Truncated     bool             `json:"truncated"`
	Buckets       map[string]int   `json:"buckets"`
	BucketHits    map[string]int64 `json:"bucket_hits"`
	Items         []prevItem       `json:"items"`
	Tokens        []struct {
		PlaceholderID string `json:"placeholder_id"`
		Distinct      int    `json:"distinct"`
	} `json:"tokens"`
	Error string `json:"error"`
}

func (r prevResp) item(path string) *prevItem {
	for i := range r.Items {
		if r.Items[i].Path == path {
			return &r.Items[i]
		}
	}
	return nil
}

func newPreviewFor(f *proxyFixture, sim *handlers.AccessSimulator) *handlers.ImpactPreview {
	phH := handlers.NewPlaceholderHandler(f.placeholderQ, f.svcQ, nil).WithKeyLookup(f.apiKeyQ, f.crypto)
	return handlers.NewImpactPreview(sim, phH, f.placeholderQ, f.svcQ, f.apiKeyQ, f.groupQ, f.logQ)
}

func previewDo(t *testing.T, fn http.HandlerFunc, id string, body interface{}) (int, prevResp) {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/preview-impact", bytes.NewReader(b))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	fn(w, r)
	var out prevResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("preview response: %v %s", err, w.Body.String())
	}
	return w.Code, out
}

func seedLogRow(t *testing.T, f *proxyFixture, phID, service, method, path, decision string, repeat int, detail string) {
	t.Helper()
	_, err := f.rawDB.Exec(`INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code, decision, approval_id, deny_detail, repeat_count)
		VALUES (?, ?, ?, ?, ?, 200, ?, '', ?, ?)`, f.clientID, phID, service, method, path, decision, detail, repeat)
	if err != nil {
		t.Fatal(err)
	}
}

func tableCounts(t *testing.T, f *proxyFixture) string {
	t.Helper()
	var sb strings.Builder
	for _, tbl := range []string{"request_log", "approvals", "placeholder_keys", "api_keys", "services", "request_log_detail"} {
		var n int
		if err := f.rawDB.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sb, "%s=%d ", tbl, n)
	}
	var rc int
	_ = f.rawDB.QueryRow("SELECT COALESCE(SUM(repeat_count),0) FROM request_log").Scan(&rc)
	fmt.Fprintf(&sb, "repeat=%d", rc)
	return sb.String()
}

const (
	pvCurrent = `{"version":"1","provider":"anthropic","rules":[
	 {"name":"r","deny_all_other":true,"endpoints":[
	  {"method":"GET","path":"/v1/*","allow":true},
	  {"method":"POST","path":"/v1/messages","allow":true},
	  {"method":"POST","path":"/v1/batch","approve":true},
	  {"method":"POST","path":"/v1/constrained","allow":true,"constraints":{"body":{"model":{"oneOf":["a"]}}}}]}]}`
	pvDraft = `{"version":"1","provider":"anthropic","rules":[
	 {"name":"block","endpoints":[{"method":"GET","path":"/v1/secret"}]},
	 {"name":"r","deny_all_other":true,"endpoints":[
	  {"method":"GET","path":"/v1/*","allow":true},
	  {"method":"POST","path":"/v1/messages","approve":true},
	  {"method":"POST","path":"/v1/batch","allow":true},
	  {"method":"POST","path":"/v1/complete","allow":true},
	  {"method":"POST","path":"/v1/constrained","allow":true,"constraints":{"body":{"model":{"oneOf":["a"]}}}}]}]}`
)

func seedPreviewRows(t *testing.T, f *proxyFixture) {
	t.Helper()
	for _, r := range [][]interface{}{
		{"GET", "/v1/models", "allowed", 1},
		{"GET", "/v1/secret", "allowed", 2},
		{"POST", "/v1/messages", "allowed", 1},
		{"POST", "/v1/messages", "allowed", 3}, // same group: hits add up
		{"POST", "/v1/batch", "held", 1},
		{"POST", "/v1/complete", "acl_denied", 5},
		{"POST", "/v1/constrained", "allowed", 1},
		{"DELETE", "/v1/files/1", "acl_denied", 1},
	} {
		seedLogRow(t, f, f.placeholderID, "anthropic", r[0].(string), r[1].(string), r[2].(string), r[3].(int), "")
	}
	// A marker row of suppressed denials is not traffic.
	seedLogRow(t, f, f.placeholderID, "anthropic", "POST", "[further denials suppressed this minute]", "acl_denied", 1, "")
}

func TestPreviewTokenBuckets(t *testing.T) {
	rig := newApproveRig(t, pvCurrent)
	f := rig.f
	seedPreviewRows(t, f)
	p := newPreviewFor(f, newSimFor(f))
	before := tableCounts(t, f)

	code, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": pvDraft})
	if code != 200 {
		t.Fatalf("%d %s", code, r.Error)
	}
	if after := tableCounts(t, f); after != before {
		t.Fatalf("preview changed the database: %s -> %s", before, after)
	}
	want := map[string]string{
		"/v1/models":      "unchanged",
		"/v1/secret":      "newly_refused",
		"/v1/messages":    "newly_held",
		"/v1/batch":       "no_longer_held",
		"/v1/complete":    "newly_allowed",
		"/v1/files/1":     "unchanged",
		"/v1/constrained": "unknown",
	}
	for path, b := range want {
		it := r.item(path)
		if it == nil || it.Bucket != b {
			t.Errorf("%s: %+v want %s", path, it, b)
		}
	}
	if r.Distinct != len(want) || r.WindowDays != 7 || r.Truncated {
		t.Errorf("summary: %+v", r)
	}
	if r.TotalRequests != 15 {
		t.Errorf("total_requests %d (the marker row must not count)", r.TotalRequests)
	}
	if it := r.item("/v1/messages"); it.Hits != 4 || it.Before.Verdict != "allowed" || it.After.Verdict != "held" || it.After.Kind != "approval" {
		t.Errorf("messages item: %+v", it)
	}
	if it := r.item("/v1/secret"); it.After.Verdict != "refused" || it.After.Kind != "token_acl" || it.After.Rule == "" {
		t.Errorf("secret item: %+v", it)
	}
	if it := r.item("/v1/constrained"); it.ReasonCode != "body_constraints" || it.Reason == "" {
		t.Errorf("unknown reason: %+v", it)
	}
	if it := r.item("/v1/secret"); !strings.HasPrefix(it.SimulateURL, "/admin/phantom-tokens/"+f.placeholderID+"?tab=test&method=GET&path=%2Fv1%2Fsecret") {
		t.Errorf("simulate url %q", it.SimulateURL)
	}
	for b, n := range map[string]int{"newly_refused": 1, "newly_allowed": 1, "newly_held": 1, "no_longer_held": 1, "unknown": 1, "unchanged": 2} {
		if r.Buckets[b] != n {
			t.Errorf("bucket %s = %d want %d", b, r.Buckets[b], n)
		}
	}
	if r.BucketHits["newly_allowed"] != 5 {
		t.Errorf("bucket_hits %v", r.BucketHits)
	}
}

func TestPreviewTokenWideApprovalDraft(t *testing.T) {
	rig := newApproveRig(t, pvCurrent)
	f := rig.f
	seedPreviewRows(t, f)
	p := newPreviewFor(f, newSimFor(f))
	_, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"requires_approval": true, "approval_ttl_minutes": 30})
	// Every group that was decidable and not already held is now held; the
	// body-constrained one stays unknown, the approve-rule one was held before.
	if r.Buckets["newly_held"] != 5 || r.Buckets["unchanged"] != 1 || r.Buckets["unknown"] != 1 {
		t.Fatalf("token-wide approval: %+v", r.Buckets)
	}
	if it := r.item("/v1/models"); it.Bucket != "newly_held" || it.After.Rule != "token-wide approval" {
		t.Errorf("%+v", it)
	}
}

// A rate-limited rule must not count the replay: the enforcing check would
// refuse the second evaluation of a group.
func TestPreviewDoesNotCountRateLimits(t *testing.T) {
	cfg := `{"version":"1","provider":"anthropic","rules":[{"name":"rl","rate_limit":{"requests_per_minute":1},"endpoints":[{"method":"GET","path":"/v1/*","allow":true}]}]}`
	rig := newApproveRig(t, cfg)
	f := rig.f
	for i := 0; i < 20; i++ {
		seedLogRow(t, f, f.placeholderID, "anthropic", "GET", fmt.Sprintf("/v1/p%d", i), "allowed", 1, "")
	}
	p := newPreviewFor(f, newSimFor(f))
	_, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": cfg})
	if r.Buckets["unchanged"] != 20 {
		t.Fatalf("rate limit was counted: %+v", r.Buckets)
	}
	// And the live proxy still has its whole allowance.
	code, _ := rig.do("GET", "/v1/live")
	if code != 200 {
		t.Fatalf("the proxy's first request was refused (%d): the preview consumed the rate limit", code)
	}
}

func TestPreviewInvalidDraftIs400(t *testing.T) {
	rig := newApproveRig(t, pvCurrent)
	f := rig.f
	p := newPreviewFor(f, newSimFor(f))
	if code, _ := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": "{not json"}); code != 400 {
		t.Errorf("token draft: %d", code)
	}
	if code, _ := previewDo(t, p.Key, f.apiKeyID, map[string]interface{}{"acl": "{not json"}); code != 400 {
		t.Errorf("key draft: %d", code)
	}
	if code, _ := previewDo(t, p.Key, f.apiKeyID, map[string]interface{}{}); code != 400 {
		t.Errorf("key draft missing: %d", code)
	}
	if code, _ := previewDo(t, p.Service, f.serviceID, map[string]interface{}{"default_acl": "{not json"}); code != 400 {
		t.Errorf("service draft: %d", code)
	}
	if code, _ := previewDo(t, p.Token, "nope", map[string]interface{}{}); code != 404 {
		t.Errorf("unknown token: %d", code)
	}
	if code, _ := previewDo(t, p.Key, "nope", map[string]interface{}{"acl": ""}); code != 404 {
		t.Errorf("unknown key: %d", code)
	}
	// A save rejects branch rules in a credential ACL; so does the preview.
	branch := `{"version":"1","provider":"github","rules":[{"name":"x","github_branch_policies":{"o/r":{"branch_policy":{"rules":[{"branches":"**","role":"read"}]}}},"endpoints":[]}]}`
	if code, _ := previewDo(t, p.Key, f.apiKeyID, map[string]interface{}{"acl": branch}); code != 400 {
		t.Errorf("key draft with branch rules: %d", code)
	}
}

func TestPreviewKeyAndServiceFanOut(t *testing.T) {
	rig := newApproveRig(t, "")
	f := rig.f
	// Tokens: the fixture token (on the key), a second client token on the
	// same key, one on another key, and a pool token whose group has the key.
	for _, q := range []string{
		`INSERT INTO clients (id, name, token_hash) VALUES ('cli-2', 'second', 'h2')`,
		`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('key-2', '` + f.serviceID + `', 'other', 'x')`,
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval) VALUES ('ph-same', 'SAME', 'sk-dw-same', '` + f.serviceID + `', '` + f.apiKeyID + `', 'cli-2', 0)`,
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval) VALUES ('ph-other', 'OTHER', 'sk-dw-other', '` + f.serviceID + `', 'key-2', 'cli-2', 0)`,
		`INSERT INTO api_key_groups (id, name, service_id) VALUES ('grp-1', 'pool', '` + f.serviceID + `')`,
		`INSERT INTO api_key_group_members (group_id, api_key_id, priority) VALUES ('grp-1', '` + f.apiKeyID + `', 0)`,
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, group_id, client_id, requires_approval) VALUES ('ph-pool', 'POOL', 'sk-dw-pool', '` + f.serviceID + `', 'grp-1', 'cli-2', 0)`,
	} {
		if _, err := f.rawDB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, id := range []string{f.placeholderID, "ph-same", "ph-other", "ph-pool"} {
		seedLogRow(t, f, id, "anthropic", "GET", "/v1/models", "allowed", 2, "")
	}
	p := newPreviewFor(f, newSimFor(f))
	deny := `{"version":"1","provider":"anthropic","rules":[{"name":"d","endpoints":[{"method":"GET","path":"/v1/models"}]}]}`

	code, r := previewDo(t, p.Key, f.apiKeyID, map[string]interface{}{"acl": deny})
	if code != 200 {
		t.Fatalf("%d %s", code, r.Error)
	}
	got := map[string]bool{}
	for _, it := range r.Items {
		got[it.PlaceholderID] = true
		if it.Bucket != "newly_refused" || it.After.Kind != "api_key_acl" {
			t.Errorf("item %+v", it)
		}
	}
	if len(got) != 3 || !got[f.placeholderID] || !got["ph-same"] || !got["ph-pool"] || got["ph-other"] {
		t.Fatalf("key fan-out reached %v", got)
	}
	if len(r.Tokens) != 3 {
		t.Fatalf("per-token groups: %+v", r.Tokens)
	}

	code, r = previewDo(t, p.Service, f.serviceID, map[string]interface{}{"default_acl": deny})
	if code != 200 {
		t.Fatalf("%d %s", code, r.Error)
	}
	if r.Buckets["newly_refused"] != 4 || len(r.Tokens) != 4 || r.Items[0].After.Kind != "service_acl" {
		t.Fatalf("service fan-out: %+v", r)
	}
}

func TestPreviewCapAndTimeout(t *testing.T) {
	rig := newApproveRig(t, "")
	f := rig.f
	tx, err := f.rawDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5100; i++ {
		if _, err := tx.Exec(`INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code, decision, repeat_count) VALUES (?, ?, 'anthropic', 'GET', ?, 200, 'allowed', 1)`,
			f.clientID, f.placeholderID, fmt.Sprintf("/v1/p%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	p := newPreviewFor(f, newSimFor(f))
	_, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{})
	if !r.Truncated || r.Distinct != 5000 || r.Buckets["unchanged"] != 5000 {
		t.Fatalf("5,000 group cap: truncated=%v distinct=%d %v", r.Truncated, r.Distinct, r.Buckets)
	}
	if len(r.Items) != 200 || r.TotalRequests != 5100 {
		t.Fatalf("200 items per bucket: %d items, total %d", len(r.Items), r.TotalRequests)
	}
	started := time.Now()
	p.WithTimeout(time.Nanosecond)
	_, r = previewDo(t, p.Token, f.placeholderID, map[string]interface{}{})
	if !r.Truncated || r.Distinct >= 5000 {
		t.Fatalf("timeout must give a partial result: truncated=%v distinct=%d", r.Truncated, r.Distinct)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("deadline was not honored")
	}
}

// ---- GitHub: per-ref evaluation and unknown reasons ----

func previewGitHubConfigs(t *testing.T) (current, draft string) {
	t.Helper()
	current = builderConfig(t, false, false)
	var cfg services.PermissionConfig
	if err := json.Unmarshal([]byte(current), &cfg); err != nil {
		t.Fatal(err)
	}
	pol := cfg.Rules[0].GitHubBranchPolicies["owner/repo"]
	pol.BranchPolicy.Rules[3].Role = "agent" // "**" now lets the agent push
	cfg.Rules[0].GitHubBranchPolicies["owner/repo"] = pol
	b, _ := json.Marshal(cfg)
	return current, string(b)
}

func TestPreviewGitHubRefsAndUnknown(t *testing.T) {
	current, draft := previewGitHubConfigs(t)
	e := newBrEnv(t, current)
	f := e.f
	sim := newSimFor(f).WithGitHubDefaultBranchFetcher(func(context.Context, handlers.GitHubDefaultBranchRequest) (string, error) { return "main", nil })
	p := newPreviewFor(f, sim)

	refs := `{"kind":"branch_rules","layer":"placeholder","rule":"** → read","reason":"x","refs":[` +
		`{"ref":"refs/heads/feature/x","verdict":"refused","pattern":"**","role":"read","reason":"branch \"feature/x\" has role \"read\""},` +
		`{"ref":"refs/heads/agent/x","verdict":"allowed","pattern":"agent/**","role":"agent","reason":""}],"sees":""}`
	push := "/owner/repo.git/git-receive-pack"
	seedLogRow(t, f, f.placeholderID, "github", "POST", push, "acl_denied", 2, refs)
	seedLogRow(t, f, f.placeholderID, "github", "POST", "/owner/other.git/git-receive-pack", "allowed", 1, "")
	seedLogRow(t, f, f.placeholderID, "github", "GET", "/owner/repo.git/info/refs", "allowed", 1, "")
	seedLogRow(t, f, f.placeholderID, "github", "POST", "/repos/owner/repo/git/refs", "allowed", 1, "")
	seedLogRow(t, f, f.placeholderID, "github", "GET", "/repos/owner/repo/issues", "allowed", 1, "")
	before := tableCounts(t, f)

	code, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": draft})
	if code != 200 {
		t.Fatalf("%d %s", code, r.Error)
	}
	if after := tableCounts(t, f); after != before {
		t.Fatalf("preview changed the database: %s -> %s", before, after)
	}
	it := r.item(push)
	if it == nil || it.Bucket != "newly_allowed" || len(it.Refs) != 2 || it.Hits != 2 {
		t.Fatalf("push with refs: %+v", it)
	}
	if it.Refs[0].Ref != "refs/heads/feature/x" || it.Refs[0].Before.Verdict != "refused" || it.Refs[0].After.Verdict != "allowed" {
		t.Errorf("feature ref: %+v", it.Refs[0])
	}
	if it.Refs[1].Before.Verdict != "allowed" || it.Refs[1].After.Verdict != "allowed" {
		t.Errorf("agent ref: %+v", it.Refs[1])
	}
	for path, code := range map[string]string{
		"/owner/other.git/git-receive-pack": "push_refs_not_logged",
		"/owner/repo.git/info/refs":         "query_not_logged",
		"/repos/owner/repo/git/refs":        "body_depends",
	} {
		u := r.item(path)
		if path == "/owner/other.git/git-receive-pack" {
			// A repository without branch rules is not guarded: decided by the ACL alone.
			if u == nil || u.Bucket != "unchanged" {
				t.Errorf("%s: %+v", path, u)
			}
			continue
		}
		if u == nil || u.Bucket != "unknown" || u.ReasonCode != code || u.Reason == "" {
			t.Errorf("%s: %+v want %s", path, u, code)
		}
	}
	if u := r.item("/repos/owner/repo/issues"); u == nil || u.Bucket != "unchanged" {
		t.Errorf("issues: %+v", u)
	}

	// An allowed push to the guarded repository has no refs in the log.
	seedLogRow(t, f, f.placeholderID, "github", "POST", "/owner/repo.git/git-receive-pack?x", "allowed", 1, "")
	seedLogRow(t, f, f.placeholderID, "github", "POST", "/owner/repo.git/git-receive-pack", "allowed", 1, "")
	_, r = previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": draft})
	if u := r.item(push); u == nil || u.Bucket != "newly_allowed" {
		t.Errorf("group with refs and allowed rows keeps its refs: %+v", u)
	}
}

func TestPreviewGitHubAllowedPushIsUnknown(t *testing.T) {
	current, draft := previewGitHubConfigs(t)
	e := newBrEnv(t, current)
	f := e.f
	p := newPreviewFor(f, newSimFor(f).WithGitHubDefaultBranchFetcher(func(context.Context, handlers.GitHubDefaultBranchRequest) (string, error) { return "main", nil }))
	seedLogRow(t, f, f.placeholderID, "github", "POST", "/owner/repo.git/git-receive-pack", "allowed", 3, "")
	_, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": draft})
	u := r.item("/owner/repo.git/git-receive-pack")
	if u == nil || u.Bucket != "unknown" || u.ReasonCode != "push_refs_not_logged" || u.Hits != 3 {
		t.Fatalf("%+v", u)
	}
}

func TestPreviewDefaultBranchLookupFailureIsUnknown(t *testing.T) {
	current, draft := previewGitHubConfigs(t)
	e := newBrEnv(t, current)
	f := e.f
	sim := newSimFor(f).WithGitHubDefaultBranchFetcher(func(context.Context, handlers.GitHubDefaultBranchRequest) (string, error) {
		return "", fmt.Errorf("down")
	})
	p := newPreviewFor(f, sim)
	refs := `{"kind":"branch_rules","layer":"placeholder","rule":"","reason":"x","refs":[{"ref":"refs/heads/agent/x","verdict":"allowed","pattern":"","role":"","reason":""}],"sees":""}`
	seedLogRow(t, f, f.placeholderID, "github", "POST", "/owner/repo.git/git-receive-pack", "acl_denied", 1, refs)
	_, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": draft})
	u := r.item("/owner/repo.git/git-receive-pack")
	if u == nil || u.Bucket != "unknown" || u.ReasonCode != "default_branch_unavailable" {
		t.Fatalf("a failed default-branch lookup must be unknown, not a rule change: %+v", u)
	}
}

// A git discovery request is evaluated as the proxy does: GET .../info/refs?service=X
// is checked as POST .../git-X. The query is not logged, so both forms are
// checked: refused only when both are refused, otherwise unknown.
func TestPreviewInfoRefsEvaluatesBothMappedForms(t *testing.T) {
	cfg := `{"version":"1","provider":"anthropic","rules":[{"name":"r","deny_all_other":true,"endpoints":[
	  {"method":"POST","path":"/v1/repo.git/git-upload-pack","allow":true},
	  {"method":"GET","path":"/v1/*","allow":true}]}]}`
	rig := newApproveRig(t, cfg)
	f := rig.f
	seedLogRow(t, f, f.placeholderID, "anthropic", "GET", "/other/repo.git/info/refs", "acl_denied", 3, "")
	seedLogRow(t, f, f.placeholderID, "anthropic", "GET", "/v1/repo.git/info/refs", "allowed", 1, "")
	p := newPreviewFor(f, newSimFor(f))
	code, r := previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": cfg})
	if code != 200 {
		t.Fatalf("%d %s", code, r.Error)
	}
	refused := r.item("/other/repo.git/info/refs")
	if refused == nil || refused.Bucket != "unchanged" || refused.Before.Verdict != "refused" || refused.After.Verdict != "refused" {
		t.Fatalf("both mapped forms refused must be a known verdict: %+v", refused)
	}
	// Both POST forms refused but the plain GET allowed (any other service value): unknown.
	seedLogRow(t, f, f.placeholderID, "anthropic", "GET", "/v1/other.git/info/refs", "allowed", 1, "")
	_, r = previewDo(t, p.Token, f.placeholderID, map[string]interface{}{"permission_config": cfg})
	if g := r.item("/v1/other.git/info/refs"); g == nil || g.Bucket != "unchanged" && g.Bucket != "unknown" {
		t.Fatalf("unexpected: %+v", g)
	} else if g.Bucket != "unknown" {
		t.Fatalf("plain GET allowed must keep the group unknown: %+v", g)
	}
	mixed := r.item("/v1/repo.git/info/refs")
	if mixed == nil || mixed.Bucket != "unknown" || mixed.ReasonCode != "query_not_logged" {
		t.Fatalf("upload-pack allowed, receive-pack refused must be unknown: %+v", mixed)
	}
}

// Tokens made by a key suite are ordinary token rows on the key (suite_id set),
// so the key preview's fan-out includes them.
func TestPreviewKeyFanOutIncludesKeySuiteTokens(t *testing.T) {
	rig := newApproveRig(t, "")
	f := rig.f
	for _, q := range []string{
		`INSERT INTO key_suites (id, name) VALUES ('suite-1', 'team')`,
		`INSERT INTO clients (id, name, token_hash) VALUES ('cli-s', 'suite client', 'hs')`,
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, suite_id, requires_approval) VALUES ('ph-suite', 'SUITE', 'sk-dw-suite', '` + f.serviceID + `', '` + f.apiKeyID + `', 'cli-s', 'suite-1', 0)`,
	} {
		if _, err := f.rawDB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	seedLogRow(t, f, "ph-suite", "anthropic", "GET", "/v1/models", "allowed", 2, "")
	deny := `{"version":"1","provider":"anthropic","rules":[{"name":"d","endpoints":[{"method":"GET","path":"/v1/models"}]}]}`
	code, r := previewDo(t, newPreviewFor(f, newSimFor(f)).Key, f.apiKeyID, map[string]interface{}{"acl": deny})
	if code != 200 {
		t.Fatalf("%d %s", code, r.Error)
	}
	if it := r.item("/v1/models"); it == nil || it.PlaceholderID != "ph-suite" || it.Bucket != "newly_refused" {
		t.Fatalf("suite token missing from key fan-out: %+v", r.Items)
	}
}
