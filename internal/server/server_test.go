package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestHealthzChecksDatabase(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, mux: http.NewServeMux()}
	s.setupHealthRoute()
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok\n" {
		t.Fatalf("healthy response = %d %q", recorder.Code, recorder.Body.String())
	}
	db.Close()
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed database response = %d, want 503", recorder.Code)
	}
}

func TestSeedDefaultServicesGitHubUsesFineGrainedPhantomMode(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s := &Server{db: db, config: &Config{EncryptionKey: []byte("0123456789abcdef0123456789abcdef")}}
	if err := s.seedDefaultServices(); err != nil {
		t.Fatal(err)
	}

	gh, err := queries.NewServiceQueries(db).GetByName("github")
	if err != nil {
		t.Fatal(err)
	}
	if gh.KeyPrefix != "github_pat_" {
		t.Fatalf("KeyPrefix = %q, want github_pat_", gh.KeyPrefix)
	}
	if gh.KeyLength != 93 {
		t.Fatalf("KeyLength = %d, want 93", gh.KeyLength)
	}
	if gh.DeliveryMode != "proxy" {
		t.Fatalf("DeliveryMode = %q, want proxy", gh.DeliveryMode)
	}
	if gh.HostPattern != "api.github.com,github.com" {
		t.Fatalf("HostPattern = %q", gh.HostPattern)
	}
	if gh.AuthHeader != "Authorization" || gh.AuthPrefix != "Bearer " {
		t.Fatalf("auth = %q %q", gh.AuthHeader, gh.AuthPrefix)
	}
}
