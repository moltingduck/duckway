package queries

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SQLite-only constructs return a 500 on PostgreSQL. There is no live
// PostgreSQL in CI (see DUCKWAY_POSTGRES_TEST_URL), so guard statically.
func TestQueriesAvoidSQLiteOnlyCollation(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(string(b)), "COLLATE NOCASE") {
			t.Errorf("%s uses COLLATE NOCASE (SQLite-only); order by LOWER(col), col instead", f)
		}
	}
}
