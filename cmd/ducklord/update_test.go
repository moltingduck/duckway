package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateOriginRules(t *testing.T) {
	base, _ := url.Parse("https://duckway.example")
	for _, tc := range []struct {
		url  string
		same bool
	}{
		{"https://duckway.example/download/a", true},
		{"https://other.example/download/a", false},
		{"http://localhost:8080", false},
		{"http://10.0.0.2", false},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameOrigin(base, u); got != tc.same {
			t.Errorf("sameOrigin(%q)=%v", tc.url, got)
		}
	}
}

func TestUpdateServerURLAcceptsHTTPForRemoteHosts(t *testing.T) {
	for _, raw := range []string{"http://duckway.example", "https://duckway.example", "http://localhost:8080"} {
		base, err := url.Parse(raw)
		if err != nil || !validDucklordUpdateBaseURL(base) {
			t.Errorf("rejected valid server URL %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"ftp://duckway.example", "https://user@duckway.example", "https://duckway.example/path", "https://duckway.example?query=1"} {
		base, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if validDucklordUpdateBaseURL(base) {
			t.Errorf("accepted invalid server URL %q", raw)
		}
	}
}

func TestDownloadVerifiedDucklordRefreshesManifestAfterArtifactMismatch(t *testing.T) {
	refreshed := []byte("artifact matching refreshed manifest")
	manifestRequests, downloadRequests := 0, 0
	name := "ducklord-" + runtime.GOOS + "-" + runtime.GOARCH
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ducklord/update-info":
			manifestRequests++
			writeTestManifest(t, w, "v1.2.3", name, refreshed)
		case "/download/" + name:
			downloadRequests++
			if downloadRequests == 1 {
				_, _ = w.Write([]byte("artifact from replaced deployment"))
			} else {
				_, _ = w.Write(refreshed)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL)
	manifestURL, _ := url.Parse(server.URL + "/ducklord/update-info")
	initial := testManifest("v1.2.3", name, []byte("artifact originally published"))
	path, gotManifest, err := downloadVerifiedDucklord(server.Client(), *base, *manifestURL, initial, t.TempDir(), 0700)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(refreshed) || gotManifest.SHA256 != testManifest("v1.2.3", name, refreshed).SHA256 {
		t.Fatalf("retry returned artifact %q and manifest %+v", got, gotManifest)
	}
	if manifestRequests != 1 || downloadRequests != 2 {
		t.Fatalf("manifest refreshes=%d download requests=%d; want 1 and 2", manifestRequests, downloadRequests)
	}
}

func TestDownloadVerifiedDucklordStopsWhenVersionChangesOnRefresh(t *testing.T) {
	name := "ducklord-" + runtime.GOOS + "-" + runtime.GOARCH
	manifestRequests, downloadRequests := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ducklord/update-info":
			manifestRequests++
			version := "v1.2.4"
			writeTestManifest(t, w, version, name, []byte("not matching downloaded artifact"))
		case "/download/" + name:
			downloadRequests++
			_, _ = w.Write([]byte("stale artifact"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL)
	manifestURL, _ := url.Parse(server.URL + "/ducklord/update-info")
	initial := testManifest("v1.2.3", name, []byte("different expected artifact"))
	dir := t.TempDir()
	path, _, err := downloadVerifiedDucklord(server.Client(), *base, *manifestURL, initial, dir, 0700)
	if err == nil || !strings.Contains(err.Error(), "rerun update") {
		t.Fatalf("expected version-change error, got path=%q err=%v", path, err)
	}
	if manifestRequests != 1 || downloadRequests != 1 {
		t.Fatalf("manifest refreshes=%d download requests=%d; want 1 each", manifestRequests, downloadRequests)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed update left staged files: %v", entries)
	}
}

func testManifest(version, name string, artifact []byte) ducklordUpdateManifest {
	return ducklordUpdateManifest{
		Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH, Binary: name,
		DownloadPath: "/download/" + name, Size: int64(len(artifact)),
		SHA256: fmt.Sprintf("%x", sha256.Sum256(artifact)),
	}
}

func writeTestManifest(t *testing.T, w http.ResponseWriter, version, name string, artifact []byte) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(testManifest(version, name, artifact)); err != nil {
		t.Errorf("encode test manifest: %v", err)
	}
}

func TestUpdateMetadataIsScopedToExecutable(t *testing.T) {
	home := t.TempDir()
	a := updateMetadataPath(home, "/usr/local/bin/ducklord")
	b := updateMetadataPath(home, "/opt/duckway/ducklord")
	if a == b {
		t.Fatal("different installs share update metadata")
	}
	if !strings.HasPrefix(a, filepath.Join(home, ".ducklord", "updates")) {
		t.Fatalf("unexpected metadata path %q", a)
	}
}

func TestValidRecommendedVersion(t *testing.T) {
	for _, value := range []string{"v1.2.3", "1.0.0-rc.1+build_2", "commit abc1234 (built 2026-04-30, go1.25)", "dev (go1.25)"} {
		if !validRecommendedVersion(value) {
			t.Errorf("rejected %q", value)
		}
	}
	for _, value := range []string{"", "../bin/ducklord", "v1;touch /tmp/pwned", strings.Repeat("x", 129)} {
		if validRecommendedVersion(value) {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestInstalledBinaryMatchesManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ducklord")
	contents := []byte("same release")
	if err := os.WriteFile(path, contents, 0700); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(contents))
	manifest := ducklordUpdateManifest{Size: int64(len(contents)), SHA256: hash}
	matched, err := installedBinaryMatches(path, manifest)
	if err != nil || !matched {
		t.Fatalf("matching installed binary: matched=%v err=%v", matched, err)
	}
	manifest.SHA256 = strings.Repeat("0", 64)
	matched, err = installedBinaryMatches(path, manifest)
	if err != nil || matched {
		t.Fatalf("different installed binary: matched=%v err=%v", matched, err)
	}
}

func TestPromoteDucklordBinaryAtomicAndFailurePreservesExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "ducklord")
	if err := os.WriteFile(exe, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, []byte("new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := promoteDucklordBinary(exe, staged); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	prev, _ := os.ReadFile(exe + ".previous")
	if string(got) != "new" || string(prev) != "old" {
		t.Fatalf("current=%q previous=%q", got, prev)
	}
	if err := promoteDucklordBinary(exe, filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "original executable remains") {
		t.Fatalf("expected clear failed promotion, got %v", err)
	}
	got, _ = os.ReadFile(exe)
	if string(got) != "new" {
		t.Fatalf("failed promotion changed executable to %q", got)
	}
}

func TestUpdateRejectsInvalidManifestBeforePrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ducklordUpdateManifest{Version: "bad;version", OS: "linux", Arch: "amd64", Binary: "ducklord-linux-amd64", DownloadPath: "/download/ducklord-linux-amd64", Size: 1, SHA256: strings.Repeat("a", 64)})
	}))
	defer server.Close()
	err := runDucklordUpdate([]string{"--check", "--server", server.URL}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "invalid recommended version") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateServerURLAllowsSingleTrailingSlash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ducklord/update-info" {
			t.Errorf("manifest path=%q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(ducklordUpdateManifest{Version: "dev", OS: runtime.GOOS, Arch: runtime.GOARCH, Binary: "ducklord-" + runtime.GOOS + "-" + runtime.GOARCH, DownloadPath: "/download/ducklord-" + runtime.GOOS + "-" + runtime.GOARCH, Size: 1, SHA256: strings.Repeat("a", 64)})
	}))
	defer server.Close()
	err := runDucklordUpdate([]string{"--check", "--server", server.URL + "/"}, &strings.Builder{})
	if err != nil {
		t.Fatalf("server URL with trailing slash rejected: %v", err)
	}
}
