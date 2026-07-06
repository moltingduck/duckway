package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleClientUpdateInfoReturnsPinnedBinaryManifest(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "duckway-client-linux-amd64")
	if err := os.WriteFile(binaryPath, []byte("test binary"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUCKWAY_CLIENT_RECOMMENDED_VERSION", "v-test")
	t.Setenv("DUCKWAY_CLIENT_UPDATE_REQUIRED", "1")

	req := httptest.NewRequest(http.MethodGet, "/client/update-info?os=linux&arch=amd64&version=v-old", nil)
	rec := httptest.NewRecorder()
	handleClientUpdateInfo(rec, req, dir)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["binary"] != "duckway-client-linux-amd64" {
		t.Fatalf("binary = %v", got["binary"])
	}
	if got["download_url"] != "/download/duckway-client-linux-amd64" {
		t.Fatalf("download_url = %v", got["download_url"])
	}
	if got["client_recommended_version"] != "v-test" {
		t.Fatalf("recommended = %v", got["client_recommended_version"])
	}
	if got["update_required"] != true {
		t.Fatalf("update_required = %v", got["update_required"])
	}
	if sha, _ := got["sha256"].(string); len(sha) != 64 {
		t.Fatalf("sha256 = %q", sha)
	}
}

func TestHandleClientUpdateInfoRejectsUnsupportedPlatform(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/client/update-info?os=windows&arch=amd64", nil)
	rec := httptest.NewRecorder()
	handleClientUpdateInfo(rec, req, t.TempDir())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
