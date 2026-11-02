package serverplugin

import (
	"context"
	"encoding/json"
	"github.com/hackerduck/duckway/internal/clientplugin"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckStagesAvailableAndPublishRequiresExactCandidate(t *testing.T) {
	s, m, data, hits, source := newFixture(t)
	ctx := context.Background()
	available, err := s.Import(ctx, "duckllo", source)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("check downloaded artifact: %d", hits.Load())
	}
	if catalog, err := s.Catalog(ctx); err != nil || len(catalog) != 0 {
		t.Fatalf("unpublished catalog=%v %v", catalog, err)
	}
	if err := s.SetEnabled(ctx, "duckllo", true); err == nil {
		t.Fatal("enabled unpublished plugin")
	}
	if _, err := s.Publish(ctx, "duckllo", "sha256-"+strings.Repeat("0", 64), currentApproval(s)); err == nil {
		t.Fatal("published wrong expected version")
	}
	oldVersion := available.Version
	if _, err := s.Publish(ctx, "duckllo", oldVersion, currentApproval(s)); err != nil {
		t.Fatal(err)
	}
	*data = []byte("next valid release")
	*m = fixtureManifest(*data)
	if _, err := s.Check(ctx, "duckllo"); err != nil {
		t.Fatal(err)
	}
	current, err := s.Manifest(ctx, "duckllo")
	if err != nil || current.Version != oldVersion {
		t.Fatalf("check changed publication=%v %v", current, err)
	}
	list, err := s.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	summary := list[0].AdminSummary()
	if summary["available_version"] != m.Version || summary["last_checked_at"] == "" || summary["last_successful_check_at"] == "" || summary["next_check_at"] == "" {
		t.Fatalf("tracking metadata=%v", summary)
	}
	checked := m.Version
	*data = []byte("source moved again")
	*m = fixtureManifest(*data)
	if _, err := s.Publish(ctx, "duckllo", checked, currentApproval(s)); err == nil {
		t.Fatal("source changed after check but publish succeeded")
	}
	current, err = s.Manifest(ctx, "duckllo")
	if err != nil || current.Version != oldVersion {
		t.Fatal("failed publish replaced current release")
	}
	list, _ = s.List(ctx, true)
	if list[0].CheckError == "" {
		t.Fatal("source race not recorded")
	}
	if _, err := s.Check(ctx, "duckllo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "duckllo", m.Version, currentApproval(s)); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(ctx, true)
	if list[0].AdminSummary()["available_version"] != "" {
		t.Fatal("published version still shown available")
	}
}
func TestPublishStopsWhenSourceChangesDuringArtifactFetch(t *testing.T) {
	data := []byte("first")
	m := fixtureManifest(data)
	original := m.Version
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			_ = json.NewEncoder(w).Encode(m)
			return
		}
		_, _ = w.Write(data)
		m = fixtureManifest([]byte("changed"))
	}))
	t.Cleanup(server.Close)
	s, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(context.Background(), "duckllo", server.URL+"/manifest"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(context.Background(), "duckllo", original, currentApproval(s)); err == nil {
		t.Fatal("publish ignored source change during artifact fetch")
	}
	if list, err := s.Catalog(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("failed publish became visible: %v %v", list, err)
	}
}
func TestDueChecksSharedAcrossStoresAndFailuresPreserveLatest(t *testing.T) {
	s, _, _, hits, source := newFixture(t)
	ctx := context.Background()
	if _, err := s.Import(ctx, "duckllo", source); err != nil {
		t.Fatal(err)
	}
	other, err := New(strings.TrimSuffix(s.dir, "/plugins"), strings.TrimSuffix(source, "/manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.CheckDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatal("source checked before due")
	}
	future := time.Now().Add(7 * time.Hour)
	if err := s.CheckDue(ctx, future); err != nil {
		t.Fatal(err)
	}
	if err := other.CheckDue(ctx, future); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("duplicate due check requests=%d", hits.Load())
	}
	// A disallowed network policy models an unavailable upstream without changing the source.
	strict, _ := New(strings.TrimSuffix(s.dir, "/plugins"), "")
	if _, err := strict.Check(ctx, "duckllo"); err == nil {
		t.Fatal("failed source check accepted")
	}
	list, err := strict.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Latest == nil || list[0].LastSuccessfulCheckAt == "" || list[0].CheckError == "" {
		t.Fatalf("check failure lost previous successful state: %+v", list[0])
	}
}
func TestCheckerNoStartupCheckAndCancellation(t *testing.T) {
	s, _, _, hits, source := newFixture(t)
	if _, err := s.Import(context.Background(), "duckllo", source); err != nil {
		t.Fatal(err)
	}
	s.interval = time.Hour
	stop := make(chan struct{})
	checker := StartChecker(s, stop)
	t.Cleanup(checker.Stop)
	if hits.Load() != 1 {
		t.Fatal("scheduler checked immediately on startup")
	}
	close(stop)
	checker.Stop()
	var started atomic.Bool
	entered := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if started.CompareAndSwap(false, true) {
			close(entered)
		}
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(upstream.Close)
	slow, _ := New(t.TempDir(), upstream.URL)
	slow.interval = 5 * time.Millisecond
	root, done, err := slow.locked(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	c := catalog{Plugins: map[string]Entry{"duckllo": {Name: "duckllo", Source: upstream.URL + "/manifest", Releases: make(map[string]clientplugin.Manifest)}}}
	if err := saveCatalog(root, c); err != nil {
		done()
		t.Fatal(err)
	}
	done()
	stop2 := make(chan struct{})
	running := StartChecker(slow, stop2)
	t.Cleanup(running.Stop)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not check due source")
	}
	close(stop2)
	running.Stop()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel source HTTP")
	}
}
func TestCheckIntervalConfiguration(t *testing.T) {
	for _, raw := range []string{"0", "-1h", "10s", "8d", "168h1s", "invalid"} {
		if _, err := checkInterval(raw); err == nil {
			t.Errorf("interval accepted %q", raw)
		}
	}
	for _, raw := range []string{"", "1m", "6h", "168h"} {
		if _, err := checkInterval(raw); err != nil {
			t.Errorf("interval rejected %q: %v", raw, err)
		}
	}
}
