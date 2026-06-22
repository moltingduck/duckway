package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CheckServerVersion fetches the gateway's reported build version from
// GET /version. No auth required.
func CheckServerVersion(serverURL string) (string, error) {
	cli := &http.Client{Timeout: 10 * time.Second, Transport: directTransport}
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
	cli := &http.Client{Timeout: 5 * time.Minute, Transport: directTransport}
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
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // best-effort cleanup if we don't reach Rename

	written, err := io.Copy(tmp, &progressReader{
		r:     resp.Body,
		total: resp.ContentLength, // -1 if unknown
		start: time.Now(),
	})
	fmt.Print("\r\033[K") // clear the progress line
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
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}

// progressReader wraps an io.Reader and prints a progress bar to stdout.
type progressReader struct {
	r       io.Reader
	total   int64 // -1 if Content-Length unknown
	read    int64
	start   time.Time
	lastPct int
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.read += int64(n)
	p.print()
	return n, err
}

const barWidth = 25

func (p *progressReader) print() {
	elapsed := time.Since(p.start).Seconds()
	speed := float64(p.read) / elapsed // bytes/s

	if p.total > 0 {
		pct := int(float64(p.read) / float64(p.total) * 100)
		if pct == p.lastPct {
			return
		}
		p.lastPct = pct
		filled := barWidth * pct / 100
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		fmt.Printf("\rDownloading  [%s]  %3d%%  %s / %s  %s/s",
			bar, pct, fmtBytes(p.read), fmtBytes(p.total), fmtBytes(int64(speed)))
	} else {
		// Unknown total — just show bytes received and speed.
		fmt.Printf("\rDownloading  %s  %s/s", fmtBytes(p.read), fmtBytes(int64(speed)))
	}
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
