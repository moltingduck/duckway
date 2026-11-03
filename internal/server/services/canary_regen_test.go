package services

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newRegenFixture(t *testing.T, email string, rt http.RoundTripper) (*CanaryService, *queries.CanaryQueries) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO clients (id, short_id, name, token_hash, is_active) VALUES ('c1','s1','alpha','h',1)`); err != nil {
		t.Fatal(err)
	}
	q := queries.NewCanaryQueries(db)
	if err := q.SaveSettings(&queries.CanarySettings{Email: email, EnabledTypes: `["env_file"]`, ExcludeClients: "[]"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old1", "old2"} {
		if err := q.Create(&queries.CanaryToken{ID: id, ClientID: "c1", TokenType: "dns", DeployPath: ".x/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewCanaryService(q)
	svc.client = &http.Client{Transport: rt}
	return svc, q
}

func count(t *testing.T, q *queries.CanaryQueries) int {
	t.Helper()
	l, err := q.ListByClient("c1")
	if err != nil {
		t.Fatal(err)
	}
	return len(l)
}

func TestRegenerateWithoutEmailKeepsTokens(t *testing.T) {
	called := false
	svc, q := newRegenFixture(t, "", rtFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("x") }))
	if err := svc.RegenerateForClient("c1", "alpha", "s1"); !errors.Is(err, ErrCanaryNoAlertEmail) {
		t.Fatalf("err=%v", err)
	}
	if n := count(t, q); n != 2 || called {
		t.Fatalf("tokens=%d called=%v, want 2/false", n, called)
	}
}

func TestRegenerateGenerationFailureKeepsOldTokens(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unreachable") }))
	if err := svc.RegenerateForClient("c1", "alpha", "s1"); !errors.Is(err, ErrCanaryNothingGenerated) {
		t.Fatalf("err=%v", err)
	}
	if n := count(t, q); n != 2 {
		t.Fatalf("tokens=%d, want old 2 kept", n)
	}
}

func TestRegenerateSuccessReplacesOldTokens(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"token":"","hostname":"h.example","auth_token":""}`)), Header: http.Header{}}, nil
	}))
	if err := svc.RegenerateForClient("c1", "alpha", "s1"); err != nil {
		t.Fatal(err)
	}
	l, _ := q.ListByClient("c1")
	if len(l) != 1 || l[0].ID == "old1" || l[0].ID == "old2" {
		t.Fatalf("tokens=%+v, want only the fresh one", l)
	}
}
