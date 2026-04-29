package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// CheckServerVersion fetches the gateway's reported build version from
// GET /version. No auth required.
func CheckServerVersion(serverURL string) (string, error) {
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Get(serverURL + "/version")
	if err != nil {
		return "", fmt.Errorf("contact server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("server returned %d", resp.StatusCode)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if payload.Version == "" {
		return "", fmt.Errorf("server returned empty version")
	}
	return payload.Version, nil
}

// DownloadAndReplaceClient downloads the appropriate client binary for the
// current OS/arch from the gateway and atomically replaces the running
// executable. The running daemon (if any) keeps the old inode until it
// exits — the user must restart it after `duckway update` to pick up the
// new binary.
func DownloadAndReplaceClient(serverURL string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}

	osName := runtime.GOOS
	arch := runtime.GOARCH
	switch osName {
	case "linux", "darwin":
		// supported
	default:
		return fmt.Errorf("auto-update not supported on %s — download manually from %s/download/", osName, serverURL)
	}
	switch arch {
	case "amd64", "arm64":
		// supported
	default:
		return fmt.Errorf("auto-update not supported on %s/%s — download manually from %s/download/", osName, arch, serverURL)
	}

	url := fmt.Sprintf("%s/download/duckway-client-%s-%s", serverURL, osName, arch)
	cli := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cli.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d (server probably has no client binaries — check %s/download/)", resp.StatusCode, serverURL)
	}

	// Write to temp file in the same directory so os.Rename is atomic
	// (must be on the same filesystem). Permission-denied here usually
	// means the user doesn't own the install dir — they need sudo.
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".duckway-update-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w (try with sudo)", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // best-effort cleanup if we don't reach Rename

	written, err := io.Copy(tmp, resp.Body)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("write binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if written < 1024*1024 {
		return fmt.Errorf("downloaded binary suspiciously small (%d bytes) — refusing to replace", written)
	}

	// Match permissions of the existing binary (typically 0755)
	mode := os.FileMode(0755)
	if info, err := os.Stat(exe); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Atomic replace. On Linux/macOS, rename works even when the binary is
	// currently executing — the running process holds the old inode open.
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("replace %s: %w (try with sudo)", exe, err)
	}
	return nil
}
