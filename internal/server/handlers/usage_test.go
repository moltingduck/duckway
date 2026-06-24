package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

type usageTestDeps struct {
	h       *UsageHandler
	apiKeyQ *queries.APIKeyQueries
	svcQ    *queries.ServiceQueries
	logQ    *queries.RequestLogQueries
	clientQ *queries.ClientQueries
	convQ   *queries.ConversationUsageQueries
}

func newUsageTestDeps(t *testing.T) usageTestDeps {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	d := usageTestDeps{
		apiKeyQ: queries.NewAPIKeyQueries(db),
		svcQ:    queries.NewServiceQueries(db),
		logQ:    queries.NewRequestLogQueries(db),
		clientQ: queries.NewClientQueries(db),
		convQ:   queries.NewConversationUsageQueries(db),
	}
	d.h = NewUsageHandler(d.apiKeyQ, d.logQ, d.convQ)
	return d
}

func TestUsageList_ParsesSnapshot(t *testing.T) {
	d := newUsageTestDeps(t)
	h, apiKeyQ, svcQ := d.h, d.apiKeyQ, d.svcQ

	if err := svcQ.Create(&models.Service{ID: "svc-anth", Name: "anthropic", UpstreamURL: "https://api.anthropic.com", HostPattern: "api.anthropic.com", IsActive: true}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if err := apiKeyQ.Create(&models.APIKey{ID: "k1", ServiceID: "svc-anth", Name: "primary", KeyEncrypted: "x"}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	// Snapshot: 1000 token limit, 250 remaining → 750 used, 75%.
	snap := `{"updated_at":"2026-05-19T10:00:00Z","provider":"anthropic","metrics":{"tokens":{"limit":1000,"remaining":250,"reset":"2026-05-19T11:00:00Z"}},"subscription":{"5h_status":"allowed_warning"}}`
	if err := apiKeyQ.UpdateUsageSnapshot("k1", snap); err != nil {
		t.Fatalf("update snapshot: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/usage", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []usageRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if !row.HasData {
		t.Error("HasData = false, want true")
	}
	if row.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", row.Provider)
	}
	if len(row.Metrics) != 1 || row.Metrics[0].Name != "tokens" {
		t.Fatalf("metrics = %+v", row.Metrics)
	}
	m := row.Metrics[0]
	if m.Used != 750 {
		t.Errorf("used = %d, want 750", m.Used)
	}
	if m.UsedPct < 74.9 || m.UsedPct > 75.1 {
		t.Errorf("used_pct = %v, want ~75", m.UsedPct)
	}
	if row.Subscription["5h_status"] != "allowed_warning" {
		t.Errorf("subscription not surfaced: %+v", row.Subscription)
	}
}

func TestUsageList_IncludesLLMKeyWithoutData(t *testing.T) {
	d := newUsageTestDeps(t)
	h, apiKeyQ := d.h, d.apiKeyQ
	// openai is an LLM service — its keys should appear even with no
	// snapshot, marked HasData=false. The openai service is seeded by
	// migrations as "svc-openai-default", so we just add a key to it.
	_ = apiKeyQ.Create(&models.APIKey{ID: "k2", ServiceID: "svc-openai-default", Name: "codex-key", KeyEncrypted: "x"})

	req := httptest.NewRequest("GET", "/api/usage", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)

	var rows []usageRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].KeyName != "codex-key" {
		t.Fatalf("expected the codex-key row, got %+v", rows)
	}
	if rows[0].HasData {
		t.Error("HasData should be false for a key with no snapshot")
	}
}

func TestUsageList_ExcludesNonLLMKeyWithoutData(t *testing.T) {
	d := newUsageTestDeps(t)
	h, apiKeyQ, svcQ := d.h, d.apiKeyQ, d.svcQ
	// github is not an LLM service and has no snapshot → must NOT appear.
	_ = svcQ.Create(&models.Service{ID: "svc-gh", Name: "github", UpstreamURL: "https://api.github.com", HostPattern: "api.github.com", IsActive: true})
	_ = apiKeyQ.Create(&models.APIKey{ID: "k3", ServiceID: "svc-gh", Name: "gh-token", KeyEncrypted: "x"})

	req := httptest.NewRequest("GET", "/api/usage", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)

	var rows []usageRow
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	for _, r := range rows {
		if r.KeyName == "gh-token" {
			t.Errorf("non-LLM key without data should be excluded; got %+v", r)
		}
	}
}

func TestUsageList_SurfacesTokenTotals(t *testing.T) {
	d := newUsageTestDeps(t)
	h, apiKeyQ, svcQ, convQ := d.h, d.apiKeyQ, d.svcQ, d.convQ
	_ = svcQ.Create(&models.Service{ID: "svc-anth", Name: "anthropic", UpstreamURL: "https://api.anthropic.com", HostPattern: "api.anthropic.com", IsActive: true})
	_ = apiKeyQ.Create(&models.APIKey{ID: "k1", ServiceID: "svc-anth", Name: "primary", KeyEncrypted: "x"})

	// Insert two captured requests (same conversation) for k1.
	_ = convQ.Insert(&queries.ConversationUsageRecord{APIKeyID: "k1", ConversationID: "sess1", Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 20})
	_ = convQ.Insert(&queries.ConversationUsageRecord{APIKeyID: "k1", ConversationID: "sess1", Model: "claude-opus-4-7", InputTokens: 200, OutputTokens: 30})

	req := httptest.NewRequest("GET", "/api/usage", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)

	var rows []usageRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.TokensInput != 300 || r.TokensOutput != 50 {
		t.Errorf("token totals wrong: in=%d out=%d", r.TokensInput, r.TokensOutput)
	}
	if r.CapturedRequests != 2 || r.Conversations != 1 {
		t.Errorf("captured=%d conversations=%d, want 2/1", r.CapturedRequests, r.Conversations)
	}
	if !r.HasData {
		t.Error("HasData should be true once token data exists")
	}
}

func TestUsageConversations_DrillDown(t *testing.T) {
	d := newUsageTestDeps(t)
	h, convQ := d.h, d.convQ
	_ = convQ.Insert(&queries.ConversationUsageRecord{APIKeyID: "k1", ConversationID: "big", Model: "m", InputTokens: 1000, OutputTokens: 500})
	_ = convQ.Insert(&queries.ConversationUsageRecord{APIKeyID: "k1", ConversationID: "small", Model: "m", InputTokens: 5, OutputTokens: 2})

	req := httptest.NewRequest("GET", "/api/usage/conversations?key_id=k1", nil)
	rec := httptest.NewRecorder()
	h.Conversations(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var rows []queries.ConversationUsageRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ConversationID != "big" {
		t.Fatalf("expected big-first, got %+v", rows)
	}
}

func TestUsageConversations_RequiresKeyID(t *testing.T) {
	h := newUsageTestDeps(t).h
	req := httptest.NewRequest("GET", "/api/usage/conversations", nil)
	rec := httptest.NewRecorder()
	h.Conversations(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing key_id: status %d, want 400", rec.Code)
	}
}

func TestUsageSessions_Aggregates(t *testing.T) {
	d := newUsageTestDeps(t)
	h, logQ, clientQ := d.h, d.logQ, d.clientQ
	_ = clientQ.Create(&models.Client{ID: "c1", Name: "laptop"})

	// 2 OK + 1 error for client c1 / anthropic.
	_ = logQ.Log("c1", "", "anthropic", "POST", "/v1/messages", 200)
	_ = logQ.Log("c1", "", "anthropic", "POST", "/v1/messages", 200)
	_ = logQ.Log("c1", "", "anthropic", "POST", "/v1/messages", 429)
	// 1 OK for c1 / openai.
	_ = logQ.Log("c1", "", "openai", "POST", "/v1/chat/completions", 200)

	req := httptest.NewRequest("GET", "/api/usage/sessions?hours=0", nil)
	rec := httptest.NewRecorder()
	h.Sessions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var rows []queries.SessionUsageRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (anthropic + openai): %+v", len(rows), rows)
	}
	// Busiest first → anthropic (3 requests) before openai (1).
	if rows[0].ServiceName != "anthropic" || rows[0].Requests != 3 || rows[0].Errors != 1 {
		t.Errorf("anthropic row wrong: %+v", rows[0])
	}
	if rows[0].ClientName != "laptop" {
		t.Errorf("client_name = %q, want laptop", rows[0].ClientName)
	}
	if rows[1].ServiceName != "openai" || rows[1].Requests != 1 || rows[1].Errors != 0 {
		t.Errorf("openai row wrong: %+v", rows[1])
	}
}
