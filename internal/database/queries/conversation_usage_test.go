package queries

import (
	"testing"

	"github.com/hackerduck/duckway/internal/database"
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
