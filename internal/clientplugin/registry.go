package clientplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

var ErrNotInstalled = errors.New("plugin is not installed")
var ErrDownloadFailed = errors.New("managed plugin download failed")
var ErrVerificationFailed = errors.New("plugin verification failed")
var ErrServerChanged = errors.New("configured Duckway server changed; explicit plugin reinstall or migration is required")
var ErrLocalInstall = errors.New("plugin is installed locally; use plugin install-local --dir PATH to replace it")

const (
	SourceManaged = "managed"
	SourceLocal   = "local"
)

// Host's config directory must be private to the current OS user. HTTPClient is
// optional; its redirect policy is always overridden for plugin downloads.
type Host struct {
	ConfigDir  string
	ServerURL  string
	Token      string
	HTTPClient *http.Client
	OS         string
	Arch       string
}

type Installed struct {
	Manifest     Manifest `json:"manifest"`
	Source       string   `json:"source,omitempty"`
	ServerOrigin string   `json:"server_origin,omitempty"`
	Release      string   `json:"release"`
	Artifact     Artifact `json:"artifact"`
}

func (h Host) root() string { return filepath.Join(h.ConfigDir, "plugins") }
func (h Host) platform() (string, string) {
	goos, goarch := h.OS, h.Arch
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// privateDir rejects symlink components, foreign ownership and group/other write
// access before creating a registry directory. Ancestors may be public (e.g.
// /tmp) but the config directory and every registry directory must be private.
func privateDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := noSymlinkParents(abs); err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("plugin registry requires real directories")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("plugin registry directory must be private and owned by current user")
	}
	return nil
}

func noSymlinkParents(dir string) error {
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("plugin registry path contains a symlink or non-directory")
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
				return fmt.Errorf("plugin registry ancestor has unsafe ownership")
			}
			if info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return fmt.Errorf("plugin registry ancestor is writable without sticky protection")
			}
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}

func regularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("plugin file must be regular, owned and not writable by others")
	}
	return nil
}

func (h Host) lock(ctx context.Context, exclusive bool) (*os.File, error) {
	return h.lockNamed(ctx, ".lock", exclusive)
}

func (h Host) lockNamed(ctx context.Context, lockName string, exclusive bool) (*os.File, error) {
	if h.ConfigDir == "" {
		return nil, fmt.Errorf("plugin config directory is required")
	}
	if err := privateDir(h.ConfigDir); err != nil {
		return nil, err
	}
	if err := privateDir(h.root()); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(h.root(), lockName)
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), lockPath)
	if err := regularFile(lockPath); err != nil {
		f.Close()
		return nil, err
	}
	op := syscall.LOCK_SH
	if exclusive {
		op = syscall.LOCK_EX
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := syscall.Flock(fd, op|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return f, nil
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

func (h Host) get(ctx context.Context, path string) (*http.Response, error) {
	origin, err := h.remoteOrigin()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, "/client/plugins") || strings.ContainsAny(path, "?#\\") {
		return nil, fmt.Errorf("invalid managed plugin route")
	}
	c := h.managedClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid managed plugin request")
	}
	req.Header.Set("X-Duckway-Token", h.Token)
	resp, err := c.Do(req)
	if err != nil {
		return nil, ErrDownloadFailed
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: HTTP %d", ErrDownloadFailed, resp.StatusCode)
	}
	return resp, nil
}

