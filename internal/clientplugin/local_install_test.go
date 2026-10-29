package clientplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func writeLocalPackage(t *testing.T, name, release string, manifestContent, artifactContent []byte) string {
	t.Helper()
	manifestDigest := sha256.Sum256([]byte(release))
	version := "sha256-" + hex.EncodeToString(manifestDigest[:])
	artifactDigest := sha256.Sum256(manifestContent)
	artifactPath := "/client/releases/" + version + "/" + name + "-" + runtime.GOOS + "-" + runtime.GOARCH
	manifest := Manifest{APIVersion: APIVersion, Name: name, Version: version, Artifacts: []Artifact{{
		OS: runtime.GOOS, Arch: runtime.GOARCH, URL: artifactPath,
		SHA256: hex.EncodeToString(artifactDigest[:]), Size: int64(len(manifestContent)),
	}}}
	dir := t.TempDir()
	artifactFile := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(artifactPath, "/")))
	if err := os.MkdirAll(filepath.Dir(artifactFile), 0700); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactFile, artifactContent, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func localProbeProgram(source string) []byte {
	return []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$DUCKWAY_PLUGIN_SOURCE\" \"$DUCKWAY_PLUGIN_ORIGIN\" \"$1\"\n# " + source + "\n")
}

func TestInstallLocalNeedsNoDuckwayServerAndPreservesLocalData(t *testing.T) {
	ctx := context.Background()
	host := Host{ConfigDir: filepath.Join(t.TempDir(), "duckway")}
	firstProgram := localProbeProgram("v1")
	firstPackage := writeLocalPackage(t, "duckllo", "first", firstProgram, firstProgram)
	first, err := host.InstallLocal(ctx, firstPackage)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != SourceLocal || first.ServerOrigin != "" || first.Manifest.Version == "" {
		t.Fatalf("unexpected local provenance: %#v", first)
	}
	if _, err := os.Stat(filepath.Join(host.ConfigDir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("local install changed or created Duckway config: %v", err)
	}
	dataDir := filepath.Join(host.root(), "duckllo", "data")
	if err := os.WriteFile(filepath.Join(dataDir, "state.json"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	var stderr strings.Builder
	code, err := host.Run(ctx, "duckllo", []string{"--help"}, RunOptions{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Env: []string{}})
	if err != nil || code != 0 || stdout.String() != "local||--help" {
		t.Fatalf("local child metadata/run = %d, %v, %q", code, err, stdout.String())
	}
	secondProgram := localProbeProgram("v2")
	badPackage := writeLocalPackage(t, "duckllo", "second", secondProgram, []byte("tampered"))
	if _, err := host.InstallLocal(ctx, badPackage); !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("tampered package error = %v", err)
	}
	afterFailure, err := host.List()
	if err != nil || len(afterFailure) != 1 || afterFailure[0].Manifest.Version != first.Manifest.Version {
		t.Fatalf("failed reinstall changed active release: %#v, %v", afterFailure, err)
	}
	if data, err := os.ReadFile(filepath.Join(dataDir, "state.json")); err != nil || string(data) != "preserve" {
		t.Fatalf("failed reinstall changed plugin data: %q, %v", data, err)
	}
	secondPackage := writeLocalPackage(t, "duckllo", "second", secondProgram, secondProgram)
	second, err := host.InstallLocal(ctx, secondPackage)
	if err != nil || second.Manifest.Version == first.Manifest.Version || second.Source != SourceLocal {
		t.Fatalf("valid local replacement = %#v, %v", second, err)
	}
	if data, err := os.ReadFile(filepath.Join(dataDir, "state.json")); err != nil || string(data) != "preserve" {
		t.Fatalf("successful reinstall changed plugin data: %q, %v", data, err)
	}
}

func TestLocalManagedCollisionAndUpdateDoNotContactServer(t *testing.T) {
	f := newFixture(t)
	managed := f.install(t)
	localProgram := localProbeProgram("local")
	packageDir := writeLocalPackage(t, "duckllo", "local", localProgram, localProgram)
	if _, err := f.host.InstallLocal(context.Background(), packageDir); err == nil || !strings.Contains(err.Error(), "server-managed") {
		t.Fatalf("local install replaced managed plugin: %v", err)
	}
	if _, err := f.host.Update(context.Background(), "duckllo"); err != nil {
		t.Fatalf("managed update unexpectedly failed: %v", err)
	}
	if managed.Source != SourceManaged {
		t.Fatalf("managed install source = %q", managed.Source)
	}

	requests := 0
	var report Report
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/client/plugins/report" {
			http.NotFound(w, r)
			return
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			http.Error(w, "bad report", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	localHost := Host{ConfigDir: filepath.Join(t.TempDir(), "duckway"), ServerURL: server.URL, Token: "test-token", HTTPClient: server.Client()}
	if err := localHost.RecordOperation(context.Background(), "duckllo", "install", ErrVerificationFailed); err != nil {
		t.Fatalf("seed historical managed first-install result: %v", err)
	}
	if requests != 0 {
		t.Fatalf("saving a pending result made %d HTTP requests", requests)
	}
	if _, err := localHost.InstallLocal(context.Background(), packageDir); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("local install made %d HTTP requests", requests)
	}
	if _, err := localHost.Update(context.Background(), "duckllo"); !errors.Is(err, ErrLocalInstall) {
		t.Fatalf("server update for local install error = %v", err)
	}
	if _, err := localHost.Install(context.Background(), "duckllo"); !errors.Is(err, ErrLocalInstall) {
		t.Fatalf("server install over local install error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("managed install/update made %d HTTP requests before rejecting local provenance", requests)
	}
	if err := localHost.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("local-only report made %d HTTP requests", requests)
	}
	if len(report.Plugins) != 0 {
		t.Fatalf("local plugin leaked into server inventory: %#v", report)
	}
}

func TestInstallLocalRejectsSymlinksTraversalAndFIFO(t *testing.T) {
	program := localProbeProgram("local")
	packageDir := writeLocalPackage(t, "duckllo", "safe", program, program)
	manifest, err := os.ReadFile(filepath.Join(packageDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(packageDir, filepath.FromSlash(strings.TrimPrefix(decoded.Artifacts[0].URL, "/")))
	host := Host{ConfigDir: filepath.Join(t.TempDir(), "duckway")}

	symlinkRoot := filepath.Join(t.TempDir(), "package-link")
	if err := os.Symlink(packageDir, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := host.InstallLocal(context.Background(), symlinkRoot); err == nil {
		t.Fatal("symlink package root accepted")
	}

	outside := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(outside, program, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, artifactPath); err != nil {
		t.Fatal(err)
	}
	if _, err := host.InstallLocal(context.Background(), packageDir); err == nil {
		t.Fatal("symlink artifact accepted")
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(artifactPath, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if _, err := host.InstallLocal(ctx, packageDir); err == nil {
		t.Fatal("FIFO artifact accepted")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("FIFO artifact open blocked")
	}

	decoded.Artifacts[0].URL = "/../../outside"
	badManifest, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "manifest.json"), badManifest, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.InstallLocal(context.Background(), packageDir); err == nil {
		t.Fatal("traversal artifact path accepted")
	}
}
