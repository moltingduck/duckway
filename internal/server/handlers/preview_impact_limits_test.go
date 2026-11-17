package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A second preview while one is running is refused with 429 instead of piling
// up replays of up to 5,000 groups each.
func TestPreviewRefusesConcurrentRun(t *testing.T) {
	p := &ImpactPreview{timeout: time.Second, running: make(chan struct{}, 1)}
	p.running <- struct{}{} // a preview is in flight
	w := httptest.NewRecorder()
	p.respond(w, httptest.NewRequest(http.MethodPost, "/preview-impact", nil), nil, previewDraft{})
	if w.Code != http.StatusTooManyRequests || w.Body.String() == "" {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	<-p.running
	w = httptest.NewRecorder()
	p.respond(w, httptest.NewRequest(http.MethodPost, "/preview-impact", nil), nil, previewDraft{}) // no tokens: trivial run
	if w.Code != http.StatusOK {
		t.Fatalf("free slot: %d %s", w.Code, w.Body.String())
	}
	if len(p.running) != 0 {
		t.Fatal("slot not released")
	}
}
