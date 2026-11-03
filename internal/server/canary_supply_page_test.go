package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCanaryAndSupplyChainPagesAreCSPClean(t *testing.T) {
	s, shared := newClientDetailServer(t)
	for _, p := range []string{"/admin/canary", "/admin/supply-chain"} {
		rec := svcDo(s, shared, http.MethodGet, p, "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d", p, rec.Code)
		}
		checkPage(t, rec)
		body := rec.Body.String()
		for _, bad := range []string{"style=\"", " onclick=", " onsubmit=", "confirm(", "alert(", "prompt(", "innerHTML"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
	}
	sc := svcDo(s, shared, http.MethodGet, "/admin/supply-chain", "", true).Body.String()
	for _, want := range []string{`role="switch"`, "aria-checked", "data-action=\"scRetry\"", "<details"} {
		if !strings.Contains(sc, want) {
			t.Errorf("supply-chain page missing %q", want)
		}
	}
	cn := svcDo(s, shared, http.MethodGet, "/admin/canary", "", true).Body.String()
	for _, want := range []string{"cn-warn", "cnRegenAll", "cn-confirm", "cnRetryFleet"} {
		if !strings.Contains(cn, want) {
			t.Errorf("canary page missing %q", want)
		}
	}
}

func TestCanaryOverviewAndMaskedTokens(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "alpha", "tok1")
	seedDetailClient(t, s, "c2", "<img src=x onerror=alert(1)>", "tok2")
	seedDetailClient(t, s, "c3", "drafty", "tok3")
	svcExec(t, s, `INSERT INTO client_drafts (client_id) VALUES ('c3')`)
	svcExec(t, s, `INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, secret_value, memo, deploy_path, deploy_content, created_at)
		VALUES ('t1','c1','aws_keys','CANARYID1234567','AUTHSECRET','TOKVALSECRET','SECRETVALSECRET','m','.aws/x.bak','CONTENTSECRET','2026-01-02 03:04:05')`)

	var ov struct {
		AlertConfigured    bool `json:"alert_configured"`
		LastTriggeredKnown bool `json:"last_triggered_known"`
		Clients            []struct {
			ClientID      string `json:"client_id"`
			Name          string `json:"name"`
			TokenCount    int    `json:"token_count"`
			LastCreatedAt string `json:"last_created_at"`
			Excluded      bool   `json:"excluded"`
		} `json:"clients"`
	}
	get := func() {
		rec := svcDo(s, shared, http.MethodGet, "/api/canary/overview", "", true)
		if rec.Code != 200 {
			t.Fatalf("overview=%d %s", rec.Code, rec.Body.String())
		}
		ov.Clients = nil
		if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
			t.Fatal(err)
		}
	}
	get()
	if ov.AlertConfigured || ov.LastTriggeredKnown {
		t.Fatalf("no email set: alert=%v triggered_known=%v", ov.AlertConfigured, ov.LastTriggeredKnown)
	}
	if len(ov.Clients) != 2 {
		t.Fatalf("drafts must be excluded, got %d clients", len(ov.Clients))
	}
	counts := map[string]int{}
	for _, c := range ov.Clients {
		counts[c.ClientID] = c.TokenCount
	}
	if counts["c1"] != 1 || counts["c2"] != 0 {
		t.Fatalf("counts=%v", counts)
	}
	if rec := svcDo(s, shared, http.MethodPost, "/api/canary/settings", `{"email":"a@b.co","exclude_clients":["alpha"]}`, true); rec.Code != 200 {
		t.Fatalf("save=%d", rec.Code)
	}
	get()
	if !ov.AlertConfigured {
		t.Fatal("email set must count as alert configured")
	}
	for _, c := range ov.Clients {
		if c.ClientID == "c1" && !c.Excluded {
			t.Fatal("alpha should be excluded")
		}
	}
	if rec := svcDo(s, shared, http.MethodGet, "/api/canary/overview", "", false); rec.Code == 200 {
		t.Fatal("anonymous overview must not be served")
	}

	rec := svcDo(s, shared, http.MethodGet, "/api/canary/clients/c1", "", true)
	if rec.Code != 200 {
		t.Fatalf("list=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{`"auth_token"`, "TOKVALSECRET", "SECRETVALSECRET", "CONTENTSECRET"} {
		if strings.Contains(body, secret) {
			t.Errorf("token list leaks %s", secret)
		}
	}
	// The manage link carries the auth token exactly as the pre-redesign page
	// did (needed to open canarytokens.org); nothing else exposes it.
	if strings.Count(body, "AUTHSECRET") != 1 || !strings.Contains(body, "manage_url") {
		t.Errorf("auth token should appear only inside manage_url: %s", body)
	}
	if !strings.Contains(body, "CANARYID1234567") || !strings.Contains(body, ".aws/x.bak") {
		t.Errorf("token list lost display fields: %s", body)
	}
}

func TestSupplyChainErrorPaths(t *testing.T) {
	s, shared := newClientDetailServer(t)
	for _, tc := range []struct{ path, body string }{
		{"/api/supply-chain/min-age", `{"days":0}`},
		{"/api/supply-chain/min-age", `{"days":366}`},
		{"/api/supply-chain/min-age", `{"days":"x"}`},
		{"/api/supply-chain/not-a-manager", `{"enabled":true}`},
	} {
		rec := svcDo(s, shared, http.MethodPost, tc.path, tc.body, true)
		if rec.Code < 400 || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s %s => %d %s, want JSON error", tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}
	if rec := svcDo(s, shared, http.MethodGet, "/api/supply-chain", "", false); rec.Code == 200 {
		t.Fatal("anonymous list must not be served")
	}
	// Storage failure surfaces as a 500 JSON error, which the page shows with Retry.
	_ = s.db.Close()
	rec := svcDo(s, shared, http.MethodPost, "/api/supply-chain/min-age", `{"days":5}`, true)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("closed db => %d %s", rec.Code, rec.Body.String())
	}
}

func TestRegenerateEndpointRejectsWithoutEmailAndKeepsTokens(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "alpha", "tok1")
	svcExec(t, s, `INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, memo, deploy_path, deploy_content) VALUES ('t1','c1','dns','','','v','m','.x/a','c')`)
	rec := svcDo(s, shared, http.MethodPost, "/api/canary/clients/c1/generate?name=alpha", "", true)
	if rec.Code < 400 || rec.Code >= 500 || !strings.Contains(rec.Body.String(), "alert email") {
		t.Fatalf("regenerate without email => %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM canary_tokens WHERE client_id='c1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("tokens=%d, want 1 kept", n)
	}
}
