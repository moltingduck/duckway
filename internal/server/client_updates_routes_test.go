package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hackerduck/duckway/internal/controlplane"
	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
	"github.com/hackerduck/duckway/web"
)

func TestClientUpdateRoutesEnforceClientAndAdminAuth(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	downloadDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(downloadDir, "duckway-client-linux-amd64"), bytes.Repeat([]byte("x"), 2<<20), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUCKWAY_DOWNLOAD_DIR", downloadDir)
	t.Setenv("DUCKWAY_CLIENT_RECOMMENDED_VERSION", "v2")
	config := &Config{DataDir: dir, EncryptionKey: bytes.Repeat([]byte{1}, 32), SessionSecret: bytes.Repeat([]byte{2}, 32)}
	s := &Server{config: config, db: db, mux: http.NewServeMux(), stopCh: make(chan struct{})}
	shared := s.initShared()
	s.SetupAdminRoutes(web.Content, shared)
	s.SetupGatewayRoutes(shared)
	clientToken := "client-route-token"
	if err := queries.NewClientQueries(db).Create(&models.Client{ID: "route-client", ShortID: "route1", Name: "route client", TokenHash: services.HashToken(clientToken), CanaryEnabled: true, UpdatePolicy: "managed"}); err != nil {
		t.Fatal(err)
	}
	body := `{"protocol_version":1,"version":"v1","os":"linux","arch":"amd64","boot_id":"boot","install_path":"/tmp/duckway","install_writable":true,"capabilities":["managed_update_v1"],"components":{"proxy":"running"}}`

	req := httptest.NewRequest(http.MethodPost, "/client/control/heartbeat", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous heartbeat status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/client/control/heartbeat", bytes.NewBufferString(body))
	req.Header.Set("X-Duckway-Token", clientToken)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated heartbeat status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/client-updates", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous admin API status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/client-updates", nil)
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated admin API status=%d body=%s", rec.Code, rec.Body.String())
	}

	rolloutBody := `{"target_version":"v2","max_concurrency":1,"start_interval_seconds":0,"failure_threshold_percent":50}`
	req = httptest.NewRequest(http.MethodPost, "/api/client-updates", bytes.NewBufferString(rolloutBody))
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rollout status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/client/control/heartbeat", bytes.NewBufferString(body))
	req.Header.Set("X-Duckway-Token", clientToken)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var heartbeat controlplane.HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &heartbeat); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || heartbeat.Command == nil || heartbeat.Command.TargetVersion != "v2" || heartbeat.Command.Size != 2<<20 {
		t.Fatalf("leased command status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, status := range []controlplane.JobStatusRequest{
		{LeaseToken: heartbeat.Command.LeaseToken, Status: controlplane.JobRunning, RunningVersion: "v1"},
		{LeaseToken: heartbeat.Command.LeaseToken, Status: controlplane.JobHealthy, RunningVersion: "v2"},
	} {
		statusBody, _ := json.Marshal(status)
		req = httptest.NewRequest(http.MethodPost, "/client/control/jobs/"+heartbeat.Command.ID+"/status", bytes.NewReader(statusBody))
		req.Header.Set("X-Duckway-Token", clientToken)
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("job status %s=%d body=%s", status.Status, rec.Code, rec.Body.String())
		}
	}
}
