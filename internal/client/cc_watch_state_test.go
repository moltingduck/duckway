package client

import "testing"

func TestCCInboxCursorPersists(t *testing.T) {
	dir := t.TempDir()
	if got := loadCCInboxCursor(dir); got != 0 {
		t.Fatalf("initial cursor = %d, want 0", got)
	}
	if err := saveCCInboxCursor(dir, 42); err != nil {
		t.Fatalf("saveCCInboxCursor: %v", err)
	}
	if got := loadCCInboxCursor(dir); got != 42 {
		t.Fatalf("loaded cursor = %d, want 42", got)
	}
}