// Install selects a name only from the configured Duckway server. Upstream
// sources and version approval are exclusively managed by the server.
func (h Host) Install(ctx context.Context, name string) (Installed, error) {
	if err := ValidateName(name); err != nil {
		return Installed{}, err
	}
	lock, err := h.lock(ctx, true)
	if err != nil {
		return Installed{}, err
	}
	defer unlock(lock)
	previous, err := h.read(name)
	if err == nil && previous.Source == SourceLocal {
		return Installed{}, ErrLocalInstall
	}
	if err != nil && !errors.Is(err, ErrNotInstalled) {
		return Installed{}, err
	}
	if _, err := h.remoteOrigin(); err != nil {
		return Installed{}, err
	}
	return h.installLocked(ctx, name, false)
}
func (h Host) Update(ctx context.Context, name string) (Installed, error) {
	if err := ValidateName(name); err != nil {
		return Installed{}, err
	}
	lock, err := h.lock(ctx, true)
	if err != nil {
		return Installed{}, err
	}
	defer unlock(lock)
	previous, err := h.read(name)
	if err != nil {
		return Installed{}, err
	}
	if previous.Source == SourceLocal {
		return Installed{}, ErrLocalInstall
	}
	origin, err := h.remoteOrigin()
	if err != nil {
		return Installed{}, err
	}
	if previous.ServerOrigin != origin {
		return Installed{}, ErrServerChanged
	}
	return h.installLocked(ctx, name, true)
}
func (h Host) installLocked(ctx context.Context, name string, update bool) (installed Installed, err error) {
	origin, err := h.remoteOrigin()
	if err != nil {
		return installed, err
	}
	if !update {
		if _, err := h.read(name); err == nil {
			return installed, fmt.Errorf("plugin already installed; use plugin update")
		} else if !errors.Is(err, ErrNotInstalled) {
			return installed, err
		}
	}
	resp, err := h.get(ctx, "/client/plugins/"+name+"/manifest")
	if err != nil {
		return installed, err
	}
	m, err := DecodeManifest(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return installed, fmt.Errorf("%w: %v", ErrVerificationFailed, err)
	}
	if m.Name != name {
		return installed, fmt.Errorf("%w: server manifest changed plugin name", ErrVerificationFailed)
	}
	if err := ValidateManagedManifest(m); err != nil {
		return installed, fmt.Errorf("%w: %v", ErrVerificationFailed, err)
	}
	goos, goarch := h.platform()
	var artifact Artifact
	for _, a := range m.Artifacts {
		if a.OS == goos && a.Arch == goarch {
			artifact = a
			break
		}
	}
	if artifact.URL == "" {
		return installed, fmt.Errorf("manifest has no artifact for %s/%s", goos, goarch)
	}
	resp, err = h.get(ctx, artifact.URL)
	if err != nil {
		return installed, err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && resp.ContentLength != artifact.Size {
		return installed, fmt.Errorf("%w: artifact size mismatch", ErrVerificationFailed)
	}
	return h.activate(ctx, m, artifact, SourceManaged, origin, resp.Body)
}

func (h Host) activate(ctx context.Context, m Manifest, artifact Artifact, source, origin string, content io.Reader) (installed Installed, err error) {
	if err := ctx.Err(); err != nil {
		return installed, err
	}
	pluginDir := filepath.Join(h.root(), m.Name)
	for _, dir := range []string{pluginDir, filepath.Join(pluginDir, "releases"), filepath.Join(pluginDir, "data")} {
		if err := privateDir(dir); err != nil {
			return installed, err
		}
	}
	stage, err := os.MkdirTemp(filepath.Join(pluginDir, "releases"), "release-")
	if err != nil {
		return installed, err
	}
	activated := false
	defer func() {
		if !activated {
			_ = os.RemoveAll(stage)
		}
	}()
	executable := filepath.Join(stage, "executable")
	f, err := os.OpenFile(executable, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return installed, err
	}
	digest := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, digest), io.LimitReader(content, artifact.Size+1))
	if copyErr == nil && (n != artifact.Size || hex.EncodeToString(digest.Sum(nil)) != artifact.SHA256) {
		copyErr = fmt.Errorf("%w: artifact checksum or size mismatch", ErrVerificationFailed)
	}
	if copyErr == nil {
		copyErr = f.Chmod(0700)
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		return installed, copyErr
	}
	if closeErr != nil {
		return installed, closeErr
	}
	if err := syncDir(stage); err != nil {
		return installed, err
	}
	if err := syncDir(filepath.Dir(stage)); err != nil {
		return installed, err
	}
	installed = Installed{Manifest: m, Source: source, ServerOrigin: origin, Release: filepath.Base(stage), Artifact: artifact}
	data, err := json.Marshal(installed)
	if err != nil {
		return Installed{}, err
	}
	// Atomic metadata replacement is the sole activation point. The executable is
	// already complete in its private immutable release directory on this filesystem.
	if err := atomicWrite(pluginDir, "current.json", data); err != nil {
		return Installed{}, err
	}
	activated = true
	// Old release files may still be executing; Unix permits unlinking them. Keep
	// the current data directory untouched and reclaim only our release directory.
	entries, _ := os.ReadDir(filepath.Join(pluginDir, "releases"))
	for _, entry := range entries {
		if entry.Name() != installed.Release && strings.HasPrefix(entry.Name(), "release-") && entry.IsDir() {
			_ = os.RemoveAll(filepath.Join(pluginDir, "releases", entry.Name()))
		}
	}
	return installed, nil
}

