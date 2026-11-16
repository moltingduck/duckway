package web

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestAuditJS runs the pure audit.js checks (line diff, labels, query building)
// under node. It skips without node unless DUCKWAY_REQUIRE_NODE is set.
func TestAuditJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("DUCKWAY_REQUIRE_NODE") != "" {
			t.Fatal("node is required but not installed")
		}
		t.Skip("node not installed; skipping audit JS tests")
	}
	static, err := filepath.Abs("static")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "testdata/audit_test.js", static).CombinedOutput()
	if err != nil {
		t.Fatalf("node audit_test.js failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
