package client

import (
	"fmt"
	"sync"
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

func TestCCProcessedStoreMarkIfNewIsAtomic(t *testing.T) {
	store := NewCCProcessedStore(t.TempDir())
	const workers = 20
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.MarkIfNew("msg-1", "dwch_t")
			if err != nil {
				t.Errorf("MarkIfNew: %v", err)
				return
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)

	claimed := 0
	for ok := range results {
		if ok {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
}
