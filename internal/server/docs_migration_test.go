package server

import (
	"os"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/handlers"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// The migration tables in the user guide and the Docs page must list every
// removed URL registered in handlers/moved.go with its real new location, and
// the one parametrised entry (/admin/key-groups/{id}).
func TestMigrationDocsListEveryMovedURL(t *testing.T) {
	guide := readRepoFile(t, "docs/user-guide.md")
	page := readRepoFile(t, "web/templates/docs.html")
	for _, p := range handlers.MovedPaths() {
		m, _ := handlers.LookupMoved(p)
		for name, body := range map[string]string{"user-guide": guide, "docs page": page} {
			if !strings.Contains(body, "`"+p+"`") && !strings.Contains(body, "<code>"+p+"</code>") {
				t.Errorf("%s misses old URL %s", name, p)
			}
			if !strings.Contains(body, "`"+m.NewPath+"`") && !strings.Contains(body, "<code>"+m.NewPath+"</code>") {
				t.Errorf("%s misses new URL %s for %s", name, m.NewPath, p)
			}
		}
	}
	for name, body := range map[string]string{"user-guide": guide, "docs page": page} {
		for _, want := range []string{"/admin/key-groups/{id}", "/admin/key-pools?pool=lease:{id}"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s misses %s", name, want)
			}
		}
	}
}

// Every upgrade command written in the docs must exist in scripts/prod.sh.
func TestMigrationDocsCommandsExistInProdScript(t *testing.T) {
	script := readRepoFile(t, "scripts/prod.sh")
	for _, want := range []string{"upgrade)", "rollback)", "--check)", "--keep-data)", "--to)", "--yes", "--force"} {
		if !strings.Contains(script, want) {
			t.Fatalf("prod.sh lacks %q; update the docs", want)
		}
	}
	for name, body := range map[string]string{
		"user-guide": readRepoFile(t, "docs/user-guide.md"),
		"docs page":  readRepoFile(t, "web/templates/docs.html"),
	} {
		for _, want := range []string{
			"./scripts/prod.sh upgrade --check",
			"./scripts/prod.sh rollback --to backups/upgrade-",
			"--keep-data",
			"0.51 s",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s misses %q", name, want)
			}
		}
	}
}
