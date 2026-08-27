package queries

import (
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/models"
)

func TestConversationUsageSnapshotsEffectivePricing(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	servicesQ := NewServiceQueries(db)
	keysQ := NewAPIKeyQueries(db)
	pricingQ := NewModelPricingQueries(db)
	usageQ := NewConversationUsageQueries(db)
	if err := servicesQ.Create(&models.Service{ID: "svc-price", Name: "priced", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if err := keysQ.Create(&models.APIKey{ID: "key-price", ServiceID: "svc-price", Name: "key", KeyEncrypted: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*models.ModelPricing{
		{ID: "price-v1", ServiceID: "svc-price", Model: "model-a", Version: "v1", InputUSDMicrosPerMTok: 2_000_000, OutputUSDMicrosPerMTok: 4_000_000, EffectiveFrom: "2026-01-01T00:00:00Z"},
		{ID: "price-v2", ServiceID: "svc-price", Model: "model-a", Version: "v2", InputUSDMicrosPerMTok: 3_000_000, OutputUSDMicrosPerMTok: 5_000_000, EffectiveFrom: "2026-02-01T00:00:00Z"},
	} {
		if err := pricingQ.Create(p); err != nil {
			t.Fatal(err)
		}
	}

	at := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	record := &ConversationUsageRecord{APIKeyID: "key-price", Model: "model-a", InputTokens: 500_000, OutputTokens: 250_000}
	if err := usageQ.InsertAt(record, at); err != nil {
		t.Fatal(err)
	}
	if !record.Priced || record.PricingVersion != "v1" || record.BillableTokens != 750_000 || record.CostUSDMicros != 2_000_000 {
		t.Fatalf("unexpected snapshot: %+v", record)
	}

	var version string
	var cost int64
	if err := db.QueryRow(`SELECT pricing_version, cost_usd_micros FROM conversation_usage`).Scan(&version, &cost); err != nil {
		t.Fatal(err)
	}
	if version != "v1" || cost != 2_000_000 {
		t.Fatalf("stored snapshot version=%q cost=%d", version, cost)
	}
}

func TestMeteredUsageDetailByKeyAndCurrentGroup(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	servicesQ := NewServiceQueries(db)
	keysQ := NewAPIKeyQueries(db)
	clientsQ := NewClientQueries(db)
	usageQ := NewConversationUsageQueries(db)
	_ = servicesQ.Create(&models.Service{ID: "svc-detail", Name: "detail", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true})
	_ = keysQ.Create(&models.APIKey{ID: "key-detail", ServiceID: "svc-detail", Name: "primary", KeyEncrypted: "x"})
	_ = clientsQ.Create(&models.Client{ID: "client-detail", ShortID: "detail", Name: "workstation", TokenHash: "hash"})
	group, err := CreateKeyGroup(db, "group", "", "detail", "score")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddKeyToGroup(db, group.ID, "key-detail", 0); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for _, at := range []time.Time{now.Add(-24 * time.Hour), now.Add(-60 * 24 * time.Hour)} {
		r := &ConversationUsageRecord{ClientID: "client-detail", APIKeyID: "key-detail", KeyGroupID: group.ID, Provider: "test", Model: "model-a", InputTokens: 10, OutputTokens: 2}
		if err := usageQ.InsertAt(r, at); err != nil {
			t.Fatal(err)
		}
	}
	old := &ConversationUsageRecord{ClientID: "client-detail", APIKeyID: "key-detail", KeyGroupID: group.ID, Provider: "test", Model: "model-a", InputTokens: 100}
	if err := usageQ.InsertAt(old, now.Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	keyRows, err := usageQ.DetailByKey("key-detail", 90)
	if err != nil {
		t.Fatal(err)
	}
	groupRows, err := usageQ.DetailByKeyGroup(group.ID, 90)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyRows) != 2 || len(groupRows) != 2 {
		t.Fatalf("key rows=%d group rows=%d, want 2/2", len(keyRows), len(groupRows))
	}
	if keyRows[0].ClientName != "workstation" || keyRows[0].Model != "model-a" || keyRows[0].BillableTokens != 12 {
		t.Fatalf("unexpected detail row: %+v", keyRows[0])
	}
}
