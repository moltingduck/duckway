package queries

import (
	"context"
	"fmt"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
)

func TestReplayGroupsHonoursCancelledContext(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := NewRequestLogQueries(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	groups, _, truncated, err := q.ReplayGroups(ctx, []string{"ph1"}, ReplayWindowDays, 10)
	if err != nil || !truncated || len(groups) != 0 {
		t.Fatalf("cancelled: groups=%d truncated=%v err=%v", len(groups), truncated, err)
	}
	if _, _, truncated, err := q.ReplayGroups(context.Background(), []string{"ph1"}, ReplayWindowDays, 10); err != nil || truncated {
		t.Fatalf("live ctx on an empty log: truncated=%v err=%v", truncated, err)
	}
}

// maxRows caps the denial rows read across all chunks of placeholder ids.
func TestAttachReplayRefsMaxRowsIsATotalCap(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil { // the groups name tokens that need not exist
		t.Fatal(err)
	}
	q := NewRequestLogQueries(db)
	const n = 450 // three chunks of replayChunk=200 ids
	var groups []ReplayGroup
	detail := `{"kind":"branch_rules","refs":[{"ref":"refs/heads/x","verdict":"refused"}]}`
	for i := 0; i < n; i++ {
		ph := fmt.Sprintf("ph%03d", i)
		path := "/o/r.git/git-receive-pack"
		if _, err := db.Exec(`INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code, decision, deny_detail, repeat_count)
			VALUES ('', ?, 'github', 'POST', ?, 403, ?, ?, 1)`, ph, path, DecisionACLDenied, detail); err != nil {
			t.Fatal(err)
		}
		groups = append(groups, ReplayGroup{PlaceholderID: ph, Method: "POST", Path: path})
	}
	const cap = 30
	if err := q.AttachReplayRefs(context.Background(), groups, ReplayWindowDays, cap); err != nil {
		t.Fatal(err)
	}
	attached := 0
	for _, g := range groups {
		if len(g.Refs) > 0 {
			attached++
		}
	}
	if attached == 0 || attached > cap {
		t.Fatalf("attached %d groups with cap %d (a per-chunk cap would allow up to %d)", attached, cap, cap*3)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.AttachReplayRefs(ctx, groups, ReplayWindowDays, cap); err != nil {
		t.Fatalf("cancelled ctx must stop quietly: %v", err)
	}
}
