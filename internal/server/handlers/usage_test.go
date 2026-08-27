package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

type usageTestDeps struct {
	db      *sql.DB
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
		db:      db,
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

func TestUsageClients_AggregatesClientKeyUsage(t *testing.T) {
	d := newUsageTestDeps(t)
	h, apiKeyQ, svcQ, clientQ, convQ := d.h, d.apiKeyQ, d.svcQ, d.clientQ, d.convQ
	_ = svcQ.Create(&models.Service{ID: "svc-anth", Name: "anthropic", UpstreamURL: "https://api.anthropic.com", HostPattern: "api.anthropic.com", IsActive: true})
	_ = clientQ.Create(&models.Client{ID: "c1", ShortID: "lap123", Name: "laptop", TokenHash: "hash", CanaryEnabled: true})
	_ = apiKeyQ.Create(&models.APIKey{ID: "k1", ServiceID: "svc-anth", Name: "shared", KeyEncrypted: "x"})
	snap := `{"updated_at":"2026-05-19T10:00:00Z","provider":"anthropic","metrics":{"tokens":{"limit":1000,"remaining":100,"reset":"2026-05-19T11:00:00Z"}}}`
	_ = apiKeyQ.UpdateUsageSnapshot("k1", snap)
	_ = convQ.Insert(&queries.ConversationUsageRecord{ClientID: "c1", APIKeyID: "k1", ServiceName: "anthropic", ConversationID: "sess1", InputTokens: 100, OutputTokens: 20})
	_ = convQ.Insert(&queries.ConversationUsageRecord{ClientID: "c1", APIKeyID: "k1", ServiceName: "anthropic", ConversationID: "sess2", InputTokens: 200, OutputTokens: 30, CacheReadTokens: 10})

	req := httptest.NewRequest("GET", "/api/usage/clients?days=3", nil)
	rec := httptest.NewRecorder()
	h.Clients(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var rows []clientUsageView
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.ClientName != "laptop" || got.TotalTokens != 360 || got.Requests != 2 || got.KeysUsed != 1 {
		t.Fatalf("client totals wrong: %+v", got)
	}
	if got.Status != "near shared key limit" {
		t.Fatalf("status = %q, want near shared key limit", got.Status)
	}
	if len(got.Keys) != 1 || got.Keys[0].KeyName != "shared" || got.Keys[0].MaxUsedPct < 89.9 {
		t.Fatalf("key breakdown wrong: %+v", got.Keys)
	}
}

func TestUsageKeyAndGroupDetailAPIs(t *testing.T) {
	d := newUsageTestDeps(t)
	_ = d.svcQ.Create(&models.Service{ID: "svc-detail-api", Name: "detail-api", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true})
	_ = d.clientQ.Create(&models.Client{ID: "client-detail-api", ShortID: "detapi", Name: "agent", TokenHash: "hash"})
	_ = d.apiKeyQ.Create(&models.APIKey{ID: "key-detail-api", ServiceID: "svc-detail-api", Name: "key", KeyEncrypted: "x"})
	group, err := queries.CreateKeyGroup(d.db, "detail group", "", "detail-api", "score")
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.AddKeyToGroup(d.db, group.ID, "key-detail-api", 0); err != nil {
		t.Fatal(err)
	}
	_ = d.convQ.Insert(&queries.ConversationUsageRecord{ClientID: "client-detail-api", APIKeyID: "key-detail-api", KeyGroupID: group.ID, Provider: "test", Model: "m", InputTokens: 8, OutputTokens: 2})

	for _, tc := range []struct {
		path string
		id   string
		call func(http.ResponseWriter, *http.Request)
	}{
		{"/api/usage/keys/key-detail-api/detail?days=90", "key-detail-api", d.h.KeyDetail},
		{"/api/usage/key-groups/" + group.ID + "/detail?days=90", group.ID, d.h.KeyGroupDetail},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.SetPathValue("id", tc.id)
		rec := httptest.NewRecorder()
		tc.call(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.path, rec.Code, rec.Body.String())
		}
		var rows []queries.MeteredUsageDetailRow
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ClientName != "agent" || rows[0].BillableTokens != 10 {
			t.Fatalf("%s rows=%+v", tc.path, rows)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/usage/detail?key_id=key-detail-api&days=90", nil)
	rec := httptest.NewRecorder()
	d.h.Detail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail meteredUsageDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.WindowDays != 90 || detail.Summary.Requests != 1 || detail.Summary.BillableTokens != 10 ||
		len(detail.Clients) != 1 || len(detail.Models) != 1 || len(detail.Daily) != 1 {
		t.Fatalf("unexpected detail response: %+v", detail)
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
