package queries

import (
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/models"
)

func newConvUsageQ(t *testing.T) *ConversationUsageQueries {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewConversationUsageQueries(db)
}

func TestConversationUsage_TotalsByKey(t *testing.T) {
	q := newConvUsageQ(t)
	// Two conversations on key k1, one on k2.
	rows := []ConversationUsageRecord{
		{APIKeyID: "k1", ConversationID: "sessA", Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50},
		{APIKeyID: "k1", ConversationID: "sessA", Model: "claude-opus-4-7", InputTokens: 200, OutputTokens: 40},
		{APIKeyID: "k1", ConversationID: "sessB", Model: "claude-opus-4-7", InputTokens: 10, OutputTokens: 5},
		{APIKeyID: "k2", ConversationID: "sessC", Model: "gpt-4o", InputTokens: 7, OutputTokens: 3},
	}
	for i := range rows {
		if err := q.Insert(&rows[i]); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	totals, err := q.TotalsByKey(0)
	if err != nil {
		t.Fatalf("TotalsByKey: %v", err)
	}
	k1 := totals["k1"]
	if k1.Requests != 3 {
		t.Errorf("k1 requests = %d, want 3", k1.Requests)
	}
	if k1.InputTokens != 310 {
		t.Errorf("k1 input = %d, want 310", k1.InputTokens)
	}
	if k1.OutputTokens != 65 {
		t.Errorf("k1 output = %d, want 65", k1.OutputTokens)
	}
	if k1.CacheReadTokens != 50 {
		t.Errorf("k1 cache_read = %d, want 50", k1.CacheReadTokens)
	}
	if k1.Conversations != 2 {
		t.Errorf("k1 conversations = %d, want 2 (sessA, sessB)", k1.Conversations)
	}
	if totals["k2"].Requests != 1 || totals["k2"].InputTokens != 7 {
		t.Errorf("k2 totals wrong: %+v", totals["k2"])
	}
}

func TestConversationUsage_ByKey(t *testing.T) {
	q := newConvUsageQ(t)
	_ = q.Insert(&ConversationUsageRecord{APIKeyID: "k1", ConversationID: "big", Model: "m", InputTokens: 1000, OutputTokens: 500})
	_ = q.Insert(&ConversationUsageRecord{APIKeyID: "k1", ConversationID: "big", Model: "m", InputTokens: 1000, OutputTokens: 500})
	_ = q.Insert(&ConversationUsageRecord{APIKeyID: "k1", ConversationID: "small", Model: "m", InputTokens: 10, OutputTokens: 5})

	rows, err := q.ByKey("k1", 0)
	if err != nil {
		t.Fatalf("ByKey: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d conversation rows, want 2: %+v", len(rows), rows)
	}
	// Busiest (most tokens) first.
	if rows[0].ConversationID != "big" {
		t.Errorf("first row = %q, want big", rows[0].ConversationID)
	}
	if rows[0].Requests != 2 || rows[0].InputTokens != 2000 || rows[0].OutputTokens != 1000 {
		t.Errorf("big row wrong: %+v", rows[0])
	}
	if rows[1].ConversationID != "small" {
		t.Errorf("second row = %q, want small", rows[1].ConversationID)
	}
}

func TestConversationUsage_ClientKeyUsage(t *testing.T) {
	q := newConvUsageQ(t)
	svcQ := NewServiceQueries(q.db)
	clientQ := NewClientQueries(q.db)
	apiKeyQ := NewAPIKeyQueries(q.db)

	_ = svcQ.Create(&models.Service{ID: "svc-anth", Name: "anthropic", UpstreamURL: "https://api.anthropic.com", HostPattern: "api.anthropic.com"})
	_ = clientQ.Create(&models.Client{ID: "c1", ShortID: "client", Name: "laptop", TokenHash: "hash", CanaryEnabled: true})
	_ = apiKeyQ.Create(&models.APIKey{ID: "k1", ServiceID: "svc-anth", Name: "shared", KeyEncrypted: "x"})
	_ = q.Insert(&ConversationUsageRecord{ClientID: "c1", APIKeyID: "k1", ServiceName: "anthropic", ConversationID: "s1", InputTokens: 100, OutputTokens: 20})
	_ = q.Insert(&ConversationUsageRecord{ClientID: "c1", APIKeyID: "k1", ServiceName: "anthropic", ConversationID: "s2", InputTokens: 30, OutputTokens: 10, CacheReadTokens: 5})
	if _, err := q.db.Exec(
		`INSERT INTO conversation_usage (client_id, api_key_id, service_name, conversation_id, input_tokens, output_tokens, created_at)
		 VALUES ('c1','k1','anthropic','old',999,1, datetime('now','-10 days'))`); err != nil {
		t.Fatal(err)
	}

	rows, err := q.ClientKeyUsage(7)
	if err != nil {
		t.Fatalf("ClientKeyUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.ClientName != "laptop" || got.KeyName != "shared" || got.ServiceName != "anthropic" {
		t.Fatalf("joined names wrong: %+v", got)
	}
	if got.Requests != 2 || got.InputTokens != 130 || got.OutputTokens != 30 || got.CacheReadTokens != 5 {
		t.Fatalf("totals wrong: %+v", got)
	}
	if got.Conversations != 2 {
		t.Fatalf("conversations = %d, want 2", got.Conversations)
	}
}

func TestConversationUsage_PruneOlderThan(t *testing.T) {
	q := newConvUsageQ(t)
	// Insert a fresh row + an old one (createdAt overridden via raw SQL).
	_ = q.Insert(&ConversationUsageRecord{APIKeyID: "k1", ConversationID: "new", InputTokens: 1})
	if _, err := q.db.Exec(
		`INSERT INTO conversation_usage (api_key_id, conversation_id, input_tokens, created_at)
		 VALUES ('k1','old',1, datetime('now','-40 days'))`); err != nil {
		t.Fatal(err)
	}

	n, err := q.PruneOlderThan(30)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1 (the 40-day-old one)", n)
	}
	totals, _ := q.TotalsByKey(0)
	if totals["k1"].Requests != 1 {
		t.Errorf("after prune k1 requests = %d, want 1", totals["k1"].Requests)
	}
}
