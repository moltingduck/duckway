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

func newUsageTestHandler(t *testing.T) (*UsageHandler, *queries.APIKeyQueries, *queries.ServiceQueries, *queries.RequestLogQueries, *queries.ClientQueries) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	apiKeyQ := queries.NewAPIKeyQueries(db)
	svcQ := queries.NewServiceQueries(db)
	logQ := queries.NewRequestLogQueries(db)
	clientQ := queries.NewClientQueries(db)
	return NewUsageHandler(apiKeyQ, logQ), apiKeyQ, svcQ, logQ, clientQ
}

func TestUsageList_ParsesSnapshot(t *testing.T) {
	h, apiKeyQ, svcQ, _, _ := newUsageTestHandler(t)

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
	h, apiKeyQ, svcQ, _, _ := newUsageTestHandler(t)
	// openai is an LLM service — its keys should appear even with no
	// snapshot, marked HasData=false.
	_ = svcQ.Create(&models.Service{ID: "svc-oai", Name: "openai", UpstreamURL: "https://api.openai.com", HostPattern: "api.openai.com", IsActive: true})
	_ = apiKeyQ.Create(&models.APIKey{ID: "k2", ServiceID: "svc-oai", Name: "codex-key", KeyEncrypted: "x"})

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
	h, apiKeyQ, svcQ, _, _ := newUsageTestHandler(t)
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

func TestUsageSessions_Aggregates(t *testing.T) {
	h, _, _, logQ, clientQ := newUsageTestHandler(t)
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
