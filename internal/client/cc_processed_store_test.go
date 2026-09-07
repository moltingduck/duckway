package client

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

func TestCCProcessedStorePersistsPromptRejectionUntilCompleted(t *testing.T) {
	dir := t.TempDir()
	store := NewCCProcessedStore(dir)
	if err := store.RecordPromptRejection("message-1", "dwch_task", "rejected", "message-1", "rejection-key"); err != nil {
		t.Fatal(err)
	}
	reloaded := NewCCProcessedStore(dir)
	receipt, ok, err := reloaded.PromptRejection("message-1", "dwch_task")
	if err != nil || !ok || receipt.Content != "rejected" || receipt.ReplyTo != "message-1" || receipt.DeliveryKey != "rejection-key" {
		t.Fatalf("rejection=%+v ok=%v", receipt, ok)
	}
	if _, ok, _ := reloaded.PromptRejection("message-1", "other-channel"); ok {
		t.Fatal("rejection leaked across channel handles")
	}
	if err := reloaded.Mark("message-1", "dwch_task"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := reloaded.PromptRejection("message-1", "dwch_task"); ok {
		t.Fatal("completed message retained a pending rejection receipt")
	}
}

func TestCCProcessedStoreNeverPrunesPendingPromptRejection(t *testing.T) {
	dir := t.TempDir()
	store := NewCCProcessedStore(dir)
	if err := store.RecordPromptRejection("pending", "dwch_task", "rejected", "pending", "pending-key"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	for i := 0; i < ccProcessedMessageLimit+10; i++ {
		id := fmt.Sprintf("completed-%04d", i)
		store.data[id] = processedMessageRecord{MessageID: id, Handle: "dwch_task", SeenAt: time.Unix(int64(i), 0)}
	}
	store.pruneLocked()
	err := store.flushLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reloaded := NewCCProcessedStore(dir)
	if _, ok, err := reloaded.PromptRejection("pending", "dwch_task"); err != nil || !ok {
		t.Fatalf("pending receipt pruned: ok=%v err=%v", ok, err)
	}
	if len(reloaded.data) != ccProcessedMessageLimit+1 {
		t.Fatalf("records=%d, want pending plus bounded completed records", len(reloaded.data))
	}
}

func TestCCProcessedStoreCorruptionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cc-processed-messages.json"), []byte("[partial"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewCCProcessedStore(dir)
	if _, _, err := store.PromptRejection("message-1", "dwch_task"); err == nil {
		t.Fatal("corrupt outcome store was treated as empty")
	}
	if !store.Seen("message-1") {
		t.Fatal("corrupt outcome store did not fail closed for deduplication")
	}
	if err := store.RecordPromptRejection("message-1", "dwch_task", "rejected", "message-1", "key"); err == nil {
		t.Fatal("corrupt outcome store was overwritten")
	}
}
