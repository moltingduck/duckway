package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type UpdateInfo struct {
	ServerVersion            string `json:"server_version"`
	ClientCurrentVersion     string `json:"client_current_version,omitempty"`
	ClientRecommendedVersion string `json:"client_recommended_version"`
	ClientMinVersion         string `json:"client_min_version,omitempty"`
	UpdateRequired           bool   `json:"update_required"`
	UpdateRecommended        bool   `json:"update_recommended"`
	RestartRequired          bool   `json:"restart_required"`
	Reason                   string `json:"reason,omitempty"`
	OS                       string `json:"os"`
	Arch                     string `json:"arch"`
	Binary                   string `json:"binary"`
	DownloadURL              string `json:"download_url"`
	SHA256                   string `json:"sha256"`
	Size                     int64  `json:"size,omitempty"`
}

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

func CheckUpdateInfo(serverURL, currentVersion string) (*UpdateInfo, error) {
	info, err := fetchUpdateInfo(serverURL, currentVersion, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	if err := validateUpdateInfo(info, runtime.GOOS, runtime.GOARCH); err != nil {
		return nil, err
	}
	return info, nil
}

func fetchUpdateInfo(serverURL, currentVersion, osName, arch string) (*UpdateInfo, error) {
	base, err := url.Parse(strings.TrimRight(serverURL, "/") + "/client/update-info")
	if err != nil {
		return nil, err
	}
	q := base.Query()
	q.Set("version", currentVersion)
	q.Set("os", osName)
	q.Set("arch", arch)
	base.RawQuery = q.Encode()

	cli := &http.Client{Timeout: 10 * time.Second, Transport: directTransport}
	resp, err := cli.Get(base.String())
	if err != nil {
		return nil, fmt.Errorf("contact server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info UpdateInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode update info: %w", err)
	}
	return &info, nil
}

// DownloadAndReplaceClient downloads the appropriate client binary for the
// current OS/arch from the gateway and atomically replaces the running
// executable. The running daemon (if any) keeps the old inode until it
// exits — the user must restart it after `duckway update` to pick up the
// new binary.
func DownloadAndReplaceClient(serverURL string) error {
	info, err := CheckUpdateInfo(serverURL, "")
	if err != nil {
		return err
	}
	return DownloadAndReplaceClientWithInfo(serverURL, info)
}

func DownloadAndReplaceClientWithInfo(serverURL string, info *UpdateInfo) error {
	if err := validateUpdateInfo(info, runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}
	if info.SHA256 == "" {
		return fmt.Errorf("server returned update manifest without sha256 for %s", info.Binary)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}

	url, err := safeDownloadURL(serverURL, info.DownloadURL)
	if err != nil {
		return err
	}
	cli := &http.Client{Timeout: 5 * time.Minute, Transport: directTransport}
	resp, err := cli.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".duckway-update-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	w := io.MultiWriter(tmp, hasher)
	written, err := io.Copy(w, &progressReader{
		r:     resp.Body,
		total: resp.ContentLength,
		start: time.Now(),
	})
	fmt.Print("\r\033[K")
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
	if info.Size > 0 && written != info.Size {
		return fmt.Errorf("downloaded binary size mismatch: got %d want %d", written, info.Size)
	}
	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(gotSHA, info.SHA256) {
		return fmt.Errorf("sha256 mismatch for %s: got %s want %s", info.Binary, gotSHA, info.SHA256)
	}

	mode := os.FileMode(0755)
	if stat, err := os.Stat(exe); err == nil {
		mode = stat.Mode().Perm()
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}

func validateUpdateInfo(info *UpdateInfo, osName, arch string) error {
	if info == nil {
		return errors.New("server returned empty update info")
	}
	if info.ClientRecommendedVersion == "" {
		return errors.New("server returned empty recommended client version")
	}
	if info.OS != osName || info.Arch != arch {
		return fmt.Errorf("server returned manifest for %s/%s, want %s/%s", info.OS, info.Arch, osName, arch)
	}
	wantBinary := fmt.Sprintf("duckway-client-%s-%s", osName, arch)
	if info.Binary != wantBinary {
		return fmt.Errorf("server returned unexpected binary %q, want %q", info.Binary, wantBinary)
	}
	if info.DownloadURL != "/download/"+wantBinary {
		return fmt.Errorf("server returned unsafe download URL %q", info.DownloadURL)
	}
	if info.SHA256 != "" {
		if len(info.SHA256) != 64 {
			return fmt.Errorf("server returned invalid sha256 length for %s", info.Binary)
		}
		if _, err := hex.DecodeString(info.SHA256); err != nil {
			return fmt.Errorf("server returned invalid sha256 for %s: %w", info.Binary, err)
		}
	}
	return nil
}

func safeDownloadURL(serverURL, downloadPath string) (string, error) {
	if !strings.HasPrefix(downloadPath, "/download/duckway-client-") || strings.Contains(downloadPath, "..") {
		return "", fmt.Errorf("unsafe download URL %q", downloadPath)
	}
	base, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(downloadPath)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
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
