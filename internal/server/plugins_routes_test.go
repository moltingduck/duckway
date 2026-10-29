package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/clientplugin"
	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
	"github.com/hackerduck/duckway/web"
)

func TestServerManagedPluginAuthImportCacheAndDisable(t *testing.T) {
	var upstreamHits atomic.Int64
	payload := []byte("cached plugin binary bytes\n")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	manifest := clientplugin.Manifest{APIVersion: 1, Name: "duckllo", Version: "sha256-" + digest, Artifacts: []clientplugin.Artifact{{OS: "linux", Arch: "amd64", URL: "/artifact", SHA256: digest, Size: int64(len(payload))}}}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.Header.Get("X-Duckway-Token") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials leaked upstream")
		}
		if r.URL.Path == "/manifest" {
			_ = json.NewEncoder(w).Encode(manifest)
		} else {
			_, _ = w.Write(payload)
		}
	}))
	t.Cleanup(source.Close)
	t.Setenv("DUCKWAY_PLUGIN_ALLOWED_ORIGINS", source.URL)
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
	gateway := &Server{config: config, db: db, mux: http.NewServeMux(), stopCh: make(chan struct{})}
	gateway.SetupGatewayRoutes(gateway.initShared())
	cookie := shared.AdminAuth.CreateSession("plugin-admin", httptest.NewRequest("POST", "http://admin.example/api/plugins/import", nil))
	token := "plugin-route-client-token"
	if err := shared.ClientQ.Create(&models.Client{ID: "plugin-client", ShortID: "plugin-client", Name: "plugin-client", TokenHash: services.HashToken(token)}); err != nil {
		t.Fatal(err)
	}
	request := func(server *Server, method, path, body, auth, origin, contentType string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://admin.example"+path, strings.NewReader(body))
		if auth == "admin" {
			req.AddCookie(cookie)
		} else if auth != "" {
			req.Header.Set("X-Duckway-Token", auth)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	binaryPath := "/client/plugins/duckllo/releases/" + manifest.Version + "/linux/amd64"
	for _, route := range []struct {
		s            *Server
		method, path string
	}{{admin, "GET", "/api/plugins"}, {admin, "POST", "/api/plugins/import"}, {admin, "POST", "/api/plugins/duckllo/check"}, {admin, "POST", "/api/plugins/duckllo/publish"}, {admin, "GET", "/api/plugins/duckllo/clients"}, {gateway, "POST", "/client/plugins/report"}, {admin, "POST", "/api/plugins/duckllo/disable"}, {admin, "POST", "/api/plugins/duckllo/enable"}, {gateway, "GET", "/client/plugins"}, {gateway, "GET", "/client/plugins/duckllo/manifest"}, {gateway, "GET", binaryPath}} {
		rec := request(route.s, route.method, route.path, "{}", "", "http://admin.example", "application/json")
		if rec.Code != 401 {
			t.Fatalf("anonymous %s: %d %s", route.path, rec.Code, rec.Body.String())
		}
	}
	rec := request(gateway, "GET", "/client/plugins", "", token, "", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty catalog=%d %s", rec.Code, rec.Body.String())
	}
	input, _ := json.Marshal(map[string]string{"name": "duckllo", "manifest_url": source.URL + "/manifest"})
	for _, tc := range []struct {
		body, origin, contentType string
		code                      int
	}{{string(input), "", "application/json", 403}, {string(input), "http://evil.example", "application/json", 403}, {string(input), "http://admin.example", "text/plain", 415}, {string(input) + " {}", "http://admin.example", "application/json", 400}, {`{"name":"duckllo","manifest_url":"x","unknown":true}`, "http://admin.example", "application/json", 400}, {strings.Repeat(" ", 8193), "http://admin.example", "application/json", 400}} {
		rec := request(admin, "POST", "/api/plugins/import", tc.body, "admin", tc.origin, tc.contentType)
		if rec.Code != tc.code {
			t.Fatalf("mutation status=%d want=%d: %s", rec.Code, tc.code, rec.Body.String())
		}
	}
	if upstreamHits.Load() != 0 {
		t.Fatal("rejected mutation contacted source")
	}
	rec = request(admin, "POST", "/api/plugins/import", string(input), "admin", "http://admin.example", "application/json")
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	// Registration checks metadata only; publication requires explicit version approval.
	if upstreamHits.Load() != 1 {
		t.Fatalf("registration downloaded binaries: %d", upstreamHits.Load())
	}
	rec = request(gateway, "GET", "/client/plugins", "", token, "", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("pending release exposed: %d %s", rec.Code, rec.Body.String())
	}
	manifestBytes, _ := json.Marshal(manifest)
	approvalHash := sha256.Sum256(manifestBytes)
	publishBody, _ := json.Marshal(map[string]string{"version": manifest.Version, "manifest_digest": hex.EncodeToString(approvalHash[:])})
	rec = request(admin, "POST", "/api/plugins/duckllo/publish", string(publishBody), "admin", "http://admin.example", "application/json")
	if rec.Code != 200 {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	rec = request(admin, "GET", "/api/plugins", "", "admin", "", "")
	if rec.Code != 200 {
		t.Fatalf("admin list: %d %s", rec.Code, rec.Body.String())
	}
	var summaries []struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Artifacts []struct {
			OS     string `json:"os"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &summaries); err != nil || len(summaries) != 1 || summaries[0].Version != manifest.Version || len(summaries[0].Artifacts) != 1 || summaries[0].Artifacts[0].SHA256 != digest || summaries[0].Artifacts[0].Size != int64(len(payload)) {
		t.Fatalf("admin metadata: %s err=%v", rec.Body.String(), err)
	}
	source.Close()
	hits := upstreamHits.Load()
	for _, route := range []string{"/client/plugins", "/client/plugins/duckllo/manifest"} {
		rec = request(gateway, "GET", route, "", token, "", "")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), source.URL) || !strings.Contains(rec.Body.String(), binaryPath) {
			t.Fatalf("cached manifest %s: %d %s", route, rec.Code, rec.Body.String())
		}
	}
	rec = request(gateway, "GET", binaryPath, "", token, "", "")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("cached download=%d %q", rec.Code, rec.Body.Bytes())
	}
	if upstreamHits.Load() != hits {
		t.Fatal("client fetch contacted source")
	}
	reportBody, _ := json.Marshal(map[string]any{"api_version": 1, "plugins": []map[string]string{{"name": "duckllo", "version": manifest.Version}}})
	rec = request(gateway, "POST", "/client/plugins/report", string(reportBody), token, "", "application/json")
	if rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
	rec = request(gateway, "POST", "/client/plugins/report", `{"api_version":1,"plugins":[],"client_id":"other-client"}`, token, "", "application/json")
	if rec.Code != 400 {
		t.Fatalf("spoofedidentity accepted: %d", rec.Code)
	}
	rec = request(admin, "GET", "/api/plugins/duckllo/clients", "", "admin", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"up_to_date"`) || !strings.Contains(rec.Body.String(), `"client_id":"plugin-client"`) {
		t.Fatalf("fleet: %d %s", rec.Code, rec.Body.String())
	}
	rec = request(admin, "POST", "/api/plugins/duckllo/disable", "{}", "admin", "http://admin.example", "application/json")
	if rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	for _, route := range []string{"/client/plugins/duckllo/manifest", binaryPath} {
		rec = request(gateway, "GET", route, "", token, "", "")
		if rec.Code != 404 {
			t.Fatalf("disabled %s: %d", route, rec.Code)
		}
	}
	rec = request(gateway, "GET", "/client/plugins", "", token, "", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("disabled catalog=%d %s", rec.Code, rec.Body.String())
	}
	rec = request(admin, "POST", "/api/plugins/duckllo/enable", "{}", "admin", "http://admin.example", "application/json")
	if rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	rec = request(gateway, "GET", binaryPath, "", token, "", "")
	if rec.Code != 200 {
		t.Fatalf("reenabled download=%d", rec.Code)
	}
	// No unauthenticated download alias is registered.
	rec = request(gateway, "GET", "/download/plugins/duckllo", "", "", "", "")
	if rec.Code == 200 {
		t.Fatal("public download exposed plugin")
	}
}
