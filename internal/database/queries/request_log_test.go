package queries_test

import (
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestRecentReportsWhetherDetailWasCaptured(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO clients (id, name, token_hash) VALUES ('c1', 'c1', 'h1')`); err != nil {
		t.Fatal(err)
	}
	q := queries.NewRequestLogQueries(db)
	withID, err := q.LogWithReturn("c1", "", "svc", "GET", "/with", 200)
	if err != nil {
		t.Fatal(err)
	}
	withoutID, err := q.LogWithReturn("c1", "", "svc", "GET", "/without", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.StoreDetail(&queries.RequestLogDetail{LogID: withID}); err != nil {
		t.Fatal(err)
	}
	rows, err := q.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ID] = r.HasDetail
	}
	if !got[withID] || got[withoutID] {
		t.Fatalf("has_detail = %v (with=%d without=%d)", got, withID, withoutID)
	}
}
