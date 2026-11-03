package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	ptFullToken  = "sk-dwph-0123456789abcdefghijklmnop"
	ptAllowRead  = `{"version":"1","provider":"svc","rules":[{"name":"reads","endpoints":[{"method":"GET","path":"/v1/*","allow":true}],"deny_all_other":false}]}`
	ptDenyDelete = `{"version":"1","provider":"svc","rules":[{"name":"no deletes","endpoints":[{"method":"DELETE","path":"/*","allow":false}],"deny_all_other":false}]}`
	// Custom features the visual builder cannot express: body constraint, rate limit, deny_all_other.
	ptCustomRule = `{"version":"1","provider":"svc","rules":[{"name":"chat only","endpoints":[{"method":"POST","path":"/v1/chat","allow":true,"constraints":{"body":{"model":{"oneOf":["gpt-4o"]}}}},{"method":"GET","path":"/v1/models","allow":true}],"rate_limit":{"requests_per_minute":1},"deny_all_other":true}]}`
)

type ptEnv struct {
	s      *Server
	shared *SharedServices
}

func newPTEnv(t *testing.T) *ptEnv {
	t.Helper()
	s, shared := newShellTestServer(t)
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern, default_acl) VALUES ('svc-pt', 'svc', 'Test <b>Service</b>', 'https://pt.example', 'pt.example', '')`)
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern, default_acl) VALUES ('svc-narrow', 'narrow', 'Narrow', 'https://n.example', 'n.example', ?)`,
		`{"version":"1","provider":"narrow","rules":[{"name":"svc rule","endpoints":[{"method":"POST","path":"/v1/chat","allow":false}],"deny_all_other":false}]}`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, acl) VALUES ('key-pt', 'svc-pt', 'plain key', 'x', '')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, acl) VALUES ('key-narrow', 'svc-narrow', 'cred', 'x', ?)`,
		`{"version":"1","provider":"narrow","rules":[{"name":"cred rule","endpoints":[{"method":"PUT","path":"/v1/*","allow":false}],"deny_all_other":false}]}`)
	exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES ('c-pt', 'pt1', 'pt <i>client</i>', 'h')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, permission_config, requires_approval, approval_ttl_minutes)
		VALUES ('ph-pt', 'PT_KEY', ?, 'svc-pt', 'key-pt', 'c-pt', ?, 0, 60)`, ptFullToken, ptAllowRead)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, permission_config, requires_approval, approval_ttl_minutes)
		VALUES ('ph-narrow', 'NARROW_KEY', 'sk-dwph-narrow0000000000000000000', 'svc-narrow', 'key-narrow', 'c-pt', '', 1, 1440)`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, permission_config, requires_approval, approval_ttl_minutes)
		VALUES ('ph-xss', '<script>alert(1)</script>', 'sk-dwph-xss00000000000000000000000', 'svc-pt', 'key-pt', 'c-pt', NULL, 0, 60)`)
	return &ptEnv{s: s, shared: shared}
}

func (e *ptEnv) do(t *testing.T, method, path, body string, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if admin {
		req.AddCookie(e.shared.AdminAuth.CreateSession("duckway", req))
	}
	rec := httptest.NewRecorder()
	testSameOrigin(req)
	e.s.ServeHTTP(rec, req)
	return rec
}

type ptTestResult struct {
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by"`
	Reason    string `json:"reason"`
	RuleIndex int    `json:"rule_index"`
	RuleName  string `json:"rule_name"`
	NeedsBody bool   `json:"needs_body"`
	Layers    []struct {
		Layer      string `json:"layer"`
		Configured bool   `json:"configured"`
		Result     string `json:"result"`
		RuleIndex  int    `json:"rule_index"`
		Default    bool   `json:"default"`
		Note       string `json:"note"`
	} `json:"layers"`
	Approval struct {
		Required   bool `json:"required"`
		TTLMinutes int  `json:"ttl_minutes"`
	} `json:"approval"`
}

