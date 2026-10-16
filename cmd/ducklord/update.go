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
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"

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
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Geteuid() == 0 {
			if u, e := userHome(sudoUser); e == nil {
				home = u
			}
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		metadataPath := updateMetadataPath(home, exe)
		b, err := os.ReadFile(metadataPath)
		if err != nil {
			return errors.New("this Ducklord is not managed by Duckway; install it with Duckway or update it manually")
		}
		var metadata struct {
			Managed bool   `json:"managed"`
			Path    string `json:"executable_path"`
			Server  string `json:"server_url"`
		}
		if json.Unmarshal(b, &metadata) != nil || !metadata.Managed || metadata.Path != filepath.Clean(exe) || metadata.Server == "" {
			return errors.New("duckway update metadata is invalid; reinstall Ducklord with Duckway or update it manually")
		}
		server = metadata.Server
	}
	base, err := url.Parse(server)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") || base.Opaque != "" {
		return errors.New("invalid Duckway server URL")
	}
	if base.Scheme != "https" && (base.Scheme != "http" || !isLoopbackHost(base.Hostname())) {
		return errors.New("ducklord updates require HTTPS (HTTP is allowed only for loopback servers)")
	}
	base.Path = ""
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
	manifest, err := fetchDucklordManifest(client, manifestURL)
	if err != nil {
		return err
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
	lock, err := os.OpenFile(exe+".update.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open update lock (system installs may require sudo): %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock Ducklord update: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	alreadyUpdated, err := installedBinaryMatches(exe, manifest)
	if err != nil {
		return fmt.Errorf("recheck installed Ducklord after acquiring update lock: %w", err)
	}
	if alreadyUpdated {
		fmt.Fprintf(out, "Ducklord is already updated to %s.\n", manifest.Version)
		return nil
	}
	var tmpName string
	for attempt := 0; attempt < 2; attempt++ {
		downloadURL := *base
		downloadURL.Path += manifest.DownloadPath
		dl, err := client.Get(downloadURL.String())
		if err != nil {
			return fmt.Errorf("download Ducklord update: %w", err)
		}
		if dl.StatusCode != http.StatusOK {
			dl.Body.Close()
			return fmt.Errorf("duckway artifact download failed with status %d", dl.StatusCode)
		}
		tmp, err := os.CreateTemp(filepath.Dir(exe), ".ducklord-update-*")
		if err != nil {
			dl.Body.Close()
			return err
		}
		tmpName = tmp.Name()
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(dl.Body, manifest.Size+1))
		dl.Body.Close()
		if copyErr != nil {
			tmp.Close()
			os.Remove(tmpName)
			return copyErr
		}
		if n != manifest.Size || hex.EncodeToString(h.Sum(nil)) != strings.ToLower(manifest.SHA256) {
			tmp.Close()
			os.Remove(tmpName)
			if attempt != 0 {
				return errors.New("downloaded Ducklord failed size or SHA-256 verification after refreshing update information")
			}
			refreshed, refreshErr := fetchDucklordManifest(client, manifestURL)
			if refreshErr != nil {
				return fmt.Errorf("artifact changed during publication and manifest refresh failed: %w", refreshErr)
			}
			if refreshed.Version != manifest.Version {
				return errors.New("ducklord update changed during download; rerun update to review the new version")
			}
			manifest = refreshed
			continue
		}
		if err := tmp.Chmod(info.Mode().Perm() | 0100); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpName)
			return err
		}
		break
	}
	defer os.Remove(tmpName)
	previous := exe + ".previous"
	if err := promoteDucklordBinary(exe, tmpName); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated Ducklord to %s. Previous executable: %s\n", manifest.Version, previous)
	return nil
}

func fetchDucklordManifest(client *http.Client, manifestURL url.URL) (ducklordUpdateManifest, error) {
	resp, err := client.Get(manifestURL.String())
	if err != nil {
		return ducklordUpdateManifest{}, fmt.Errorf("fetch Ducklord update information: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ducklordUpdateManifest{}, fmt.Errorf("duckway server has no Ducklord update for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var manifest ducklordUpdateManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("invalid update manifest: %w", err)
	}
	if !validRecommendedVersion(manifest.Version) {
		return manifest, errors.New("duckway server returned an invalid recommended version")
	}
	wantBinary := "ducklord-" + runtime.GOOS + "-" + runtime.GOARCH
	_, hashErr := hex.DecodeString(manifest.SHA256)
	if manifest.OS != runtime.GOOS || manifest.Arch != runtime.GOARCH || manifest.Binary != wantBinary || manifest.DownloadPath != "/download/"+wantBinary || manifest.Size <= 0 || len(manifest.SHA256) != 64 || hashErr != nil {
		return manifest, errors.New("duckway server returned an invalid or unsupported Ducklord artifact")
	}
	return manifest, nil
}

func promoteDucklordBinary(executable, staged string) error {
	previous := executable + ".previous"
	backupStage := previous + ".new"
	if err := os.Remove(backupStage); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove incomplete rollback staging file: %w", err)
	}
	if err := os.Link(executable, backupStage); err != nil {
		return fmt.Errorf("preserve previous Ducklord executable (system installs may require sudo): %w", err)
	}
	if err := os.Rename(backupStage, previous); err != nil {
		if cleanupErr := os.Remove(backupStage); cleanupErr != nil && !os.IsNotExist(cleanupErr) {
			return fmt.Errorf("record previous Ducklord executable: %w; temporary backup remains at %s (cleanup failed: %v)", err, backupStage, cleanupErr)
		}
		return fmt.Errorf("record previous Ducklord executable: %w; live executable remains in place", err)
	}
	if err := os.Rename(staged, executable); err != nil {
		return fmt.Errorf("install updated Ducklord atomically: %w; original executable remains in place and was preserved at %s", err, previous)
	}
	return nil
}

func validRecommendedVersion(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, r := range v {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_+", r) {
			return false
		}
	}
	return true
}

func installedBinaryMatches(executable string, manifest ducklordUpdateManifest) (bool, error) {
	info, err := os.Stat(executable)
	if err != nil {
		return false, err
	}
	if info.Size() != manifest.Size {
		return false, nil
	}
	f, err := os.Open(executable)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(manifest.SHA256), nil
}

func updateMetadataPath(home, executable string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(executable)))
	return filepath.Join(home, ".ducklord", "updates", hex.EncodeToString(sum[:])+".json")
}

func userHome(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host) }
