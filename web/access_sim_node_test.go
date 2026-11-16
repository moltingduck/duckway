package web

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestAccessSimJS runs the pure-module checks (hex parse/format round trip,
// UTF-8 and control-byte detection, request/response adapters and the
// refusal-reason adapter) under node. Like TestBranchRulesJS it skips without
// node unless DUCKWAY_REQUIRE_NODE is set.
func TestAccessSimJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("DUCKWAY_REQUIRE_NODE") != "" {
			t.Fatal("node is required but not installed")
		}
		t.Skip("node not installed; skipping access simulator JS tests")
	}
	static, err := filepath.Abs("static")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "testdata/access_sim_test.js", static).CombinedOutput()
	if err != nil {
		t.Fatalf("node access_sim_test.js failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
