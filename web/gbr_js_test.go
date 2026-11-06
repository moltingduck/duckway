package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
	cmd := exec.Command(node, "testdata/gbr_test.js", src, vectors, acl, common)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node gbr_test.js failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
