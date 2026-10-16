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

func TestUpdateOriginAndLoopbackRules(t *testing.T) {
	base, _ := url.Parse("https://duckway.example")
	for _, tc := range []struct {
		url            string
		same, loopback bool
	}{
		{"https://duckway.example/download/a", true, false},
		{"https://other.example/download/a", false, false},
		{"http://localhost:8080", false, true},
		{"http://10.0.0.2", false, false},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameOrigin(base, u); got != tc.same {
			t.Errorf("sameOrigin(%q)=%v", tc.url, got)
		}
		if got := isLoopbackHost(u.Hostname()); got != tc.loopback {
			t.Errorf("isLoopbackHost(%q)=%v", tc.url, got)
		}
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
	for _, value := range []string{"v1.2.3", "1.0.0-rc.1+build_2"} {
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
