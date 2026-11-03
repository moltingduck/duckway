package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
)

const ccMaskSecret = "SECRET-VALUE-zzz-12345"

func ccMaskFixture(t *testing.T) (*handlers.ControlChannelHandler, *queries.ControlChannelQueries) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-mask', 'telegram-mask', 'Telegram Mask', 'https://t.test', 't.test')`)
	exec(`INSERT INTO clients (id, name, token_hash) VALUES ('client-mask', 'mask client', 'hash-mask')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('key-mask', 'svc-mask', 'bot key', 'enc-'||?)`, ccMaskSecret)
	cfg := `{"chat_id":"42","bot_token":"` + ccMaskSecret + `","nested":{"webhook_secret":"` + ccMaskSecret + `","note":"hi"},"agent_options":{}}`
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		VALUES ('cc-mask', 'Mask CC', 'svc-mask', 'key-mask', 'client-mask', 'claude_code', '', ?, 1)`, cfg)
	ccQ := queries.NewControlChannelQueries(db)
	h := handlers.NewControlChannelHandler(ccQ, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db),
		queries.NewServiceQueries(db), queries.NewClientQueries(db), nil, nil, nil)
	return h, ccQ
}

func TestControlChannelListGetNeverReturnSecrets(t *testing.T) {
	h, _ := ccMaskFixture(t)

	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/cc", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), ccMaskSecret) {
		t.Fatalf("list leaked secret or failed: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "chat_id") {
		t.Fatalf("non-secret config missing: %s", rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cc/cc-mask", nil)
	req.SetPathValue("id", "cc-mask")
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, ccMaskSecret) || strings.Contains(body, "enc-") {
		t.Fatalf("detail leaked secret or failed: %d %s", rec.Code, body)
	}
	if !strings.Contains(body, "********") {
		t.Fatalf("expected mask in detail: %s", body)
	}
}

func TestControlChannelUpdateKeepsMaskedSecrets(t *testing.T) {
	h, ccQ := ccMaskFixture(t)
	body := `{"config":{"chat_id":"43","bot_token":"********","nested":{"webhook_secret":"********","note":"changed"}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/cc/cc-mask", strings.NewReader(body))
	req.SetPathValue("id", "cc-mask")
	rec := httptest.NewRecorder()
	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	cc, err := ccQ.GetByID("cc-mask")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(cc.Config, ccMaskSecret) != 2 || strings.Contains(cc.Config, "********") {
		t.Fatalf("stored secrets not preserved: %s", cc.Config)
	}
	if !strings.Contains(cc.Config, `"chat_id":"43"`) || !strings.Contains(cc.Config, "changed") {
		t.Fatalf("non-secret edits lost: %s", cc.Config)
	}

	// A replacement value is stored as-is.
	req = httptest.NewRequest(http.MethodPut, "/api/cc/cc-mask", strings.NewReader(`{"config":{"bot_token":"NEW-TOKEN-1"}}`))
	req.SetPathValue("id", "cc-mask")
	rec = httptest.NewRecorder()
	h.Update(rec, req)
	cc, _ = ccQ.GetByID("cc-mask")
	if !strings.Contains(cc.Config, "NEW-TOKEN-1") {
		t.Fatalf("new secret not stored: %s", cc.Config)
	}
}
