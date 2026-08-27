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
