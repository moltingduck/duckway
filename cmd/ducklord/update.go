package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/version"
)

type ducklordUpdateManifest struct {
	Version      string `json:"recommended_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Binary       string `json:"binary"`
	DownloadPath string `json:"download_path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

func runDucklordUpdate(args []string, out io.Writer) error {
	check, serverOverride := false, ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check":
			check = true
		case "--server":
			if i+1 >= len(args) {
				return errors.New("update --server requires a URL")
			}
			serverOverride = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown update option: %s", args[i])
		}
	}
	server := serverOverride
	if server == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(home, ".ducklord", "update.json"))
		if err != nil {
			return errors.New("this Ducklord is not managed by Duckway; install it with Duckway or update it manually")
		}
		var metadata struct {
			Managed bool   `json:"managed"`
			Server  string `json:"server_url"`
		}
		if json.Unmarshal(b, &metadata) != nil || !metadata.Managed || metadata.Server == "" {
			return errors.New("Duckway update metadata is invalid; reinstall Ducklord with Duckway or update it manually")
		}
		server = metadata.Server
	}
	base, err := url.Parse(server)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("invalid Duckway server URL")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && isLoopbackHost(base.Hostname())) {
		return errors.New("Ducklord updates require HTTPS (HTTP is allowed only for loopback servers)")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	manifestURL := *base
	manifestURL.Path += "/ducklord/update-info"
	query := manifestURL.Query()
	query.Set("os", runtime.GOOS)
	query.Set("arch", runtime.GOARCH)
	manifestURL.RawQuery = query.Encode()
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !sameOrigin(base, req.URL) {
			return errors.New("redirect rejected")
		}
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(manifestURL.String())
	if err != nil {
		return fmt.Errorf("fetch Ducklord update information: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Duckway server has no Ducklord update for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var manifest ducklordUpdateManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&manifest); err != nil {
		return fmt.Errorf("invalid update manifest: %w", err)
	}
	wantBinary := "ducklord-" + runtime.GOOS + "-" + runtime.GOARCH
	_, hashErr := hex.DecodeString(manifest.SHA256)
	if manifest.OS != runtime.GOOS || manifest.Arch != runtime.GOARCH || manifest.Binary != wantBinary || manifest.DownloadPath != "/download/"+wantBinary || manifest.Size <= 0 || len(manifest.SHA256) != 64 || hashErr != nil {
		return errors.New("Duckway server returned an invalid or unsupported Ducklord artifact")
	}
	current := version.Get()
	fmt.Fprintf(out, "Ducklord: %s (recommended %s)\n", current, manifest.Version)
	if current == manifest.Version {
		fmt.Fprintln(out, "Ducklord is up to date.")
		return nil
	}
	if check {
		return nil
	}
	fmt.Fprintf(out, "Update Ducklord to %s? [y/N] ", manifest.Version)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes" {
		fmt.Fprintln(out, "Update cancelled.")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	info, err := os.Lstat(exe)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to update a non-regular or symlink executable")
	}
	downloadURL := *base
	downloadURL.Path += manifest.DownloadPath
	dl, err := client.Get(downloadURL.String())
	if err != nil {
		return fmt.Errorf("download Ducklord update: %w", err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		return fmt.Errorf("Duckway artifact download failed with status %d", dl.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".ducklord-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(dl.Body, manifest.Size+1))
	if copyErr != nil {
		tmp.Close()
		return copyErr
	}
	if n != manifest.Size || hex.EncodeToString(h.Sum(nil)) != strings.ToLower(manifest.SHA256) {
		tmp.Close()
		return errors.New("downloaded Ducklord failed size or SHA-256 verification")
	}
	if err := tmp.Chmod(info.Mode().Perm() | 0100); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	previous := exe + ".previous"
	_ = os.Remove(previous)
	if err := os.Rename(exe, previous); err != nil {
		return fmt.Errorf("preserve previous Ducklord executable: %w", err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		_ = os.Rename(previous, exe)
		return fmt.Errorf("install updated Ducklord: %w", err)
	}
	fmt.Fprintf(out, "Updated Ducklord to %s. Previous executable: %s\n", manifest.Version, previous)
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host) }
