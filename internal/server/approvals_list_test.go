package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/web"
)

func TestApprovalsListRequiresAuthAndCarriesDetailFields(t *testing.T) {
	dataDir := t.TempDir()
	db, err := database.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	config := &Config{DataDir: dataDir, EncryptionKey: bytes.Repeat([]byte{1}, 32), SessionSecret: bytes.Repeat([]byte{2}, 32)}
	admin := &Server{config: config, db: db, mux: http.NewServeMux(), stopCh: make(chan struct{})}
	shared := admin.initShared()
	admin.SetupAdminRoutes(web.Content, shared)

	xssClient := `<img src=x onerror=alert(1)>`
	xssPath := `/v1/"><script>alert(1)</script>`
	info, _ := json.Marshal(map[string]string{"method": "POST", "path": xssPath})
	for _, q := range []string{
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('s1','svc','Svc','https://e.example','e.example')`,
		`INSERT INTO clients (id, name, token_hash, last_seen_at) VALUES ('c1','` + xssClient + `','h1','2026-11-03 10:00:00')`,
		`INSERT INTO client_runtime_status (client_id, version) VALUES ('c1','v0.42.0')`,
		`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k1','s1','prod-key','enc')`,
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('p1','OPENAI_API_KEY','ph-1','s1','k1','c1')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id, status, request_info) VALUES ('a1','p1','pending',?)`, string(info)); err != nil {
		t.Fatal(err)
	}

	anon := httptest.NewRecorder()
	admin.ServeHTTP(anon, httptest.NewRequest("GET", "http://admin.example/api/approvals/list", nil))
	if anon.Code != 401 {
		t.Fatalf("anonymous list = %d, want 401", anon.Code)
	}

	req := httptest.NewRequest("GET", "http://admin.example/api/approvals/list?status=pending", nil)
	req.AddCookie(shared.AdminAuth.CreateSession("approvals-admin", req))
	rec := httptest.NewRecorder()
	testSameOrigin(req)
	admin.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	r := rows[0]
	if r["client_name"] != xssClient {
		t.Errorf("client_name not preserved as data: %v", r["client_name"])
	}
	for k, want := range map[string]any{"credential_name": "prod-key", "client_version": "v0.42.0", "client_last_seen_at": "2026-11-03 10:00:00", "requires_approval": true, "env_name": "OPENAI_API_KEY"} {
		if r[k] != want {
			t.Errorf("%s = %v, want %v", k, r[k], want)
		}
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r["request_info"].(string)), &got); err != nil || got["path"] != xssPath {
		t.Errorf("request_info path not preserved: %v %v", got, err)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("<script>")) || bytes.Contains(rec.Body.Bytes(), []byte("<img")) {
		t.Error("JSON must escape HTML-significant characters")
	}
}
