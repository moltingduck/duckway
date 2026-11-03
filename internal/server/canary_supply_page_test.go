package server

import (
	"encoding/json"
	"errors"
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
	// (Closing the DB would now sign the admin out first: the session epoch cannot
	// be read, so the session fails closed. Fail only the write instead.)
	svcExec(t, s, `CREATE TRIGGER fail_settings_ins BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	svcExec(t, s, `CREATE TRIGGER fail_settings_upd BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT, 'boom'); END`)
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

func TestCanaryOverviewExclusionByIDUnreadableAndOrder(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "bravo", "tok1")
	seedDetailClient(t, s, "c2", "Alpha", "tok2")
	seedDetailClient(t, s, "c3", "charlie", "tok3")
	if rec := svcDo(s, shared, http.MethodPost, "/api/canary/settings", `{"email":"a@b.co","exclude_clients":["c3"]}`, true); rec.Code != 200 {
		t.Fatalf("save=%d", rec.Code)
	}
	type ovT struct {
		Unreadable bool `json:"exclusion_unreadable"`
		Clients    []struct {
			Name     string `json:"name"`
			ClientID string `json:"client_id"`
			Excluded bool   `json:"excluded"`
		} `json:"clients"`
	}
	get := func() ovT {
		rec := svcDo(s, shared, http.MethodGet, "/api/canary/overview", "", true)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("overview=%d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
		}
		var o ovT
		if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	o := get()
	if len(o.Clients) != 3 || o.Clients[0].Name != "Alpha" || o.Clients[1].Name != "bravo" {
		t.Fatalf("want case-insensitive name order, got %+v", o.Clients)
	}
	for _, c := range o.Clients {
		if c.Excluded != (c.ClientID == "c3") {
			t.Fatalf("exclusion by ID: %+v", c)
		}
	}
	svcExec(t, s, `UPDATE canary_settings SET exclude_clients = 'not json' WHERE id = 'default'`)
	if o := get(); !o.Unreadable {
		t.Fatal("unparseable exclusion list must be surfaced")
	}
}

func TestCanaryGenerateExcludedIs409AndResponsesNoStore(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "alpha", "tok1")
	svcExec(t, s, `INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, memo, deploy_path, deploy_content) VALUES ('t1','c1','dns','','','v','m','.x/a','c')`)
	if rec := svcDo(s, shared, http.MethodPost, "/api/canary/settings", `{"email":"a@b.co","exclude_clients":["c1"]}`, true); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	rec := svcDo(s, shared, http.MethodPost, "/api/canary/clients/c1/generate?name=alpha", "", true)
	if rec.Code != http.StatusConflict || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("excluded => %d cache=%q %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	// canary_enabled=false is enforced server-side too
	svcExec(t, s, `UPDATE canary_settings SET exclude_clients = '[]' WHERE id = 'default'`)
	svcExec(t, s, `UPDATE clients SET canary_enabled = 0 WHERE id = 'c1'`)
	rec = svcDo(s, shared, http.MethodPost, "/api/canary/clients/c1/generate?name=alpha", "", true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("canary disabled => %d %s", rec.Code, rec.Body.String())
	}
	rec = svcDo(s, shared, http.MethodGet, "/api/canary/clients/c1", "", true)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token list cache=%q", rec.Header().Get("Cache-Control"))
	}
}

func TestCanaryGenerateIgnoresNameQueryAndResolvesServerSide(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "alpha", "tok1")
	if rec := svcDo(s, shared, http.MethodPost, "/api/canary/settings", `{"email":"a@b.co","exclude_clients":["alpha"]}`, true); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	// A spoofed ?name= must not dodge the exclusion that matches the real name.
	rec := svcDo(s, shared, http.MethodPost, "/api/canary/clients/c1/generate?name=not-excluded", "", true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("spoofed name => %d %s, want 409", rec.Code, rec.Body.String())
	}
	if rec := svcDo(s, shared, http.MethodPost, "/api/canary/clients/nope/generate?name=alpha", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown client => %d", rec.Code)
	}
}

func TestCanaryDeleteEndpointsKeepRowsOnRemoteFailureAndLocalOnly(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "alpha", "tok1")
	svcExec(t, s, `INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, memo, deploy_path, deploy_content) VALUES ('t1','c1','env_file','TOK','AUTH','v','m','.x/a','c')`)
	// Inject a failing transport: the test never touches the network.
	shared.CanarySvc.SetHTTPClient(&http.Client{Transport: failingRT{}})
	rec := svcDo(s, shared, http.MethodDelete, "/api/canary/tokens/t1", "", true)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("token delete with failing remote => %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM canary_tokens WHERE id='t1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("row must be kept, n=%d", n)
	}
	if rec := svcDo(s, shared, http.MethodDelete, "/api/canary/clients/c1", "", true); rec.Code != http.StatusBadGateway {
		t.Fatalf("client delete => %d", rec.Code)
	}
	if rec := svcDo(s, shared, http.MethodDelete, "/api/canary/tokens/t1?local_only=1", "", true); rec.Code != 200 {
		t.Fatalf("local_only => %d %s", rec.Code, rec.Body.String())
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM canary_tokens WHERE id='t1'`).Scan(&n)
	if n != 0 {
		t.Fatal("local_only must remove the row")
	}
}

