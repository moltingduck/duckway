package services

import (
	"database/sql"
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

// lastRegenDB is the DB of the most recent fixture (tests here are not parallel).
var lastRegenDB *sql.DB

func newRegenFixture(t *testing.T, email string, rt http.RoundTripper) (*CanaryService, *queries.CanaryQueries) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	lastRegenDB = db
	if _, err := db.Exec(`INSERT INTO clients (id, short_id, name, token_hash, is_active) VALUES ('c1','s1','alpha','h',1)`); err != nil {
		t.Fatal(err)
	}
	q := queries.NewCanaryQueries(db)
	if err := q.SaveSettings(&queries.CanarySettings{Email: email, EnabledTypes: `["env_file"]`, ExcludeClients: "[]"}); err != nil {
		t.Fatal(err)
	}
	for id, typ := range map[string]string{"old1": "env_file", "old2": "dns"} {
		if err := q.Create(&queries.CanaryToken{ID: id, ClientID: "c1", TokenType: typ, DeployPath: ".x/" + id}); err != nil {
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
	if _, err := svc.RegenerateForClient("c1", "alpha", "s1"); !errors.Is(err, ErrCanaryNoAlertEmail) {
		t.Fatalf("err=%v", err)
	}
	if n := count(t, q); n != 2 || called {
		t.Fatalf("tokens=%d called=%v, want 2/false", n, called)
	}
}

func TestRegenerateGenerationFailureKeepsOldTokens(t *testing.T) {
	svc, q := newRegenFixture(t, "a@b.co", rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unreachable") }))
	if _, err := svc.RegenerateForClient("c1", "alpha", "s1"); !errors.Is(err, ErrCanaryNothingGenerated) {
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
	res, err := svc.RegenerateForClient("c1", "alpha", "s1")
	if err != nil {
		t.Fatal(err)
	}
	l, _ := q.ListByClient("c1")
	ids := map[string]bool{}
	for _, tk := range l {
		ids[tk.ID] = true
	}
	// old1 (env_file) was replaced; old2 (a type with no replacement) is kept.
	if len(l) != 2 || ids["old1"] || !ids["old2"] || res.Replaced != 1 || res.Created != 1 {
		t.Fatalf("tokens=%+v res=%+v, want fresh env_file + kept old2", l, res)
	}
}
