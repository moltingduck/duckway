package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleClientUpdateInfoReturnsPinnedBinaryManifest(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "duckway-client-linux-amd64")
	if err := os.WriteFile(binaryPath, []byte("test binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ducklion-linux-amd64"), []byte("ducklion binary"), 0600); err != nil {
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
	if got["ducklion_binary"] != "ducklion-linux-amd64" {
		t.Fatalf("ducklion_binary = %v", got["ducklion_binary"])
	}
	if got["ducklion_download_url"] != "/download/ducklion-linux-amd64" {
		t.Fatalf("ducklion_download_url = %v", got["ducklion_download_url"])
	}
	if sha, _ := got["ducklion_sha256"].(string); len(sha) != 64 {
		t.Fatalf("ducklion_sha256 = %q", sha)
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

func TestServeClientDownloadOnlyAllowsPinnedBinaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "duckway-client-linux-amd64"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ducklion-linux-amd64"), []byte("lion"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/download/duckway-client-linux-amd64", nil)
	req.SetPathValue("binary", "duckway-client-linux-amd64")
	rec := httptest.NewRecorder()
	serveClientDownload(rec, req, dir)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("allowed download status=%d body=%q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/download/ducklion-linux-amd64", nil)
	req.SetPathValue("binary", "ducklion-linux-amd64")
	rec = httptest.NewRecorder()
	serveClientDownload(rec, req, dir)
	if rec.Code != http.StatusOK || rec.Body.String() != "lion" {
		t.Fatalf("ducklion download status=%d body=%q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/download/secret.txt", nil)
	req.SetPathValue("binary", "secret.txt")
	rec = httptest.NewRecorder()
	serveClientDownload(rec, req, dir)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("secret download status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestInstallScriptSupportsUserLocalInstall(t *testing.T) {
	for _, want := range []string{
		`Install location:`,
		`read choice < /dev/tty`,
		`DEST="${DUCKWAY_INSTALL_PATH:-$HOME/.local/bin/duckway}"`,
		`DUCKWAY_INSTALL_PATH="$custom_path"`,
		`INSTALL_MODE="custom"`,
		`sudo mkdir -p "$DEST_DIR"`,
		`DUCKLION_BINARY="ducklion-${OS}-${ARCH}"`,
		`DUCKLION_DEST="$DEST_DIR/ducklion"`,
		`sudo mv /tmp/ducklion "$DUCKLION_DEST"`,
	} {
		if !strings.Contains(installScript, want) {
			t.Fatalf("installScript missing %q", want)
		}
	}
}
