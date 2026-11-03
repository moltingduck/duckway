package queries

import (
	"strings"
	"testing"
)

// The explicit window path must use only portable constructs: two bound
// parameters compared against Go-computed UTC strings, no SQLite date
// functions, so it runs unchanged on PostgreSQL.
func TestUsageCutoffsWindowIsPortable(t *testing.T) {
	cond, args := usageCutoffs("u.created_at", 24, &UsageWindow{Since: "2025-03-10 00:00:00", Until: "2025-03-13 00:00:00"})
	if cond != "u.created_at >= ? AND u.created_at < ?" || len(args) != 2 {
		t.Fatalf("cond=%q args=%v", cond, args)
	}
	for _, bad := range []string{"datetime", "strftime", "julianday", "date("} {
		if strings.Contains(cond, bad) {
			t.Errorf("window predicate uses %s", bad)
		}
	}
	if c, a := usageCutoffs("x", 0, nil); c != "" || a != nil {
		t.Errorf("no filter expected, got %q %v", c, a)
	}
}
