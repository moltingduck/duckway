package handlers

import (
	"sync"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

// Many handlers (separate processes) racing on the first use of one database
// all end up with the same persisted key.
func TestApprovalKeyConcurrentFirstUseAgrees(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	settings := queries.NewSettingsQueries(db)
	const n = 24
	keys := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := &ProxyHandler{settings: settings} // one handler per "process"
			<-start
			k, e := h.approvalKey()
			keys[i], errs[i] = string(k), e
		}(i)
	}
	close(start)
	wg.Wait()
	stored, _ := settings.GetE(queries.SettingApprovalScopeKey)
	for i := 0; i < n; i++ {
		if errs[i] != nil || keys[i] == "" || keys[i] != stored {
			t.Fatalf("goroutine %d: key=%q stored=%q err=%v", i, keys[i], stored, errs[i])
		}
	}
	// Same handler: cached, identical.
	h := &ProxyHandler{settings: settings}
	a, _ := h.approvalKey()
	b, _ := h.approvalKey()
	if string(a) != string(b) || string(a) != stored {
		t.Fatal("a handler must keep returning the stored key")
	}
}

// A read error is returned, and no throw-away key is cached.
func TestApprovalKeyReadErrorNotCached(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := queries.NewSettingsQueries(db)
	h := &ProxyHandler{settings: settings}
	_ = db.Close()
	if k, err := h.approvalKey(); err == nil || k != nil {
		t.Fatalf("closed store must error: %v %v", k, err)
	}
	if h.scopeKey != nil {
		t.Fatal("a failed read must not cache a key")
	}
	// No settings store at all (tests): a random per-handler key is fine.
	if k, err := (&ProxyHandler{}).approvalKey(); err != nil || len(k) == 0 {
		t.Fatalf("storeless handler: %v %v", k, err)
	}
}