type failingRT struct{}

func (failingRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled in test")
}

func TestCanaryGenerateRejectsDraftClient(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c3", "drafty", "tok3")
	svcExec(t, s, `INSERT INTO client_drafts (client_id) VALUES ('c3')`)
	if rec := svcDo(s, shared, http.MethodPost, "/api/canary/settings", `{"email":"a@b.co"}`, true); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	shared.CanarySvc.SetHTTPClient(&http.Client{Transport: failingRT{}})
	if rec := svcDo(s, shared, http.MethodPost, "/api/canary/clients/c3/generate", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("draft client => %d, want 404", rec.Code)
	}
}

func TestCanarySettingsEmailValidatedServerSide(t *testing.T) {
	s, shared := newClientDetailServer(t)
	for _, bad := range []string{"not-an-email", "a@", "Bob <a@b.co>", "Bob<a@b.co>", "<a@b.co>", "a@b.co, c@d.co"} {
		body, _ := json.Marshal(map[string]string{"email": bad})
		rec := svcDo(s, shared, http.MethodPost, "/api/canary/settings", string(body), true)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"email"`) {
			t.Errorf("%q => %d %s, want 400 with field=email", bad, rec.Code, rec.Body.String())
		}
	}
	for _, good := range []string{"a@b.co", "you+tag@example.com", ""} {
		body, _ := json.Marshal(map[string]string{"email": good})
		if rec := svcDo(s, shared, http.MethodPost, "/api/canary/settings", string(body), true); rec.Code != 200 {
			t.Errorf("%q => %d %s, want 200", good, rec.Code, rec.Body.String())
		}
	}
}

func TestClientDeleteReportsRemoteCanaryFailures(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "alpha", "tok1")
	seedDetailClient(t, s, "c2", "beta", "tok2")
	svcExec(t, s, `INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, memo, deploy_path, deploy_content) VALUES ('t1','c1','env_file','TOK','AUTH','v','m','.x/a','c'),('t2','c1','env_file','TOK2','AUTH2','v','m','.x/b','c')`)
	shared.CanarySvc.SetHTTPClient(&http.Client{Transport: failingRT{}})

	rec := svcDo(s, shared, http.MethodDelete, "/api/clients/c1", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete => %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Failed int    `json:"canary_remote_failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != "deleted" || resp.Failed != 2 {
		t.Fatalf("response = %s (%v)", rec.Body.String(), err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM canary_tokens WHERE client_id='c1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("local canary rows must be removed with the client, n=%d", n)
	}
	// A client without canary tokens reports zero.
	rec = svcDo(s, shared, http.MethodDelete, "/api/clients/c2", "", true)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 || resp.Failed != 0 {
		t.Fatalf("clean delete = %d %s", rec.Code, rec.Body.String())
	}
}
