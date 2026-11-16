package web

import (
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// TestRefusalFilterKindsMatchBackend pins the "Refused by" chips in
// static/refusal-log.js to the kinds the logs API accepts: every chip must be a
// valid deny_kind, and the only accepted kind without a chip is "approval"
// (a hold, not a refusal). "credential" is simulator-only and never a log kind.
func TestRefusalFilterKindsMatchBackend(t *testing.T) {
	src, err := os.ReadFile("static/refusal-log.js")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)var KINDS = \[(.*?)\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("KINDS list not found in refusal-log.js")
	}
	chips := map[string]bool{}
	for _, m := range regexp.MustCompile(`\['([a-z_]+)',`).FindAllSubmatch(block[1], -1) {
		chips[string(m[1])] = true
	}
	backend := map[string]bool{}
	for _, k := range queries.DenyKinds {
		backend[k] = true
	}
	var extra, missing []string
	for k := range chips {
		if !backend[k] {
			extra = append(extra, k)
		}
	}
	for k := range backend {
		if !chips[k] && k != "approval" {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("refusal-log.js chips the backend rejects as deny_kind: %v", extra)
	}
	if len(missing) > 0 {
		t.Errorf("backend deny kinds without a chip in refusal-log.js: %v", missing)
	}
	if backend["credential"] {
		t.Error(`"credential" must stay simulator-only, not a logged deny kind`)
	}
}
