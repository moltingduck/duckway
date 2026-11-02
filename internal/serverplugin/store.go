package serverplugin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/clientplugin"
	"golang.org/x/sys/unix"
)

const maxStateSize = 16 << 20
const maxTotalSize int64 = 1 << 30
const maxArtifacts = 32
const maxPlugins = 256

var ErrNotFound = errors.New("plugin is unavailable")
var errStorage = errors.New("plugin storage is unavailable")

type Entry struct {
	Revision              uint64                           `json:"revision,omitempty"`
	Name                  string                           `json:"name"`
	Source                string                           `json:"manifest_url"`
	Enabled               bool                             `json:"enabled"`
	Current               string                           `json:"version"`
	Releases              map[string]clientplugin.Manifest `json:"releases"`
	Latest                *clientplugin.Manifest           `json:"latest,omitempty"`
	LastCheckedAt         string                           `json:"last_checked_at,omitempty"`
	LastSuccessfulCheckAt string                           `json:"last_successful_check_at,omitempty"`
	NextCheckAt           string                           `json:"next_check_at,omitempty"`
	CheckError            string                           `json:"check_error,omitempty"`
}
type catalog struct {
	Plugins map[string]Entry `json:"plugins"`
}

// Store has no process-local catalog: split gateway/admin processes share DataDir/plugins.
type Store struct {
	dir      string
	policy   *NetworkPolicy
	interval time.Duration
}

func New(dataDir, origins string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("plugin data directory is required")
	}
	p, err := NewNetworkPolicy(origins)
	if err != nil {
		return nil, err
	}
	interval, err := checkInterval(os.Getenv("DUCKWAY_PLUGIN_CHECK_INTERVAL"))
	if err != nil {
		return nil, err
	}
	return &Store{dir: filepath.Join(dataDir, "plugins"), policy: p, interval: interval}, nil
}

func trustedOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid()))
}
func serviceOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// Reject unsafe or symlink ancestors before opening a rooted directory.
// All later operations remain within the private directory owned by the service.
func privateRoot(dir string) (*os.Root, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, errStorage
	}
	current := string(filepath.Separator)
	top, err := os.Lstat(current)
	if err != nil || !safeAncestor(top) {
		return nil, errStorage
	}
	for _, component := range strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err = os.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
				return nil, errStorage
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !safeAncestor(info) {
			return nil, errStorage
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode().Perm()&0077 != 0 || !serviceOwner(info) {
		return nil, errStorage
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, errStorage
	}
	if validateRoot(root, info) != nil {
		root.Close()
		return nil, errStorage
	}
	return root, nil
}
func safeAncestor(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && trustedOwner(info) && (info.Mode().Perm()&0022 == 0 || info.Mode()&os.ModeSticky != 0)
}
func validateRoot(root *os.Root, expected os.FileInfo) error {
	f, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errStorage
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.IsDir() || !serviceOwner(actual) || actual.Mode().Perm()&0077 != 0 || !os.SameFile(expected, actual) {
		return errStorage
	}
	return nil
}
func (s *Store) locked(ctx context.Context, exclusive bool) (*os.Root, func(), error) {
	return s.lockedFile(ctx, ".lock", exclusive)
}
func (s *Store) lockedFile(ctx context.Context, name string, exclusive bool) (*os.Root, func(), error) {
	root, err := privateRoot(s.dir)
	if err != nil {
		return nil, nil, err
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		root.Close()
		return nil, nil, errStorage
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !serviceOwner(info) {
		file.Close()
		root.Close()
		return nil, nil, errStorage
	}
	op := unix.LOCK_SH
	if exclusive {
		op = unix.LOCK_EX
	}
	for {
		err = unix.Flock(int(file.Fd()), op|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			file.Close()
			root.Close()
			return nil, nil, errStorage
		}
		select {
		case <-ctx.Done():
			file.Close()
			root.Close()
			return nil, nil, errStorage
		case <-time.After(25 * time.Millisecond):
		}
	}
	return root, func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close(); _ = root.Close() }, nil
}
func safeParents(root *os.Root, name string) error {
	current := ""
	for _, component := range strings.Split(path.Dir(name), "/") {
		if component == "." {
			continue
		}
		current = path.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return errStorage
		}
	}
	return nil
}
func syncDirectory(root *os.Root, name string) error {
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errStorage
	}
	defer f.Close()
	if f.Sync() != nil {
		return errStorage
	}
	return nil
}
func regularOpen(root *os.Root, name string) (*os.File, error) {
	if safeParents(root, name) != nil {
		return nil, errStorage
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errStorage
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !serviceOwner(info) {
		f.Close()
		return nil, errStorage
	}
	return f, nil
}
func readCatalog(root *os.Root) (catalog, error) {
	c := catalog{Plugins: make(map[string]Entry)}
	if _, err := root.Lstat("catalog.json"); os.IsNotExist(err) {
		return c, nil
	}
	f, err := regularOpen(root, "catalog.json")
	if err != nil {
		return c, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStateSize+1))
	if err != nil || len(b) > maxStateSize {
		return c, errStorage
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Plugins == nil {
		return c, errStorage
	}
	for name, e := range c.Plugins {
		if clientplugin.ValidateName(name) != nil || name != e.Name || e.Releases == nil {
			return c, errStorage
		}
		if _, ok := e.Releases[e.Current]; !ok && (e.Current != "" || e.Enabled) {
			return c, errStorage
		}
		if e.Latest != nil && (e.Latest.Validate() != nil || e.Latest.Name != name) {
			return c, errStorage
		}
		for version, m := range e.Releases {
			if m.Validate() != nil || m.Name != name || version != m.Version {
				return c, errStorage
			}
		}
	}
	return c, nil
}
func randomName() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
func saveCatalog(root *os.Root, c catalog) error {
	b, err := json.Marshal(c)
	if err != nil || len(b) > maxStateSize {
		return errStorage
	}
	name := ".catalog-" + randomName()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errStorage
	}
	defer root.Remove(name)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errStorage
	}
	if root.Rename(name, "catalog.json") != nil {
		return errStorage
	}
	dir, err := root.Open(".")
	if err != nil {
		return errStorage
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errStorage
	}
	return nil
}
func mkdir(root *os.Root, dir string) error {
	if err := root.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return errStorage
	}
	info, err := root.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errStorage
	}
	return nil
}
func rewritten(m clientplugin.Manifest) clientplugin.Manifest {
	m.Artifacts = append([]clientplugin.Artifact(nil), m.Artifacts...)
	for i := range m.Artifacts {
		a := &m.Artifacts[i]
		a.URL = "/client/plugins/" + m.Name + "/releases/" + m.Version + "/" + a.OS + "/" + a.Arch
	}
	return m
}
func (s *Store) List(ctx context.Context, admin bool) ([]Entry, error) {
	root, done, err := s.locked(ctx, false)
	if err != nil {
		return nil, err
	}
	defer done()
	c, err := readCatalog(root)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(c.Plugins))
	for _, e := range c.Plugins {
		if admin || e.Enabled {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (s *Store) Catalog(ctx context.Context) ([]clientplugin.Manifest, error) {
	entries, err := s.List(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]clientplugin.Manifest, 0, len(entries))
	for _, e := range entries {
		out = append(out, rewritten(e.Releases[e.Current]))
	}
	return out, nil
}
func (s *Store) Manifest(ctx context.Context, name string) (clientplugin.Manifest, error) {
	list, err := s.Catalog(ctx)
	if err != nil {
		return clientplugin.Manifest{}, err
	}
	for _, m := range list {
		if m.Name == name {
			return m, nil
		}
	}
	return clientplugin.Manifest{}, ErrNotFound
}
func (s *Store) SetEnabled(ctx context.Context, name string, enabled bool) error {
	if clientplugin.ValidateName(name) != nil {
		return ErrNotFound
	}
	root, done, err := s.locked(ctx, true)
	if err != nil {
		return err
	}
	defer done()
	c, err := readCatalog(root)
	if err != nil {
		return err
	}
	e, ok := c.Plugins[name]
	if !ok {
		return ErrNotFound
	}
	if enabled && e.Current == "" {
		return errors.New("publish a verified release before enabling")
	}
	e.Enabled = enabled
	e.Revision++
	c.Plugins[name] = e
	return saveCatalog(root, c)
}
func (s *Store) Artifact(ctx context.Context, name, version, goos, arch string) (*os.File, int64, error) {
	if clientplugin.ValidateName(name) != nil {
		return nil, 0, ErrNotFound
	}
	root, done, err := s.locked(ctx, false)
	if err != nil {
		return nil, 0, err
	}
	defer done()
	c, err := readCatalog(root)
	if err != nil {
		return nil, 0, err
	}
	e, ok := c.Plugins[name]
	if !ok || !e.Enabled {
		return nil, 0, ErrNotFound
	}
	m, ok := e.Releases[version]
	if !ok {
		return nil, 0, ErrNotFound
	}
	for _, a := range m.Artifacts {
		if a.OS == goos && a.Arch == arch {
			f, err := regularOpen(root, path.Join("releases", name, version, goos+"-"+arch))
			if err != nil {
				return nil, 0, err
			}
			info, err := f.Stat()
			if err != nil || info.Size() != a.Size {
				f.Close()
				return nil, 0, errStorage
			}
			return f, a.Size, nil
		}
	}
	return nil, 0, ErrNotFound
}
func fetch(ctx context.Context, client *http.Client, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errNetwork
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errNetwork
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "" {
		resp.Body.Close()
		return nil, errNetwork
	}
	return resp, nil
}

// stageRelease writes verified bytes into an unpublished private directory.
// The caller owns a per-plugin lock; no global catalog lock is held during HTTP.
func stageRelease(ctx context.Context, root *os.Root, m clientplugin.Manifest, source *url.URL, client *http.Client) (string, error) {
	stage := ".import-" + randomName()
	if mkdir(root, stage) != nil {
		return "", errStorage
	}
	complete := false
	defer func() {
		if !complete {
			_ = root.RemoveAll(stage)
		}
	}()
	for _, a := range m.Artifacts {
		u, err := source.Parse(a.URL)
		if err != nil {
			return "", errNetwork
		}
		response, err := fetch(ctx, client, u.String())
		if err != nil {
			return "", err
		}
		if response.ContentLength >= 0 && response.ContentLength != a.Size {
			response.Body.Close()
			return "", errors.New("plugin artifact size mismatch")
		}
		f, err := root.OpenFile(path.Join(stage, a.OS+"-"+a.Arch), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			response.Body.Close()
			return "", errStorage
		}
		digest := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(f, digest), io.LimitReader(response.Body, a.Size+1))
		response.Body.Close()
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil || n != a.Size || hex.EncodeToString(digest.Sum(nil)) != a.SHA256 {
			return "", errors.New("plugin artifact verification failed")
		}
		if syncErr != nil || closeErr != nil {
			return "", errStorage
		}
	}
	if syncDirectory(root, stage) != nil {
		return "", errStorage
	}
	complete = true
	return stage, nil
}
func installStage(root *os.Root, name, version, stage string) error {
	if mkdir(root, "releases") != nil || mkdir(root, path.Join("releases", name)) != nil || syncDirectory(root, "releases") != nil {
		return errStorage
	}
	target := path.Join("releases", name, version)
	// Only unpublished orphan directories can be replaced; published releases are skipped by the caller.
	if _, err := root.Lstat(target); err == nil {
		if root.RemoveAll(target) != nil {
			return errStorage
		}
	} else if !os.IsNotExist(err) {
		return errStorage
	}
	if root.Rename(stage, target) != nil || syncDirectory(root, path.Join("releases", name)) != nil {
		return errStorage
	}
	return nil
}

// AdminSummary intentionally omits historical manifest details from list responses.
func artifactSummary(m clientplugin.Manifest) []map[string]any {
	artifacts := make([]map[string]any, 0, len(m.Artifacts))
	for _, a := range m.Artifacts {
		artifacts = append(artifacts, map[string]any{"os": a.OS, "arch": a.Arch, "size": a.Size, "sha256": a.SHA256})
	}
	return artifacts
}
func (e Entry) AdminSummary() map[string]any {
	m := e.Releases[e.Current]
	availableVersion := ""
	available := clientplugin.Manifest{}
	if e.Latest != nil && e.Latest.Version != e.Current {
		available = *e.Latest
		availableVersion = available.Version
	}
	return map[string]any{"name": e.Name, "manifest_url": e.Source, "enabled": e.Enabled, "version": e.Current, "releases": len(e.Releases), "description": m.Description, "artifacts": artifactSummary(m), "available_version": availableVersion, "available_manifest_digest": manifestDigest(e.Latest), "available_description": available.Description, "available_artifacts": artifactSummary(available), "last_checked_at": e.LastCheckedAt, "last_successful_check_at": e.LastSuccessfulCheckAt, "next_check_at": e.NextCheckAt, "check_error": e.CheckError}
}
