package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/web"
)

func TestSupplyChainPageRenders(t *testing.T) {
	h := NewAdminHandler(web.Content, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	w := httptest.NewRecorder()
	h.SupplyChainPage(w, httptest.NewRequest("GET", "/admin/supply-chain", nil))
	if w.Code != 200 {
		t.Fatalf("render status %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Package Manager Hardening", "Minimum package age", "/api/supply-chain"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

func newSupplyChainMux(t *testing.T) *http.ServeMux {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := NewSupplyChainHandler(queries.NewSettingsQueries(db))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/supply-chain", h.List)
	mux.HandleFunc("POST /api/supply-chain/min-age", h.SetMinAge)
	mux.HandleFunc("POST /api/supply-chain/{id}", h.Toggle)
	mux.HandleFunc("GET /client/supply-chain-rc", h.ClientRC)
	return mux
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestSupplyChainHandler_DefaultsAndToggle(t *testing.T) {
	mux := newSupplyChainMux(t)

	// List: defaults all-on, cooldown 1 day.
	code, out := doJSON(t, mux, "GET", "/api/supply-chain", "")
	if code != 200 {
		t.Fatalf("list status %d", code)
	}
	if string(out["min_age_days"]) != "1" {
		t.Errorf("min_age_days = %s, want 1", out["min_age_days"])
	}

	// ClientRC default: npm before= + pnpm minimum-release-age in .npmrc.
	code, rc := doJSON(t, mux, "GET", "/client/supply-chain-rc", "")
	if code != 200 {
		t.Fatalf("rc status %d", code)
	}
	var npmrc []string
	_ = json.Unmarshal(rc[".npmrc"], &npmrc)
	joined := strings.Join(npmrc, "\n")
	if !strings.Contains(joined, "ignore-scripts=true") ||
		!strings.Contains(joined, "before=") ||
		!strings.Contains(joined, "minimum-release-age=1440") {
		t.Fatalf(".npmrc default content wrong: %v", npmrc)
	}

	// Toggle npm off → before= disappears, pnpm settings remain.
	code, _ = doJSON(t, mux, "POST", "/api/supply-chain/npm", `{"enabled":false}`)
	if code != 200 {
		t.Fatalf("toggle status %d", code)
	}
	_, rc = doJSON(t, mux, "GET", "/client/supply-chain-rc", "")
	npmrc = nil
	_ = json.Unmarshal(rc[".npmrc"], &npmrc)
	joined = strings.Join(npmrc, "\n")
	if strings.Contains(joined, "before=") {
		t.Errorf("before= should be gone after npm disabled: %v", npmrc)
	}
	if !strings.Contains(joined, "minimum-release-age=1440") {
		t.Errorf("pnpm setting should remain: %v", npmrc)
	}

	// Unsupported / unknown id rejected.
	code, _ = doJSON(t, mux, "POST", "/api/supply-chain/pip", `{"enabled":false}`)
	if code != http.StatusNotFound {
		t.Errorf("toggling unsupported pip should 404, got %d", code)
	}
}

func TestSupplyChainHandler_SetMinAge(t *testing.T) {
	mux := newSupplyChainMux(t)

	code, _ := doJSON(t, mux, "POST", "/api/supply-chain/min-age", `{"days":3}`)
	if code != 200 {
		t.Fatalf("set min-age status %d", code)
	}
	_, rc := doJSON(t, mux, "GET", "/client/supply-chain-rc", "")
	var npmrc []string
	_ = json.Unmarshal(rc[".npmrc"], &npmrc)
	if !strings.Contains(strings.Join(npmrc, "\n"), "minimum-release-age=4320") {
		t.Errorf("3-day cooldown → 4320 min expected, got %v", npmrc)
	}

	// Rejects non-positive.
	code, _ = doJSON(t, mux, "POST", "/api/supply-chain/min-age", `{"days":0}`)
	if code != http.StatusBadRequest {
		t.Errorf("days=0 should 400, got %d", code)
	}
}
