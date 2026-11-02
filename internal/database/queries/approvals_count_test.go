package queries

import "testing"

func TestApprovalCountPending(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`CREATE TABLE approvals (id TEXT PRIMARY KEY, placeholder_id TEXT, status TEXT, approved_at TEXT, expires_at TEXT, request_info TEXT, created_at TEXT DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for id, st := range map[string]string{"1": "pending", "2": "pending", "3": "approved", "4": "rejected"} {
		if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id, status) VALUES (?, 'p', ?)`, id, st); err != nil {
			t.Fatal(err)
		}
	}
	n, err := NewApprovalQueries(db).CountPending()
	if err != nil || n != 2 {
		t.Fatalf("CountPending = %d, %v; want 2", n, err)
	}
}
