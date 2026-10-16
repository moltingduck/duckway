package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	clientapi "github.com/hackerduck/duckway/internal/client"
)

func TestFetchRemoteDucklionChecksManifestAndArtifact(t *testing.T) {
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	sum := sha256.Sum256(data)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/update-info":
			if r.URL.Query().Get("os") != "linux" || r.URL.Query().Get("arch") != "arm64" {
				t.Errorf("unexpected platform query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(clientapi.UpdateInfo{OS: "linux", Arch: "arm64", DucklionBinary: "ducklion-linux-arm64", DucklionDownloadURL: "/download/ducklion-linux-arm64", DucklionSize: int64(len(data)), DucklionSHA256: hex.EncodeToString(sum[:])})
		case "/download/ducklion-linux-arm64":
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	got, err := fetchRemoteDucklion(context.Background(), server.URL, "linux", "arm64", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) || sha256.Sum256(got) != sum {
		t.Fatal("downloaded bytes differ from verified artifact")
	}
}

func TestFetchRemoteDucklionRejectsCrossOriginRedirect(t *testing.T) {
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("unexpected")) }))
	defer attacker.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("%s/steal", attacker.URL), http.StatusFound)
	}))
	defer server.Close()
	_, err := fetchRemoteDucklion(context.Background(), server.URL, "linux", "amd64", server.Client())
	if err == nil {
		t.Fatal("expected cross-origin redirect to be rejected")
	}
}

func TestFetchRemoteDucklionRejectsUnconstrainedManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(clientapi.UpdateInfo{
			OS:                  "linux",
			Arch:                "amd64",
			DucklionBinary:      "ducklion-linux-amd64",
			DucklionDownloadURL: "https://attacker.invalid/binary",
			DucklionSize:        1 << 20,
			DucklionSHA256:      fmt.Sprintf("%064x", 0),
		})
	}))
	defer server.Close()
	if _, err := fetchRemoteDucklion(context.Background(), server.URL, "linux", "amd64", server.Client()); err == nil {
		t.Fatal("expected unconstrained manifest path to be rejected")
	}
}

func TestFetchRemoteDucklionRejectsMismatchedManifestPlatform(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(clientapi.UpdateInfo{
			OS: "darwin", Arch: "amd64",
			DucklionBinary: "ducklion-linux-amd64", DucklionDownloadURL: "/download/ducklion-linux-amd64",
			DucklionSize: 1 << 20, DucklionSHA256: fmt.Sprintf("%064x", 0),
		})
	}))
	defer server.Close()
	if _, err := fetchRemoteDucklion(context.Background(), server.URL, "linux", "amd64", server.Client()); err == nil {
		t.Fatal("expected mismatched platform manifest to be rejected")
	}
}

func TestFetchRemoteDucklionRequiresHTTPSOutsideLoopback(t *testing.T) {
	if _, err := fetchRemoteDucklion(context.Background(), "http://example.com", "linux", "amd64", nil); err == nil {
		t.Fatal("expected non-loopback HTTP server to be rejected")
	}
}
