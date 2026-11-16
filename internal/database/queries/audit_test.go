package queries

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
)

func auditDB(t *testing.T) (*AuditQueries, *sql.DB) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewAuditQueries(db), db
}

func TestAuditListFiltersAndCursor(t *testing.T) {
	q, _ := auditDB(t)
	now := time.Now()
	add := func(actor, action, typ, id, summary string, age time.Duration) {
		if _, err := q.Insert(AuditRow{CreatedAt: utcFormat(now.Add(-age)), Actor: actor, Action: action, ResourceType: typ, ResourceID: id, ResourceName: id, Summary: summary}); err != nil {
			t.Fatal(err)
		}
	}
	add("admin:a", "service.update", "service", "s1", "update service s1", time.Hour)
	add("admin:a", "api_key.update", "api_key", "k1", "update credential k1", 2*time.Hour)
	add("admin:b", "api_key.acl_update", "api_key", "k1", "acl", 3*time.Hour)
	add("admin:b", "client.create", "client", "c1", "create client c1", 4*time.Hour)
	add("anonymous", "signin.failed", "signin", "", "failed sign-in", 5*time.Hour)
	add("admin:a", "approval.approve", "placeholder", "p1", "approved request", 6*time.Hour)
	add("admin:a", "placeholder.update", "placeholder", "p1", "update 100%_done", 7*time.Hour)
	add("admin:a", "setting.update", "setting", "", "changed gateway_url", 400*24*time.Hour)

	count := func(f AuditFilter) int {
		f.Limit = 200
		rows, _, err := q.List(f)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	checks := map[string]struct {
		f    AuditFilter
		want int
	}{
		"all":                 {AuditFilter{}, 8},
		"actor":               {AuditFilter{Actor: "admin:b"}, 2},
		"kind rules":          {AuditFilter{Kind: "rules"}, 3}, // service, key acl, placeholder (approval excluded)
		"kind credentials":    {AuditFilter{Kind: "credentials"}, 2},
		"kind clients":        {AuditFilter{Kind: "clients"}, 1},
		"kind signins":        {AuditFilter{Kind: "signins"}, 1},
		"kind approvals":      {AuditFilter{Kind: "approvals"}, 1},
		"kind settings":       {AuditFilter{Kind: "settings"}, 1},
		"since":               {AuditFilter{Since: utcFormat(now.Add(-90 * time.Minute))}, 1},
		"resource":            {AuditFilter{ResourceType: "api_key", ResourceID: "k1"}, 2},
		"history of token":    {AuditFilter{ResourceType: "placeholder", ResourceID: "p1"}, 2},
		"q summary":           {AuditFilter{Query: "CREDENTIAL"}, 1},
		"q literal percent":   {AuditFilter{Query: "100%"}, 1},
		"q underscore escape": {AuditFilter{Query: "%_"}, 1},
	}
	for name, c := range checks {
		if got := count(c.f); got != c.want {
			t.Errorf("%s: got %d want %d", name, got, c.want)
		}
	}

	var seen []int64
	cursor := int64(0)
	for i := 0; i < 10; i++ {
		rows, next, err := q.List(AuditFilter{Limit: 3, BeforeID: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen = append(seen, r.ID)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if len(seen) != 8 {
		t.Fatalf("cursor paging saw %d rows: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("not strictly newest-first: %v", seen)
		}
	}
}

func TestAuditRetentionPrune(t *testing.T) {
	q, db := auditDB(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		_, _ = q.Insert(AuditRow{CreatedAt: utcFormat(now.AddDate(0, 0, -400-i)), Action: "old"})
	}
	_, _ = q.Insert(AuditRow{CreatedAt: utcFormat(now.AddDate(0, 0, -10)), Action: "recent"})
	n, err := q.PruneOlderThan(365, 2, 100) // batches of 2 exercise the loop
	if err != nil || n != 5 {
		t.Fatalf("pruned %d, err %v", n, err)
	}
	var left int
	_ = db.QueryRow(`SELECT COUNT(*) FROM admin_audit_log`).Scan(&left)
	if left != 1 {
		t.Fatalf("rows left = %d", left)
	}
	if n, _ := q.PruneOlderThan(0, 10, 1); n != 0 {
		t.Fatalf("days<1 must delete nothing")
	}
	_ = fmt.Sprint()
}