func (e *ptEnv) aclTest(t *testing.T, id, body string) (int, ptTestResult, string) {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/placeholders/"+id+"/acl/test", body, true)
	var out ptTestResult
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

func TestACLTestEndpointUsesRealEngine(t *testing.T) {
	e := newPTEnv(t)

	t.Run("stored rules allow", func(t *testing.T) {
		code, r, raw := e.aclTest(t, "ph-pt", `{"method":"GET","path":"/v1/models"}`)
		if code != 200 || r.Decision != "allow" || r.DecidedBy != "token" || r.RuleIndex != 0 || r.RuleName != "reads" {
			t.Fatalf("got %d %+v %s", code, r, raw)
		}
		if len(r.Layers) != 3 || r.Layers[0].Layer != "service" || r.Layers[2].Layer != "token" {
			t.Fatalf("layers: %+v", r.Layers)
		}
	})
	t.Run("no rule matches is allowed by default", func(t *testing.T) {
		_, r, _ := e.aclTest(t, "ph-pt", `{"method":"POST","path":"/other"}`)
		if r.Decision != "allow" || r.DecidedBy != "default" || r.RuleIndex != -1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("draft deny overrides stored config", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{"method": "DELETE", "path": "/v1/files/1", "permission_config": ptDenyDelete})
		_, r, _ := e.aclTest(t, "ph-pt", string(body))
		if r.Decision != "deny" || r.DecidedBy != "token" || r.RuleIndex != 0 || r.RuleName != "no deletes" || !strings.Contains(r.Reason, "no deletes") {
			t.Fatalf("%+v", r)
		}
		// the same request against the stored config is allowed: the draft was not saved
		_, r, _ = e.aclTest(t, "ph-pt", `{"method":"DELETE","path":"/v1/files/1"}`)
		if r.Decision != "allow" {
			t.Fatalf("draft leaked into stored config: %+v", r)
		}
	})
	t.Run("empty draft means no token rules", func(t *testing.T) {
		_, r, _ := e.aclTest(t, "ph-pt", `{"method":"GET","path":"/zzz","permission_config":""}`)
		if r.Decision != "allow" || r.Layers[2].Configured {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("service layer narrows", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{"method": "POST", "path": "/v1/chat", "permission_config": `{"version":"1","provider":"narrow","rules":[{"name":"wide","endpoints":[{"method":"*","path":"/*","allow":true}]}]}`})
		_, r, _ := e.aclTest(t, "ph-narrow", string(body))
		if r.Decision != "deny" || r.DecidedBy != "service" || r.RuleName != "svc rule" {
			t.Fatalf("%+v", r)
		}
		if r.Layers[2].Result != "allow" || r.Layers[2].Default {
			t.Fatalf("token layer should explicitly allow: %+v", r.Layers[2])
		}
	})
	t.Run("credential layer narrows", func(t *testing.T) {
		_, r, _ := e.aclTest(t, "ph-narrow", `{"method":"PUT","path":"/v1/thing"}`)
		if r.Decision != "deny" || r.DecidedBy != "credential" || r.RuleName != "cred rule" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("approval reflects draft", func(t *testing.T) {
		_, r, _ := e.aclTest(t, "ph-pt", `{"method":"GET","path":"/v1/models","requires_approval":true,"approval_ttl_minutes":120}`)
		if !r.Approval.Required || r.Approval.TTLMinutes != 120 {
			t.Fatalf("%+v", r.Approval)
		}
		_, r, _ = e.aclTest(t, "ph-narrow", `{"method":"GET","path":"/v1/models"}`)
		if !r.Approval.Required || r.Approval.TTLMinutes != 1440 {
			t.Fatalf("stored approval not reported: %+v", r.Approval)
		}
	})
	t.Run("custom rule evaluates with body constraints", func(t *testing.T) {
		mk := func(method, path, rb string) ptTestResult {
			b, _ := json.Marshal(map[string]interface{}{"method": method, "path": path, "body": rb, "permission_config": ptCustomRule})
			code, r, raw := e.aclTest(t, "ph-pt", string(b))
			if code != 200 {
				t.Fatalf("%d %s", code, raw)
			}
			return r
		}
		if r := mk("POST", "/v1/chat", `{"model":"gpt-4o"}`); r.Decision != "allow" || r.RuleIndex != 0 || !r.NeedsBody {
			t.Fatalf("%+v", r)
		}
		if r := mk("POST", "/v1/chat", `{"model":"other"}`); r.Decision != "deny" || r.DecidedBy != "token" {
			t.Fatalf("%+v", r)
		}
		if r := mk("DELETE", "/v1/models", ``); r.Decision != "deny" || r.DecidedBy != "token" {
			t.Fatalf("deny_all_other: %+v", r)
		}
	})
	t.Run("git discovery normalised like the proxy", func(t *testing.T) {
		cfg := `{"version":"1","provider":"github","rules":[{"name":"push","endpoints":[{"method":"POST","path":"/acme/app.git/git-receive-pack","allow":false}]}]}`
		b, _ := json.Marshal(map[string]interface{}{"method": "GET", "path": "/acme/app.git/info/refs?service=git-receive-pack", "permission_config": cfg})
		_, r, _ := e.aclTest(t, "ph-pt", string(b))
		if r.Decision != "deny" || r.RuleName != "push" {
			t.Fatalf("%+v", r)
		}
	})
}

func TestACLTestEndpointHasNoSideEffects(t *testing.T) {
	e := newPTEnv(t)
	count := func(q string) int {
		var n int
		if err := e.s.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	snapshot := func() [4]int {
		return [4]int{count(`SELECT COUNT(*) FROM request_log`), count(`SELECT COUNT(*) FROM approvals`),
			count(`SELECT usage_count FROM placeholder_keys WHERE id='ph-pt'`), count(`SELECT COUNT(*) FROM placeholder_keys WHERE permission_config = '` + ptAllowRead + `'`)}
	}
	before := snapshot()
	// The custom rule has requests_per_minute:1. If the test counted, the
	// second call would be rate limited; a real request afterwards must still pass.
	for i := 0; i < 5; i++ {
		b, _ := json.Marshal(map[string]interface{}{"method": "GET", "path": "/v1/models", "permission_config": ptCustomRule, "requires_approval": true})
		code, r, raw := e.aclTest(t, "ph-pt", string(b))
		if code != 200 || r.Decision != "allow" {
			t.Fatalf("call %d: %d %s", i, code, raw)
		}
	}
	if after := snapshot(); after != before {
		t.Fatalf("test endpoint changed state: before %v after %v", before, after)
	}
	var stored string
	var approval int
	if err := e.s.db.QueryRow(`SELECT permission_config, requires_approval FROM placeholder_keys WHERE id='ph-pt'`).Scan(&stored, &approval); err != nil || stored != ptAllowRead || approval != 0 {
		t.Fatalf("draft was persisted: %v %q %d", err, stored, approval)
	}
}

func TestACLTestEndpointRejectsBadInput(t *testing.T) {
	e := newPTEnv(t)
	huge := strings.Repeat("a", 70<<10)
	for name, body := range map[string]string{
		"bad json":       `{`,
		"missing method": `{"path":"/x"}`,
		"unknown method": `{"method":"TRACE","path":"/x"}`,
		"relative path":  `{"method":"GET","path":"x"}`,
		"empty path":     `{"method":"GET","path":""}`,
		"whitespace":     `{"method":"GET","path":"/a b"}`,
		"invalid draft":  `{"method":"GET","path":"/x","permission_config":"{\"version\":\"9\"}"}`,
		"draft no rules": `{"method":"GET","path":"/x","permission_config":"{\"version\":\"1\",\"provider\":\"p\",\"rules\":[]}"}`,
		"draft not json": `{"method":"GET","path":"/x","permission_config":"nope"}`,
		"huge body":      `{"method":"POST","path":"/x","body":"` + huge + `"}`,
	} {
		if code, _, raw := e.aclTest(t, "ph-pt", body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, code, raw)
		}
	}
	if code, _, _ := e.aclTest(t, "nope", `{"method":"GET","path":"/x"}`); code != http.StatusNotFound {
		t.Errorf("unknown id: status %d, want 404", code)
	}
}

func TestACLTestEndpointRequiresAdmin(t *testing.T) {
	e := newPTEnv(t)
	rec := e.do(t, http.MethodPost, "/api/placeholders/ph-pt/acl/test", `{"method":"GET","path":"/x"}`, false)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
		t.Fatalf("anonymous status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "decision") {
		t.Fatal("anonymous caller got a decision")
	}
}

func TestCustomRuleRoundTripsThroughSave(t *testing.T) {
	e := newPTEnv(t)
	body, _ := json.Marshal(map[string]interface{}{"permission_config": ptCustomRule})
	if rec := e.do(t, http.MethodPut, "/api/placeholders/ph-pt", string(body), true); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	var stored string
	if err := e.s.db.QueryRow(`SELECT permission_config FROM placeholder_keys WHERE id='ph-pt'`).Scan(&stored); err != nil || stored != ptCustomRule {
		t.Fatalf("custom rule not preserved verbatim: %v\n%s", err, stored)
	}
	// the page embeds the stored config for the builder without altering it
	rec := e.do(t, http.MethodGet, "/admin/phantom-tokens/ph-pt", "", true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `\"deny_all_other\":true`) && !strings.Contains(rec.Body.String(), `deny_all_other"`) && !strings.Contains(rec.Body.String(), "deny_all_other") {
		t.Fatalf("detail page does not carry custom rule: %d", rec.Code)
	}
}

func TestPhantomTokenPagesAuthAnd404(t *testing.T) {
	e := newPTEnv(t)
	for _, p := range []string{"/admin/phantom-tokens", "/admin/phantom-tokens/ph-pt"} {
		anon := e.do(t, http.MethodGet, p, "", false)
		if anon.Code == http.StatusOK || strings.Contains(anon.Body.String(), "PT_KEY") {
			t.Errorf("%s served to anonymous caller (status %d)", p, anon.Code)
		}
	}
	rec := e.do(t, http.MethodGet, "/admin/phantom-tokens/does-not-exist", "", true)
	body := rec.Body.String()
	if rec.Code != http.StatusNotFound || !strings.Contains(body, `class="sidebar"`) || !strings.Contains(body, "Phantom token not found") || !strings.Contains(body, "</html>") {
		t.Fatalf("unknown id: %d (want 404 in shell)", rec.Code)
	}
	checkPage(t, rec)
}

func TestPhantomTokenPagesRenderMaskedAndEscaped(t *testing.T) {
	e := newPTEnv(t)
	for _, p := range []string{"/admin/phantom-tokens", "/admin/phantom-tokens/ph-pt", "/admin/phantom-tokens/ph-xss"} {
		rec := e.do(t, http.MethodGet, p, "", true)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, ptFullToken) {
			t.Errorf("%s exposes the full phantom token", p)
		}
		if p != "/admin/phantom-tokens/ph-xss" && !strings.Contains(body, "sk-dwp…mnop") {
			t.Errorf("%s missing masked token", p)
		}
		for _, raw := range []string{"<script>alert(1)</script>", "<i>client</i>", "<b>Service</b>"} {
			if strings.Contains(body, raw) {
				t.Errorf("%s contains unescaped %q", p, raw)
			}
		}
		checkPage(t, rec)
	}
	list := e.do(t, http.MethodGet, "/admin/phantom-tokens", "", true).Body.String()
	if !strings.Contains(list, `href="/admin/phantom-tokens/ph-pt"`) {
		t.Error("list does not link to detail")
	}
	if !strings.Contains(list, "1 rule") {
		t.Error("list missing rules summary")
	}
}

func TestOldPlaceholdersURLIsMoved(t *testing.T) {
	e := newPTEnv(t)
	rec := e.do(t, http.MethodGet, "/admin/placeholders", "", true)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `href="/admin/phantom-tokens"`) || rec.Header().Get("Location") != "" {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPhantomDetailRequests7dUsesGoCutoff(t *testing.T) {
	e := newPTEnv(t)
	now := time.Now().UTC()
	for _, ts := range []time.Time{now.Add(-1 * time.Hour), now.Add(-6 * 24 * time.Hour), now.Add(-8 * 24 * time.Hour), now.Add(-30 * 24 * time.Hour)} {
		if _, err := e.s.db.Exec(`INSERT INTO request_log (placeholder_id, client_id, service_name, method, path, status_code, created_at) VALUES ('ph-pt','c-pt','svc','GET','/x',200,?)`, ts.Format("2006-01-02 15:04:05")); err != nil {
			t.Fatal(err)
		}
	}
	body := e.do(t, http.MethodGet, "/admin/phantom-tokens/ph-pt", "", true).Body.String()
	if !strings.Contains(body, `"requests_7d":2`) && !strings.Contains(body, `&#34;requests_7d&#34;:2`) {
		t.Errorf("expected requests_7d=2 in detail page data")
	}
}
