package services

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/database/queries"
)

func okJSON(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func reqBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// A type that fails to regenerate keeps its existing token and is counted.
func TestRegeneratePartialFailureKeepsFailedTypeTokens(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(reqBody(r), "ssh_key") {
			return nil, errors.New("boom")
		}
		return okJSON(`{"token":"","hostname":"h.example","auth_token":""}`), nil
	}))
	if err := q.SaveSettings(&queries.CanarySettings{Email: "a@b.co", EnabledTypes: `["env_file","ssh_key"]`, ExcludeClients: "[]"}); err != nil {
		t.Fatal(err)
	}
	if err := q.Create(&queries.CanaryToken{ID: "oldssh", ClientID: "c1", TokenType: "ssh_key", DeployPath: ".ssh/x"}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.RegenerateForClient("c1", "alpha", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedTypes != 1 || res.Created != 1 || res.Replaced != 1 {
		t.Fatalf("res=%+v", res)
	}
	l, _ := q.ListByClient("c1")
	got := map[string]bool{}
	for _, tk := range l {
		got[tk.ID] = true
	}
	if !got["oldssh"] || got["old1"] || !got["old2"] {
		t.Fatalf("tokens=%v, want oldssh kept, old1 replaced", got)
	}
}

// If the remote delete fails the DB row is kept (never an untracked live remote token).
func TestRegenerateRemoteDeleteFailureKeepsRow(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/delete") {
			return nil, errors.New("offline")
		}
		return okJSON(`{"token":"n","hostname":"h.example","auth_token":"a"}`), nil
	}))
	if err := q.Create(&queries.CanaryToken{ID: "remote", ClientID: "c1", TokenType: "env_file", CanaryToken: "tok", AuthToken: "auth", DeployPath: ".e"}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.RegenerateForClient("c1", "alpha", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.DeleteFailed != 1 {
		t.Fatalf("res=%+v, want 1 delete failure", res)
	}
	if _, err := q.GetByID("remote"); err != nil {
		t.Fatalf("remote-backed row must be kept: %v", err)
	}
}

func TestRegenerateExcludedOrDisabledClient(t *testing.T) {
	var calls int32
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return okJSON(`{"token":"","hostname":"h","auth_token":""}`), nil
	}))
	for _, ex := range []string{`["alpha"]`, `["c1"]`} {
		_ = q.SaveSettings(&queries.CanarySettings{Email: "a@b.co", EnabledTypes: `["env_file"]`, ExcludeClients: ex})
		if _, err := svc.RegenerateForClient("c1", "alpha", "s1"); !errors.Is(err, ErrCanaryClientExcluded) {
			t.Fatalf("exclude %s: err=%v", ex, err)
		}
	}
	_ = q.SaveSettings(&queries.CanarySettings{Email: "a@b.co", EnabledTypes: `["env_file"]`, ExcludeClients: "not json"})
	if _, err := svc.RegenerateForClient("c1", "alpha", "s1"); !errors.Is(err, ErrCanaryExclusionUnreadable) {
		t.Fatalf("unreadable: err=%v", err)
	}
	_ = q.SaveSettings(&queries.CanarySettings{Email: "a@b.co", EnabledTypes: `["env_file"]`, ExcludeClients: "[]"})
	ok, found, err := q.ClientCanaryEnabled("c1")
	if err != nil || !ok || !found {
		t.Fatalf("enabled=%v found=%v err=%v", ok, found, err)
	}
	// canary_enabled=false
	if _, err := lastRegenDB.Exec(`UPDATE clients SET canary_enabled = 0 WHERE id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegenerateForClient("c1", "alpha", "s1"); !errors.Is(err, ErrCanaryClientExcluded) {
		t.Fatalf("disabled: err=%v", err)
	}
	if calls != 0 || count(t, q) != 2 {
		t.Fatalf("calls=%d tokens=%d, want nothing generated or deleted", calls, count(t, q))
	}
}

func TestCanaryExcludedByNameOrID(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want bool
		err  bool
	}{{`["n"]`, true, false}, {`["id1"]`, true, false}, {`["x"]`, false, false}, {``, false, false}, {`{bad`, false, true}} {
		got, err := CanaryExcluded(c.raw, "n", "id1")
		if got != c.want || (err != nil) != c.err {
			t.Fatalf("%q: got=%v err=%v", c.raw, got, err)
		}
	}
}

// Concurrent regenerate and generate calls serialise per client: no duplicates.
func TestConcurrentGenerateNoDuplicates(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(*http.Request) (*http.Response, error) {
		return okJSON(`{"token":"","hostname":"h","auth_token":""}`), nil
	}))
	if _, err := lastRegenDB.Exec(`DELETE FROM canary_tokens`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = svc.GenerateForClient("c1", "alpha", "s1") }()
	}
	wg.Wait()
	if n := count(t, q); n != 1 {
		t.Fatalf("tokens=%d, want exactly 1 (one enabled type)", n)
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = svc.RegenerateForClient("c1", "alpha", "s1") }()
	}
	wg.Wait()
	if n := count(t, q); n != 1 {
		t.Fatalf("after concurrent regenerate tokens=%d, want 1", n)
	}
}
