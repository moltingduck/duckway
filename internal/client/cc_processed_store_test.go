package client

import (
	"fmt"
	"testing"
)

func TestCCProcessedStorePersistsSeenMessages(t *testing.T) {
	dir := t.TempDir()
	store := NewCCProcessedStore(dir)
	if store.Seen("m1") {
		t.Fatal("m1 should not be seen before Mark")
	}
	if err := store.Mark("m1", "dwch_a"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	reloaded := NewCCProcessedStore(dir)
	if !reloaded.Seen("m1") {
		t.Fatal("m1 should be seen after reload")
	}
}

func TestCCProcessedStorePrunesOldMessages(t *testing.T) {
	dir := t.TempDir()
	store := NewCCProcessedStore(dir)
	for i := 0; i < ccProcessedMessageLimit+5; i++ {
		if err := store.Mark(fmt.Sprintf("m%04d", i), "dwch_a"); err != nil {
			t.Fatalf("Mark %d: %v", i, err)
		}
	}
	reloaded := NewCCProcessedStore(dir)
	if reloaded.Seen("m0000") {
		t.Fatal("oldest message should have been pruned")
	}
	if !reloaded.Seen(fmt.Sprintf("m%04d", ccProcessedMessageLimit+4)) {
		t.Fatal("newest message should remain")
	}
}
