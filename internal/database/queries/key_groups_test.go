package queries_test

import (
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

// Regression: keys with the default empty usage_snapshot (”) made json_extract
// fail with "malformed JSON", so a group with members could not be loaded.
func TestGetKeyGroupWithMembersToleratesEmptyUsageSnapshot(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-kg', 'kg', 'KG', 'https://kg.example', 'kg.example')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k-empty', 'svc-kg', 'empty', 'x')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, usage_snapshot) VALUES ('k-ok', 'svc-kg', 'ok', 'x', '{"tokens_remaining":50,"tokens_limit":100}')`)
	g, err := queries.CreateKeyGroup(db, "grp", "", "kg", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"k-empty", "k-ok"} {
		if err := queries.AddKeyToGroup(db, g.ID, id, i); err != nil {
			t.Fatal(err)
		}
	}
	got, err := queries.GetKeyGroupWithMembers(db, g.ID)
	if err != nil {
		t.Fatalf("GetKeyGroupWithMembers: %v", err)
	}
	if len(got.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(got.Members))
	}
	byID := map[string]int{}
	for i, m := range got.Members {
		byID[m.APIKeyID] = i
	}
	empty, ok := got.Members[byID["k-empty"]], got.Members[byID["k-ok"]]
	if empty.TokensRemaining != nil || empty.TokensLimit != nil || empty.RequestsRemaining != nil || empty.RequestsLimit != nil {
		t.Fatalf("empty snapshot should yield nil token/request fields: %+v", empty)
	}
	if ok.TokensRemaining == nil || *ok.TokensRemaining != 50 || ok.TokensLimit == nil || *ok.TokensLimit != 100 {
		t.Fatalf("valid snapshot not parsed: %+v", ok)
	}
	// Unknown usage is preferred over a known-low remainder.
	for _, strategy := range []string{"score", "round_robin", "failover"} {
		id, err := queries.SelectKeyForGroup(db, g.ID, "", strategy)
		if err != nil {
			t.Fatalf("SelectKeyForGroup(%s): %v", strategy, err)
		}
		if strategy == "score" && id != "k-empty" {
			t.Fatalf("score selected %q, want k-empty (unknown usage preferred)", id)
		}
	}
}
