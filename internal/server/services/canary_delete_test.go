package services

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

func failingDelete() rtFunc {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})
}

func addRemoteToken(t *testing.T, q *queries.CanaryQueries, id string) {
	t.Helper()
	if err := q.Create(&queries.CanaryToken{ID: id, ClientID: "c1", TokenType: "env_file", CanaryToken: "tok" + id, AuthToken: "auth", DeployPath: ".x/" + id}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTokenKeepsRowOnRemoteFailureAndLocalOnlyRemoves(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", failingDelete())
	addRemoteToken(t, q, "r1")
	if err := svc.DeleteToken("r1", false); !errors.Is(err, ErrCanaryRemoteDeleteFailed) {
		t.Fatalf("err=%v", err)
	}
	if _, err := q.GetByID("r1"); err != nil {
		t.Fatalf("row must be kept: %v", err)
	}
	if err := svc.DeleteToken("r1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := q.GetByID("r1"); err == nil {
		t.Fatal("local-only delete must remove the row")
	}
	if err := svc.DeleteToken("missing", false); !errors.Is(err, ErrCanaryTokenNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestDeleteClientTokensCountsFailuresAndPropagatesListError(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", failingDelete())
	addRemoteToken(t, q, "r1")
	deleted, failed, err := svc.DeleteClientTokens("c1", false)
	if err != nil || deleted != 2 || failed != 1 {
		t.Fatalf("deleted=%d failed=%d err=%v (2 local-only rows go, the remote one stays)", deleted, failed, err)
	}
	if count(t, q) != 1 {
		t.Fatalf("tokens=%d, want the 1 remote-backed row kept", count(t, q))
	}
	if _, _, err := svc.DeleteClientTokens("c1", true); err != nil || count(t, q) != 0 {
		t.Fatalf("local-only: err=%v left=%d", err, count(t, q))
	}
	_ = lastRegenDB.Close()
	if _, _, err := svc.DeleteClientTokens("c1", false); err == nil {
		t.Fatal("a list failure must be returned, not swallowed")
	}
}

func TestDeleteTakesClientLock(t *testing.T) {
	svc, _ := newRegenFixture(t, "a@b.co", failingDelete())
	unlock := svc.clientLock("c1")
	done := make(chan struct{})
	go func() { _ = svc.DeleteToken("old1", true); close(done) }()
	select {
	case <-done:
		t.Fatal("DeleteToken ran while the client lock was held")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DeleteToken never ran after unlock")
	}
	done2 := make(chan struct{})
	unlock = svc.clientLock("c1")
	go func() { _, _, _ = svc.DeleteClientTokens("c1", true); close(done2) }()
	select {
	case <-done2:
		t.Fatal("DeleteClientTokens ran while the client lock was held")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	<-done2
}

func TestRegenerateDBDeleteFailureCountsAndContinues(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(*http.Request) (*http.Response, error) {
		return okJSON(`{"token":"","hostname":"h","auth_token":""}`), nil
	}))
	if _, err := lastRegenDB.Exec(`CREATE TRIGGER no_del BEFORE DELETE ON canary_tokens WHEN OLD.id = 'old1' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Skip("trigger unsupported:", err)
	}
	res, err := svc.RegenerateForClient("c1", "alpha", "s1")
	if err != nil {
		t.Fatalf("a DB delete failure must not fail the run: %v", err)
	}
	if res.DeleteFailed != 1 || res.Created != 1 {
		t.Fatalf("res=%+v", res)
	}
	if _, err := q.GetByID("old1"); err != nil {
		t.Fatal("row should remain")
	}
}

// A token removed while a delete waits for the client lock is reported as not found, not double-processed.
func TestDeleteTokenRefetchesAfterLock(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", failingDelete())
	unlock := svc.clientLock("c1")
	res := make(chan error, 1)
	go func() { res <- svc.DeleteToken("old1", false) }()
	time.Sleep(150 * time.Millisecond)
	if err := q.DeleteByID("old1"); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-res; !errors.Is(err, ErrCanaryTokenNotFound) {
		t.Fatalf("err=%v, want not found after re-fetch", err)
	}
}
