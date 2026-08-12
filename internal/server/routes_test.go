package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
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
	if err := os.WriteFile(filepath.Join(dir, "ducklord-linux-amd64"), []byte("lord"), 0600); err != nil {
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

	req = httptest.NewRequest(http.MethodGet, "/download/ducklord-linux-amd64", nil)
	req.SetPathValue("binary", "ducklord-linux-amd64")
	rec = httptest.NewRecorder()
	serveClientDownload(rec, req, dir)
	if rec.Code != http.StatusOK || rec.Body.String() != "lord" {
		t.Fatalf("ducklord download status=%d body=%q", rec.Code, rec.Body.String())
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
		`Install components:`,
		`Use j/k to move, Space to toggle, Enter to continue.`,
		`Duckway client + Ducklion`,
		`Ducklord`,
		`component_client=1`,
		`component_ducklord=0`,
		`component_key="$(dd bs=1 count=1`,
		`INSTALL_COMPONENT="all"`,
		`INSTALL_COMPONENT="ducklord"`,
		`DUCKLORD_BINARY="ducklord-${OS}-${ARCH}"`,
		`download_tool "$DUCKLORD_BINARY" "$TMP_DIR/ducklord"`,
		`DUCKLORD_DEST="$DEST_DIR/ducklord"`,
		`DUCKLORD_DEST="$DEST"`,
		`Install location:`,
		`read choice < /dev/tty`,
		`DEST="${DUCKWAY_INSTALL_PATH:-$HOME/.local/bin/$PRIMARY_NAME}"`,
		`DUCKWAY_INSTALL_PATH="$custom_path"`,
		`INSTALL_MODE="custom"`,
		`sudo mkdir -p "$DEST_DIR"`,
		`DUCKLION_BINARY="ducklion-${OS}-${ARCH}"`,
		`DUCKLION_DEST="$DEST_DIR/ducklion"`,
		`sudo mv "$TMP_DIR/ducklion" "$DUCKLION_DEST"`,
	} {
		if !strings.Contains(installScript, want) {
			t.Fatalf("installScript missing %q", want)
		}
	}
}

func TestInstallScriptShellSyntax(t *testing.T) {
	script := fmt.Sprintf(installScript, "http://duckway.test", "http://duckway.test", "http://duckway.test")
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install script syntax failed: %v\n%s", err, out)
	}
}

func TestInstallScriptInstallsSelectedComponents(t *testing.T) {
	for _, tc := range []struct {
		component string
		want      []string
		notWant   []string
	}{
		{component: "client", want: []string{"duckway", "ducklion", ".duckway/ca.pem"}, notWant: []string{"ducklord"}},
		{component: "ducklord", want: []string{"ducklord"}, notWant: []string{"duckway", "ducklion", ".duckway/ca.pem"}},
		{component: "all", want: []string{"duckway", "ducklion", "ducklord", ".duckway/ca.pem"}},
	} {
		t.Run(tc.component, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			bin := filepath.Join(dir, "bin")
			tmp := filepath.Join(dir, "tmp")
			downloads := filepath.Join(dir, "downloads")
			for _, path := range []string{home, bin, tmp, downloads} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{
				"duckway-client-linux-amd64",
				"duckway-client-linux-arm64",
				"ducklion-linux-amd64",
				"ducklion-linux-arm64",
				"ducklord-linux-amd64",
				"ducklord-linux-arm64",
				"ca.pem",
			} {
				if err := os.WriteFile(filepath.Join(downloads, name), []byte(name+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			curl := filepath.Join(bin, "curl")
			if err := os.WriteFile(curl, []byte(`#!/bin/sh
set -e
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
name="${url##*/}"
cp "$DUCKWAY_TEST_DOWNLOADS/$name" "$out"
`), 0700); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "install.sh")
			if err := os.WriteFile(script, []byte(fmt.Sprintf(installScript, "http://duckway.test", "http://duckway.test", "http://duckway.test")), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", script)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"HOME="+home,
				"TMPDIR="+tmp,
				"DUCKWAY_INSTALL=user",
				"DUCKWAY_INSTALL_COMPONENT="+tc.component,
				"DUCKWAY_TEST_DOWNLOADS="+downloads,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			for _, want := range tc.want {
				if _, err := os.Stat(filepath.Join(home, ".local", "bin", want)); err != nil {
					if want == ".duckway/ca.pem" {
						if _, caErr := os.Stat(filepath.Join(home, want)); caErr == nil {
							continue
						}
					}
					t.Fatalf("missing %s after %s install\n%s", want, tc.component, out)
				}
			}
			for _, notWant := range tc.notWant {
				if _, err := os.Stat(filepath.Join(home, ".local", "bin", notWant)); err == nil {
					t.Fatalf("unexpected %s after %s install\n%s", notWant, tc.component, out)
				}
				if notWant == ".duckway/ca.pem" {
					if _, err := os.Stat(filepath.Join(home, notWant)); err == nil {
						t.Fatalf("unexpected %s after %s install\n%s", notWant, tc.component, out)
					}
				}
			}
		})
	}
}

func TestInstallScriptInteractiveCheckboxInstallsDucklordOnly(t *testing.T) {
	dir := t.TempDir()
	home, bin, tmp, downloads := installScriptTestDirs(t, dir)
	writeInstallScriptTestDownloads(t, downloads)
	writeInstallScriptTestCurl(t, filepath.Join(bin, "curl"))
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, []byte(fmt.Sprintf(installScript, "http://duckway.test", "http://duckway.test", "http://duckway.test")), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home,
		"TMPDIR="+tmp,
		"DUCKWAY_INSTALL=user",
		"DUCKWAY_TEST_DOWNLOADS="+downloads,
	)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	if _, err := tty.Write([]byte(" j \n")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interactive install failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("interactive install timed out")
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "ducklord")); err != nil {
		t.Fatalf("ducklord missing after interactive install: %v", err)
	}
	for _, unexpected := range []string{"duckway", "ducklion"} {
		if _, err := os.Stat(filepath.Join(home, ".local", "bin", unexpected)); err == nil {
			t.Fatalf("unexpected %s after ducklord-only interactive install", unexpected)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".duckway", "ca.pem")); err == nil {
		t.Fatal("unexpected ca.pem after ducklord-only interactive install")
	}
}

func installScriptTestDirs(t *testing.T, dir string) (home, bin, tmp, downloads string) {
	t.Helper()
	home = filepath.Join(dir, "home")
	bin = filepath.Join(dir, "bin")
	tmp = filepath.Join(dir, "tmp")
	downloads = filepath.Join(dir, "downloads")
	for _, path := range []string{home, bin, tmp, downloads} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return home, bin, tmp, downloads
}

func writeInstallScriptTestDownloads(t *testing.T, downloads string) {
	t.Helper()
	for _, name := range []string{
		"duckway-client-linux-amd64",
		"duckway-client-linux-arm64",
		"ducklion-linux-amd64",
		"ducklion-linux-arm64",
		"ducklord-linux-amd64",
		"ducklord-linux-arm64",
		"ca.pem",
	} {
		if err := os.WriteFile(filepath.Join(downloads, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeInstallScriptTestCurl(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`#!/bin/sh
set -e
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
name="${url##*/}"
cp "$DUCKWAY_TEST_DOWNLOADS/$name" "$out"
`), 0700); err != nil {
		t.Fatal(err)
	}
}
