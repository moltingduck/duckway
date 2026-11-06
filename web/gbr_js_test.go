package web

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestBranchRulesJS runs the shared match vectors (also consumed by the Go
// matcher) the pure model checks of static/github-branch-rules.js and the builder/preset
// serialisation of phantom-acl.js and phantom-common.js under
// node. The repository has no JS toolchain, so the test skips when node is not
// installed; set DUCKWAY_REQUIRE_NODE=1 to make a missing node a failure.
func TestBranchRulesJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("DUCKWAY_REQUIRE_NODE") != "" {
			t.Fatal("node is required but not installed")
		}
		t.Skip("node not installed; skipping JS branch-rule tests")
	}
	vectors, err := filepath.Abs("../internal/server/services/testdata/github_branch_match_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	src, err := filepath.Abs("static/github-branch-rules.js")
	if err != nil {
		t.Fatal(err)
	}
	acl, err := filepath.Abs("static/phantom-acl.js")
	if err != nil {
		t.Fatal(err)
	}
	common, err := filepath.Abs("static/phantom-common.js")
	if err != nil {
		t.Fatal(err)
	}
	client, err := filepath.Abs("templates/client_detail.html")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "testdata/gbr_test.js", src, vectors, acl, common, client).CombinedOutput()
	if err != nil {
		t.Fatalf("node gbr_test.js failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// TestBranchRulesFixturesNotStale regenerates the UI-serialized fixtures with
// the page's own serializers and compares them byte for byte with the files the
// Go contract test reads.
func TestBranchRulesFixturesNotStale(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("DUCKWAY_REQUIRE_NODE") != "" {
			t.Fatal("node is required but not installed")
		}
		t.Skip("node not installed; skipping fixture staleness check")
	}
	static, _ := filepath.Abs("static")
	client, _ := filepath.Abs("templates/client_detail.html")
	committed, _ := filepath.Abs("../internal/server/services/testdata/gbr_ui")
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if b, err := exec.CommandContext(ctx, node, "testdata/gen_gbr_fixtures.js", static, out, client).CombinedOutput(); err != nil {
		t.Fatalf("generator failed: %v\n%s", err, b)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) == 0 {
		t.Fatalf("generator wrote nothing: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
		got, _ := os.ReadFile(filepath.Join(out, e.Name()))
		want, err := os.ReadFile(filepath.Join(committed, e.Name()))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("fixture %s is stale: run node web/testdata/gen_gbr_fixtures.js web/static internal/server/services/testdata/gbr_ui web/templates/client_detail.html", e.Name())
		}
	}
	files, _ := os.ReadDir(committed)
	for _, f := range files {
		if !seen[f.Name()] {
			t.Errorf("committed fixture %s is no longer generated", f.Name())
		}
	}
}