// InstallLocal verifies and activates one explicit local package without reading
// Duckway config or making a network request. A previous local release may be
// replaced atomically; managed/local migration is never implicit.
func (h Host) InstallLocal(ctx context.Context, packageDir string) (Installed, error) {
	if err := ctx.Err(); err != nil {
		return Installed{}, err
	}
	root, err := openLocalPackageRoot(packageDir)
	if err != nil {
		return Installed{}, err
	}
	defer root.Close()
	manifestFile, err := openLocalPackageFile(root, "manifest.json", maxManifestSize)
	if err != nil {
		return Installed{}, fmt.Errorf("open local plugin manifest: %w", err)
	}
	m, decodeErr := DecodeManifest(manifestFile)
	closeErr := manifestFile.Close()
	if decodeErr != nil {
		return Installed{}, fmt.Errorf("invalid local plugin manifest: %w", decodeErr)
	}
	if closeErr != nil {
		return Installed{}, closeErr
	}
	goos, goarch := h.platform()
	var artifact Artifact
	for _, candidate := range m.Artifacts {
		if candidate.OS == goos && candidate.Arch == goarch {
			artifact = candidate
			break
		}
	}
	if artifact.URL == "" {
		return Installed{}, fmt.Errorf("local package has no artifact for %s/%s", goos, goarch)
	}
	relative, err := localArtifactRelativePath(artifact.URL)
	if err != nil {
		return Installed{}, err
	}
	content, err := openLocalPackageFile(root, relative, artifact.Size)
	if err != nil {
		return Installed{}, fmt.Errorf("open local plugin artifact: %w", err)
	}
	defer content.Close()
	lock, err := h.lock(ctx, true)
	if err != nil {
		return Installed{}, err
	}
	defer unlock(lock)
	previous, err := h.read(m.Name)
	if err == nil && previous.Source != SourceLocal {
		return Installed{}, fmt.Errorf("plugin is server-managed; local installation cannot replace it")
	}
	if err != nil && !errors.Is(err, ErrNotInstalled) {
		return Installed{}, err
	}
	return h.activate(ctx, m, artifact, SourceLocal, "", content)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func atomicWrite(dir, name string, data []byte) error {
	target := filepath.Join(dir, name)
	if err := regularFile(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(dir, ".activate-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return err
	}
	// A failed directory fsync cannot safely roll back an already atomic rename.
	// Successful rename is activation; best-effort directory sync improves durability.
	_ = syncDir(dir)
	return nil
}

func (h Host) read(name string) (Installed, error) {
	var installed Installed
	if err := ValidateName(name); err != nil {
		return installed, err
	}
	dir := filepath.Join(h.root(), name)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return installed, ErrNotInstalled
	} else if err != nil {
		return installed, err
	}
	for _, d := range []string{dir, filepath.Join(dir, "releases"), filepath.Join(dir, "data")} {
		if err := privateDir(d); err != nil {
			return installed, err
		}
	}
	path := filepath.Join(dir, "current.json")
	if err := regularFile(path); os.IsNotExist(err) {
		return installed, ErrNotInstalled
	} else if err != nil {
		return installed, err
	}
	f, err := os.Open(path)
	if err != nil {
		return installed, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, maxManifestSize+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&installed); err != nil {
		return installed, fmt.Errorf("invalid installed plugin metadata")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return installed, fmt.Errorf("invalid installed plugin metadata")
	}
	if installed.Source == "" {
		// Records created before provenance was stored are server-managed.
		installed.Source = SourceManaged
	}
	switch installed.Source {
	case SourceManaged:
		if err := ValidateManagedManifest(installed.Manifest); err != nil {
			return installed, err
		}
		if _, err := ServerOrigin(installed.ServerOrigin); err != nil {
			return installed, err
		}
	case SourceLocal:
		if err := installed.Manifest.Validate(); err != nil {
			return installed, err
		}
		if installed.ServerOrigin != "" {
			return installed, fmt.Errorf("local plugin record unexpectedly names a server origin")
		}
	default:
		return installed, fmt.Errorf("invalid plugin installation source")
	}
	if installed.Manifest.Name != name || !strings.HasPrefix(installed.Release, "release-") || filepath.Base(installed.Release) != installed.Release || strings.ContainsAny(installed.Release, "/\\") {
		return installed, fmt.Errorf("invalid installed plugin path")
	}
	goos, goarch := h.platform()
	matched := false
	for _, a := range installed.Manifest.Artifacts {
		if a == installed.Artifact && a.OS == goos && a.Arch == goarch {
			matched = true
		}
	}
	if !matched {
		return installed, fmt.Errorf("invalid installed plugin artifact")
	}
	if err := privateDir(filepath.Join(dir, "releases", installed.Release)); err != nil {
		return installed, err
	}
	return installed, nil
}

func (h Host) List() ([]Installed, error) {
	lock, err := h.lock(context.Background(), false)
	if err != nil {
		return nil, err
	}
	defer unlock(lock)
	entries, err := os.ReadDir(h.root())
	if err != nil {
		return nil, err
	}
	var result []Installed
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := ValidateName(e.Name()); err != nil {
			return nil, err
		}
		p, err := h.read(e.Name())
		if errors.Is(err, ErrNotInstalled) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Manifest.Name < result[j].Manifest.Name })
	return result, nil
}
