package web

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestRulePreviewJS runs the pure helper checks of rule-preview.js (bucket
// tabs, summaries, confirm rule, row models, safe links) under node. Like the
// other node tests it skips without node unless DUCKWAY_REQUIRE_NODE is set.
func TestRulePreviewJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("DUCKWAY_REQUIRE_NODE") != "" {
			t.Fatal("node is required but not installed")
		}
		t.Skip("node not installed; skipping rule preview JS tests")
	}
	static, err := filepath.Abs("static")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "testdata/rule_preview_test.js", static).CombinedOutput()
	if err != nil {
		t.Fatalf("node rule_preview_test.js failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
