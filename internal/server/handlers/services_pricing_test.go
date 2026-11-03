package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

func TestServiceMetadataAndPricingHandlers(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	serviceQ := queries.NewServiceQueries(db)
	h := NewServiceHandler(serviceQ, queries.NewModelPricingQueries(db))
	if err := serviceQ.Create(&models.Service{ID: "svc-metered", Name: "metered", UpstreamURL: "https://example.test", HostPattern: "example.test", Category: "llm", UsageMetering: `{"provider":"test"}`, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	stored, err := serviceQ.GetByID("svc-metered")
	if err != nil || stored.Category != "llm" || stored.UsageMetering != `{"provider":"test"}` {
		t.Fatalf("metadata did not round trip: %+v err=%v", stored, err)
	}

	body := []byte(`{"model":"model-a","version":"2026-01","input_usd_micros_per_mtok":2000000,"effective_from":"2026-01-01T00:00:00Z"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/services/svc-metered/pricing", bytes.NewReader(body))
	req.SetPathValue("id", "svc-metered")
	rec := httptest.NewRecorder()
	h.CreatePricing(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pricing status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created models.ModelPricing
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ServiceID != "svc-metered" || created.Version != "2026-01" {
		t.Fatalf("unexpected pricing response: %+v", created)
	}

	badReq := httptest.NewRequest(http.MethodPut, "/api/services/svc-metered", bytes.NewBufferString(`{"usage_metering":"[]"}`))
	badReq.SetPathValue("id", "svc-metered")
	badRec := httptest.NewRecorder()
	h.Update(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid usage metadata status=%d body=%s", badRec.Code, badRec.Body.String())
	}
}

func TestListPricingReportsUsageReferences(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	serviceQ := queries.NewServiceQueries(db)
	pricingQ := queries.NewModelPricingQueries(db)
	if err := serviceQ.Create(&models.Service{ID: "svc-ref", Name: "refsvc", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []*models.ModelPricing{
		{ID: "pr1", ServiceID: "svc-ref", Model: "m-ref", Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z"},
		{ID: "pr2", ServiceID: "svc-ref", Model: "m-free", Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z"},
	} {
		if err := pricingQ.Create(row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO conversation_usage (client_id, api_key_id, service_name, model, pricing_version, input_tokens, output_tokens, created_at)
		VALUES ('c', 'k', 'refsvc', 'm-ref', 'v1', 1, 1, '2026-02-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	h := NewServiceHandler(serviceQ, pricingQ)
	req := httptest.NewRequest(http.MethodGet, "/api/services/svc-ref/pricing", nil)
	req.SetPathValue("id", "svc-ref")
	rec := httptest.NewRecorder()
	h.ListPricing(rec, req)
	var rows []struct {
		ID              string `json:"id"`
		Model           string `json:"model"`
		UsageReferences int64  `json:"usage_references"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	for _, r := range rows {
		want := int64(0)
		if r.ID == "pr1" {
			want = 1
		}
		if r.UsageReferences != want || r.Model == "" {
			t.Errorf("row %s: usage_references=%d want %d model=%q", r.ID, r.UsageReferences, want, r.Model)
		}
	}
}
