package main

import (
	"bytes"
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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/client"
	"github.com/hackerduck/duckway/internal/clientplugin"
)

func TestPluginCommandInstallRunAndProxy(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "duckway")
	// A fixture executable, passed directly to exec; shell metacharacters in
	// plugin arguments remain literal because the host never constructs a script.
	artifact := []byte("#!/bin/sh\nprintf '%s\\n' \"$HTTPS_PROXY\" \"$DUCKWAY_PLUGIN_NAME\" \"$1\"\ncat\nprintf 'stderr' >&2\nexit 29\n")
	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	manifest := clientplugin.Manifest{APIVersion: 1, Name: "duckllo", Version: "sha256-" + digest, Artifacts: []clientplugin.Artifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "/client/plugins/duckllo/releases/sha256-" + digest + "/" + runtime.GOOS + "/" + runtime.GOARCH, SHA256: digest, Size: int64(len(artifact))}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Duckway-Token") != "fixture-client-token" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/client/plugins" {
			_ = json.NewEncoder(w).Encode([]clientplugin.Manifest{manifest})
			return
		}
		if r.URL.Path == "/client/plugins/duckllo/manifest" {
			_ = json.NewEncoder(w).Encode(manifest)
		} else {
			_, _ = w.Write(artifact)
		}
	}))
	t.Cleanup(server.Close)
	if err := client.SaveConfig(configDir, &client.Config{ServerURL: server.URL, Token: "fixture-client-token", ProxyPort: 12345}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code, err := runPluginCommand(context.Background(), configDir, []string{"install", "duckllo"}, nil, &out, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("install: %d %v", code, err)
	}
	// Synthetic fixture only; no real server tokens enter a child or manifest.
	t.Setenv("HTTPS_PROXY", "http://stale.invalid")
	out.Reset()
	code, err = runPluginCommand(context.Background(), configDir, []string{"run", "duckllo", "--", "$(touch should-not-exist)"}, strings.NewReader("stdin invite"), &out, &stderr)
	if err != nil || code != 29 || out.String() != "http://127.0.0.1:12345\nduckllo\n$(touch should-not-exist)\nstdin invite" || stderr.String() != "stderr" {
		t.Fatalf("run: %d %v out=%q stderr=%q", code, err, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	code, err = runInstalledPlugin(context.Background(), configDir, "duckllo", []string{"fallback"}, strings.NewReader(""), &out, &stderr)
	if err != nil || code != 29 || !strings.Contains(out.String(), "fallback") {
		t.Fatalf("fallback: %d %v %q", code, err, out.String())
	}
	out.Reset()
	code, err = runPluginCommand(context.Background(), configDir, []string{"list"}, nil, &out, &stderr)
	if err != nil || code != 0 || !strings.Contains(out.String(), "duckllo\tsha256-") {
		t.Fatalf("list: %d %v %q", code, err, out.String())
	}
	server.Close()
	out.Reset()
	code, err = runPluginCommand(context.Background(), configDir, []string{"installed"}, nil, &out, &stderr)
	if err != nil || code != 0 || !strings.Contains(out.String(), "duckllo\tsha256-") {
		t.Fatalf("offline installed: %d %v %q", code, err, out.String())
	}
}

func TestPluginCommandInstallLocalNeedsNoDuckwayConfig(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "duckway")
	artifact := []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$DUCKWAY_PLUGIN_SOURCE\" \"$DUCKWAY_PLUGIN_ORIGIN\" \"$DUCKWAY_PLUGIN_API_VERSION\"\n")
	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	version := "sha256-" + digest
	artifactPath := "/client/releases/" + version + "/duckllo-client-" + runtime.GOOS + "-" + runtime.GOARCH
	manifest := clientplugin.Manifest{APIVersion: 1, Name: "duckllo", Version: version, Artifacts: []clientplugin.Artifact{{
		OS: runtime.GOOS, Arch: runtime.GOARCH, URL: artifactPath, SHA256: digest, Size: int64(len(artifact)),
	}}}
	packageDir := t.TempDir()
	path := filepath.Join(packageDir, filepath.FromSlash(strings.TrimPrefix(artifactPath, "/")))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, artifact, 0700); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "manifest.json"), manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code, err := runPluginCommand(context.Background(), configDir, []string{"install-local", "--dir", packageDir}, nil, &out, &stderr)
	if err != nil || code != 0 || !strings.Contains(out.String(), "Installed local plugin duckllo") {
		t.Fatalf("local install: code=%d err=%v out=%q stderr=%q", code, err, out.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(configDir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("local install created or changed Duckway config: %v", err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	if err := client.SaveConfig(configDir, &client.Config{ServerURL: server.URL, Token: "synthetic-token"}); err != nil {
		t.Fatal(err)
	}
	managedHistory := clientplugin.Host{ConfigDir: configDir, ServerURL: server.URL, Token: "synthetic-token"}
	if err := managedHistory.RecordOperation(context.Background(), "other-managed", "install", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runPluginCommand(context.Background(), configDir, []string{"update", "duckllo"}, nil, &out, &stderr); !errors.Is(err, clientplugin.ErrLocalInstall) {
		t.Fatalf("managed update did not reject local provenance: %v", err)
	}
	if _, err := runPluginCommand(context.Background(), configDir, []string{"install", "duckllo"}, nil, &out, &stderr); !errors.Is(err, clientplugin.ErrLocalInstall) {
		t.Fatalf("managed install did not reject local provenance: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("managed install/update for local plugin attempted %d Duckway server requests", requests.Load())
	}
	out.Reset()
	code, err = runPluginCommand(context.Background(), configDir, []string{"installed"}, nil, &out, &stderr)
	if err != nil || code != 0 || !strings.Contains(out.String(), "duckllo\t"+version+"\tlocal") {
		t.Fatalf("local installed provenance: code=%d err=%v out=%q", code, err, out.String())
	}
	out.Reset()
	code, err = runPluginCommand(context.Background(), configDir, []string{"run", "duckllo", "--"}, strings.NewReader(""), &out, &stderr)
	if err != nil || code != 0 || out.String() != "local||1" {
		t.Fatalf("local plugin run: code=%d err=%v out=%q", code, err, out.String())
	}
	if requests.Load() != 0 {
		t.Fatalf("local plugin run attempted %d Duckway server requests", requests.Load())
	}
}

func TestPluginCommandRejectsAmbiguousArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"install"}, {"install", "https://publisher.invalid/manifest"}, {"install", "--manifest", "https://user:password@example.invalid/m"}, {"install-local"}, {"install-local", "--dir"}, {"install-local", "--dir", "pkg", "extra"}, {"run", "duckllo", "arg"}, {"list", "extra"}, {"update"}, {"remove", "duckllo"}} {
		var out bytes.Buffer
		code, err := runPluginCommand(context.Background(), filepath.Join(t.TempDir(), "config"), args, nil, &out, &out)
		if err == nil || code == 0 {
			t.Fatalf("accepted %v", args)
		}
		if strings.Contains(err.Error(), "password") {
			t.Fatal("error revealed URL credentials")
		}
	}
}

func TestPluginActivationSurvivesReportDeliveryFailure(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	artifact := []byte("#!/bin/sh\nexit 0\n")
	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	manifest := clientplugin.Manifest{APIVersion: 1, Name: "duckllo", Version: "sha256-" + digest, Artifacts: []clientplugin.Artifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "/client/plugins/duckllo/releases/sha256-" + digest + "/" + runtime.GOOS + "/" + runtime.GOARCH, SHA256: digest, Size: int64(len(artifact))}}}
	var reportStatus atomic.Int32
	reportStatus.Store(503)
	var corrupt atomic.Bool
	var reportMu sync.Mutex
	var last clientplugin.Report
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Duckway-Token") != "synthetic-client-token" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/client/plugins/duckllo/manifest":
			_ = json.NewEncoder(w).Encode(manifest)
		case "/client/plugins/report":
			var report clientplugin.Report
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Error(err)
			}
			reportMu.Lock()
			last = report
			reportMu.Unlock()
			w.WriteHeader(int(reportStatus.Load()))
		default:
			if corrupt.Load() {
				_, _ = w.Write(bytes.Repeat([]byte("x"), len(artifact)))
			} else {
				_, _ = w.Write(artifact)
			}
		}
	}))
	t.Cleanup(server.Close)
	if err := client.SaveConfig(configDir, &client.Config{ServerURL: server.URL, Token: "synthetic-client-token"}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code, err := runPluginCommand(context.Background(), configDir, []string{"install", "duckllo"}, nil, &out, &stderr)
	if err != nil || code != 0 || !strings.Contains(out.String(), "Installed duckllo") || !strings.Contains(stderr.String(), "not reported") {
		t.Fatalf("delivery failure changed activation result: code=%d err=%v out=%q stderr=%q", code, err, out.String(), stderr.String())
	}
	host := clientplugin.Host{ConfigDir: configDir, ServerURL: server.URL, Token: "synthetic-client-token"}
	reportStatus.Store(204)
	if err := host.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	reportMu.Lock()
	first := last
	reportMu.Unlock()
	if len(first.Plugins) != 1 || first.Plugins[0].LastUpdate.Status != "succeeded" {
		t.Fatalf("successful result was not retried: %+v", first)
	}
	corrupt.Store(true)
	out.Reset()
	stderr.Reset()
	code, err = runPluginCommand(context.Background(), configDir, []string{"update", "duckllo"}, nil, &out, &stderr)
	if err == nil || code == 0 {
		t.Fatal("corrupt update succeeded")
	}
	reportMu.Lock()
	failed := last
	reportMu.Unlock()
	if failed.Plugins[0].Version != manifest.Version || failed.Plugins[0].LastUpdate.Status != "failed" || failed.Plugins[0].LastUpdate.ErrorCode != "verification_failed" {
		t.Fatalf("failed update reporting changed current version: %+v", failed)
	}
}
