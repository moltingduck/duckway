package ducklord

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileEndpoint identifies a directory on the local machine or a configured host.
type FileEndpoint struct {
	Client *Client
	Path   string
}
type FileEntry struct {
	Name            string `json:"name"`
	IsDir           bool   `json:"is_dir"`
	Size            int64  `json:"size"`
	NonTransferable bool   `json:"non_transferable,omitempty"`
}
type FileCopyRequest struct {
	Source, Destination FileEndpoint
	Names               []string
	Conflict            string
	Progress            func(FileCopyProgress)
}

// FileCopyProgress describes one ordered per-item file exchange transition.
// Completed counts items committed or skipped; it never counts failed or
// not-yet-started items.
type FileCopyProgress struct {
	Name        string
	Destination string
	State       string
	Completed   int
	Total       int
}
type FileCopyResult struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Skipped     bool   `json:"skipped,omitempty"`
}

const exchangeLimit = int64(1 << 30)
const exchangeMaxEntries = 10000
const exchangeMaxPathComponents = 64
const exchangeTimeout = 30 * time.Minute
const exchangePayloadRoot = "__duckway_payload"
const exchangeFooter = "__duckway_complete__"

func validateEndpoint(e FileEndpoint) error {
	if !filepath.IsAbs(e.Path) {
		return fmt.Errorf("file endpoint path must be absolute")
	}
	if filepath.Clean(e.Path) != e.Path {
		return fmt.Errorf("file endpoint path must be clean")
	}
	return nil
}

func ListFiles(ctx context.Context, endpoint FileEndpoint) ([]FileEntry, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if endpoint.Client == nil {
		if err := noSymlinkPath(endpoint.Path); err != nil {
			return nil, err
		}
		r, err := os.OpenRoot(endpoint.Path)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		dir, err := r.Open(".")
		if err != nil {
			return nil, err
		}
		defer dir.Close()
		out := make([]FileEntry, 0)
		for {
			es, readErr := dir.ReadDir(256)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return nil, readErr
			}
			if len(es) == 0 {
				break
			}
			for _, e := range es {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
				}
				i, err := e.Info()
				if err != nil {
					return nil, err
				}
				nonTransferable := !e.Type().IsRegular() && !e.IsDir()
				out = append(out, FileEntry{Name: e.Name(), IsDir: e.IsDir(), Size: i.Size(), NonTransferable: nonTransferable})
				if len(out) > 100000 {
					return nil, fmt.Errorf("directory listing exceeds entry limit")
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}
	var out []FileEntry
	err := remoteRun(ctx, *endpoint.Client, endpoint.Client.DucklionArgs("files", "list", endpoint.Path), nil, func(r io.Reader) error { return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&out) })
	if err == nil && len(out) > 100000 {
		return nil, fmt.Errorf("directory listing exceeds entry limit")
	}
	return out, err
}

func CopyFiles(parent context.Context, req FileCopyRequest) ([]FileCopyResult, error) {
	// Keep the request stable even if the caller reuses or changes its Names
	// slice while progress callbacks are running.
	req.Names = append([]string(nil), req.Names...)
	if err := validateEndpoint(req.Source); err != nil {
		return nil, err
	}
	if err := validateEndpoint(req.Destination); err != nil {
		return nil, err
	}
	if req.Conflict == "" {
		req.Conflict = "skip"
	}
	if req.Conflict != "skip" && req.Conflict != "rename" && req.Conflict != "overwrite" {
		return nil, fmt.Errorf("unknown conflict policy %q", req.Conflict)
	}
	ctx, cancel := context.WithTimeout(parent, exchangeTimeout)
	defer cancel()
	if len(req.Names) == 0 {
		return nil, fmt.Errorf("at least one file is required")
	}
	seen := map[string]bool{}
	for _, n := range req.Names {
		if !safeEntryName(n) || seen[n] {
			return nil, fmt.Errorf("invalid or duplicate file name %q", n)
		}
		seen[n] = true
	}
	if req.Source.Client == nil && req.Destination.Client == nil {
		return copyLocal(ctx, req)
	}
	results := make([]FileCopyResult, 0, len(req.Names))
	var remoteSourceEntries map[string]FileEntry
	if req.Source.Client != nil {
		entries, err := ListFiles(ctx, req.Source)
		if err != nil {
			return nil, err
		}
		remoteSourceEntries = make(map[string]FileEntry, len(entries))
		for _, entry := range entries {
			remoteSourceEntries[entry.Name] = entry
		}
		for _, name := range req.Names {
			entry, ok := remoteSourceEntries[name]
			if !ok {
				return nil, fmt.Errorf("source does not exist %q", name)
			}
			if entry.NonTransferable {
				return nil, fmt.Errorf("unsupported source %q", name)
			}
		}
	}
	for _, name := range req.Names {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
		emitFileCopyProgress(req, name, "", "copying", len(results))
		if sameClient(req.Source.Client, req.Destination.Client) {
			candidate := filepath.Join(req.Source.Path, name)
			if rel, e := filepath.Rel(candidate, req.Destination.Path); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				if (remoteSourceEntries != nil && remoteSourceEntries[name].IsDir) || (remoteSourceEntries == nil && func() bool { si, se := os.Stat(candidate); return se == nil && si.IsDir() }()) {
					return results, fmt.Errorf("destination is inside source %q", name)
				}
			}
		}
		destName, skip, err := chooseRemoteDestination(ctx, req.Destination, name, req.Conflict)
		if err != nil {
			return results, err
		}
		if skip {
			results = append(results, FileCopyResult{Name: name, Destination: filepath.Join(req.Destination.Path, name), Skipped: true})
			emitFileCopyProgress(req, name, filepath.Join(req.Destination.Path, name), "skipped", len(results))
			continue
		}
		if req.Source.Client != nil && req.Destination.Client != nil {
			if err = remotePipe(ctx, *req.Source.Client, filepath.Join(req.Source.Path, name), *req.Destination.Client, req.Destination.Path, destName, req.Conflict == "overwrite"); err != nil {
				return results, err
			}
		} else if req.Source.Client != nil {
			err = remoteToLocal(ctx, *req.Source.Client, filepath.Join(req.Source.Path, name), req.Destination.Path, destName, req.Conflict == "overwrite")
		} else {
			err = localToRemote(ctx, filepath.Join(req.Source.Path, name), *req.Destination.Client, req.Destination.Path, destName, req.Conflict == "overwrite")
		}
		if err != nil {
			return results, err
		}
		results = append(results, FileCopyResult{Name: name, Destination: filepath.Join(req.Destination.Path, destName)})
		emitFileCopyProgress(req, name, filepath.Join(req.Destination.Path, destName), "copied", len(results))
	}
	return results, nil
}

func emitFileCopyProgress(req FileCopyRequest, name, destination, state string, completed int) {
	if req.Progress != nil {
		req.Progress(FileCopyProgress{Name: name, Destination: destination, State: state, Completed: completed, Total: len(req.Names)})
	}
}

func sameClient(a, b *Client) bool {
	return a != nil && b != nil && (a == b || a.Name == b.Name && a.Host == b.Host && a.User == b.User && a.SSH == b.SSH && a.Ducklion == b.Ducklion)
}

func safeEntryName(n string) bool {
	return n != "" && n != "." && n != ".." && filepath.Base(n) == n && !strings.ContainsAny(n, "/\\")
}
func ProjectExchangePath(configPath, projectID string) (string, error) {
	if strings.TrimSpace(projectID) == "" {
		return "", fmt.Errorf("project id is required")
	}
	if configPath == "" {
		return "", fmt.Errorf("config path is required")
	}
	h := sha256.Sum256([]byte(projectID))
	root := filepath.Join(filepath.Dir(configPath), "exchange", hex.EncodeToString(h[:]))
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	return root, nil
}

func copyLocal(ctx context.Context, req FileCopyRequest) ([]FileCopyResult, error) {
	return copyLocalWithStageRemover(ctx, req, func(root *os.Root, stage string) error {
		return root.RemoveAll(stage)
	})
}

// copyLocalWithStageRemover keeps cleanup failure handling testable without
// relying on filesystem permission or timing races.
func copyLocalWithStageRemover(ctx context.Context, req FileCopyRequest, removeStage func(*os.Root, string) error) ([]FileCopyResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if err := ensureRealDir(req.Destination.Path); err != nil {
		return nil, err
	}
	if err := noSymlinkPath(req.Source.Path); err != nil {
		return nil, err
	}
	if err := noSymlinkPath(req.Destination.Path); err != nil {
		return nil, err
	}
	srcRoot, err := os.OpenRoot(req.Source.Path)
	if err != nil {
		return nil, err
	}
	defer srcRoot.Close()
	dstRoot, err := os.OpenRoot(req.Destination.Path)
	if err != nil {
		return nil, err
	}
	defer dstRoot.Close()
	res := make([]FileCopyResult, 0, len(req.Names))
	for _, n := range req.Names {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		emitFileCopyProgress(req, n, "", "copying", len(res))
		if rel, e := filepath.Rel(filepath.Join(req.Source.Path, n), req.Destination.Path); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if si, se := srcRoot.Lstat(n); se == nil && si.IsDir() {
				return res, fmt.Errorf("destination is inside source %q", n)
			}
		}
		i, err := srcRoot.Lstat(n)
		if err != nil {
			return res, err
		}
		if i.Mode()&os.ModeSymlink != 0 || !i.Mode().IsRegular() && !i.IsDir() {
			return res, fmt.Errorf("unsupported source %q", n)
		}
		dn, skip, err := chooseLocalDestination(ctx, dstRoot, n, req.Conflict)
		if err != nil {
			return res, err
		}
		if req.Conflict == "overwrite" {
			if old, e := dstRoot.Lstat(dn); e == nil && old.IsDir() != i.IsDir() {
				return res, fmt.Errorf("refusing overwrite of different destination type")
			} else if e != nil && !os.IsNotExist(e) {
				return res, e
			}
		}
		if skip {
			res = append(res, FileCopyResult{Name: n, Destination: filepath.Join(req.Destination.Path, n), Skipped: true})
			emitFileCopyProgress(req, n, filepath.Join(req.Destination.Path, n), "skipped", len(res))
			continue
		}
		stage, err := makeExchangeStage(dstRoot)
		if err != nil {
			return res, err
		}
		stageRel := filepath.Join(stage, dn)
		defer func() {
			// Best-effort fallback; the explicit cleanup below reports failures.
			_ = removeStage(dstRoot, stage)
		}()
		bytes := int64(0)
		if err = copyTreeRoot(ctx, srcRoot, dstRoot, n, stageRel, &bytes); err != nil {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		if !i.IsDir() && req.Conflict != "overwrite" {
			err = dstRoot.Link(stageRel, dn)
			if err == nil {
				_ = dstRoot.Remove(stageRel)
			}
		} else if req.Conflict == "overwrite" {
			err = dstRoot.Rename(stageRel, dn)
		} else {
			dir, openErr := dstRoot.Open(".")
			if openErr != nil {
				return res, openErr
			}
			err = renameNoReplaceAt(dir, stageRel, dn)
			_ = dir.Close()
		}
		if err != nil {
			return res, err
		}
		destination := filepath.Join(req.Destination.Path, dn)
		if err = removeStage(dstRoot, stage); err != nil {
			// The destination is already committed. Preserve its successful item
			// outcome while reporting the staging cleanup failure at batch level.
			res = append(res, FileCopyResult{Name: n, Destination: destination})
			emitFileCopyProgress(req, n, destination, "copied", len(res))
			return res, fmt.Errorf("copy committed to %s but staging cleanup failed: %w", destination, err)
		}
		res = append(res, FileCopyResult{Name: n, Destination: destination})
		emitFileCopyProgress(req, n, destination, "copied", len(res))
	}
	return res, nil
}

func makeExchangeStage(root *os.Root) (string, error) {
	var b [12]byte
	for i := 0; i < 100; i++ {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		n := ".exchange-" + hex.EncodeToString(b[:])
		if err := root.Mkdir(n, 0700); err == nil {
			return n, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not create exchange staging directory")
}

func copyTreeRoot(ctx context.Context, srcRoot, dstRoot *os.Root, src, dst string, bytes *int64) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	entry, err := srcRoot.Lstat(src)
	if err != nil {
		return err
	}
	if entry.Mode()&os.ModeSymlink != 0 || !entry.Mode().IsRegular() && !entry.IsDir() {
		return fmt.Errorf("unsupported source %q", src)
	}
	f, err := srcRoot.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("unsupported source %q", src)
	}
	if info.IsDir() {
		if err = dstRoot.Mkdir(dst, 0700); err != nil {
			return err
		}
		for {
			entries, readErr := f.ReadDir(256)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
			for _, entry := range entries {
				if err = copyTreeRoot(ctx, srcRoot, dstRoot, filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), bytes); err != nil {
					return err
				}
			}
		}
		return nil
	}
	out, err := dstRoot.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|info.Mode().Perm()&0100)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(f, exchangeLimit+1))
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && (n > exchangeLimit || *bytes > exchangeLimit-n) {
		err = fmt.Errorf("file exceeds transfer limit")
	}
	*bytes += n
	if err == nil {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		default:
		}
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = dstRoot.Remove(dst)
	}
	return err
}
func ensureRealDir(p string) error {
	i, e := os.Lstat(p)
	if e != nil {
		return e
	}
	if !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("destination is not a real directory")
	}
	return nil
}
func noSymlinkPath(p string) error {
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(p), string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		i, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains symlink: %s", cur)
		}
	}
	return nil
}
func chooseLocalDestination(ctx context.Context, r *os.Root, n, policy string) (string, bool, error) {
	i, e := r.Lstat(n)
	if os.IsNotExist(e) {
		return n, false, nil
	}
	if e != nil {
		return "", false, e
	}
	if policy == "skip" {
		return n, true, nil
	}
	if policy == "overwrite" {
		if i.IsDir() {
			return "", false, fmt.Errorf("refusing non-atomic directory overwrite")
		}
		if !i.Mode().IsRegular() {
			return "", false, fmt.Errorf("refusing overwrite of non-regular destination")
		}
		return n, false, nil
	}
	for x := 1; x <= 100000; x++ {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		default:
		}
		d := fmt.Sprintf("%s (%d)", n, x)
		if _, e = r.Lstat(d); os.IsNotExist(e) {
			return d, false, nil
		} else if e != nil {
			return "", false, e
		}
	}
	return "", false, fmt.Errorf("destination rename exceeds conflict limit")
}
func chooseRemoteDestination(ctx context.Context, e FileEndpoint, n, policy string) (string, bool, error) {
	es, err := ListFiles(ctx, FileEndpoint{Client: e.Client, Path: e.Path})
	if err != nil {
		return "", false, err
	}
	occupied := make(map[string]FileEntry, len(es))
	for _, entry := range es {
		occupied[entry.Name] = entry
	}
	entry, found := occupied[n]
	if !found {
		return n, false, nil
	}
	if policy == "skip" {
		return n, true, nil
	}
	if policy == "overwrite" {
		if entry.NonTransferable {
			return "", false, fmt.Errorf("refusing overwrite of non-transferable destination")
		}
		if entry.IsDir {
			return "", false, fmt.Errorf("refusing non-atomic directory overwrite")
		}
		return n, false, nil
	}
	for i := 1; i <= 100000; i++ {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		default:
		}
		d := fmt.Sprintf("%s (%d)", n, i)
		if _, found := occupied[d]; !found {
			return d, false, nil
		}
	}
	return "", false, fmt.Errorf("destination rename exceeds conflict limit")
}

func remoteRun(ctx context.Context, c Client, args []string, in io.Reader, consume func(io.Reader) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := c.SSHCommandParts()
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], SSHArgs(c, false, args...)...)...)
	cmd.Stdin = in
	errPipe := &limitedBuffer{limit: 4096}
	cmd.Stderr = errPipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() {
		_ = stdout.Close()
		if x, ok := in.(io.Closer); ok {
			_ = x.Close()
		}
	})
	defer stop()
	ce := consume(stdout)
	if ce != nil {
		_ = stdout.Close()
		if x, ok := in.(io.Closer); ok {
			_ = x.Close()
		}
		cancel()
	}
	we := cmd.Wait()
	if ce != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ce
	}
	if we != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("remote files command on %s: %s", c.Name, strings.TrimSpace(errPipe.String()))
	}
	return nil
}

type limitedBuffer struct {
	b     strings.Builder
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.b.Len() < b.limit {
		n := b.limit - b.b.Len()
		if len(p) > n {
			p = p[:n]
		}
		b.b.WriteString(string(p))
	}
	return original, nil
}
func (b *limitedBuffer) String() string { return b.b.String() }

func tarOne(ctx context.Context, src, name string, w io.Writer) error {
	base := filepath.Dir(src)
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	tw := tar.NewWriter(w)
	bytes := int64(0)
	entries := 0
	err = tarRoot(ctx, root, filepath.Base(src), exchangePayloadRoot, tw, &bytes, &entries)
	if err != nil {
		return err
	}
	if err = checkArchiveEntry(&entries, exchangeFooter); err != nil {
		return err
	}
	if err = tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	return tw.Close()
}

func tarRoot(ctx context.Context, root *os.Root, source, name string, tw *tar.Writer, bytes *int64, entries *int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := checkArchiveEntry(entries, name); err != nil {
		return err
	}
	i, err := root.Lstat(source)
	if err != nil {
		return err
	}
	if i.Mode()&os.ModeSymlink != 0 || (!i.Mode().IsRegular() && !i.IsDir()) {
		return fmt.Errorf("unsupported source %q", source)
	}
	h := &tar.Header{Name: name, Mode: int64(i.Mode().Perm()), ModTime: time.Unix(0, 0)}
	if i.IsDir() {
		h.Typeflag = tar.TypeDir
	} else {
		h.Size = i.Size()
	}
	if err = tw.WriteHeader(h); err != nil {
		return err
	}
	f, err := root.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	actual, err := f.Stat()
	if err != nil {
		return err
	}
	if actual.Mode()&os.ModeSymlink != 0 || actual.IsDir() != i.IsDir() || (!actual.Mode().IsRegular() && !actual.IsDir()) {
		return fmt.Errorf("source changed during transfer: %q", source)
	}
	if actual.IsDir() {
		for {
			es, e := f.ReadDir(256)
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			for _, entry := range es {
				if e := tarRoot(ctx, root, filepath.Join(source, entry.Name()), filepath.ToSlash(filepath.Join(name, entry.Name())), tw, bytes, entries); e != nil {
					return e
				}
			}
		}
		return nil
	}
	n, err := io.Copy(tw, io.LimitReader(f, exchangeLimit+1))
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && (n > exchangeLimit || *bytes > exchangeLimit-n) {
		err = fmt.Errorf("file exceeds transfer limit")
	}
	*bytes += n
	return err
}
func remoteToLocal(ctx context.Context, c Client, src, dst, name string, overwrite bool) error {
	dstRoot, e := os.OpenRoot(dst)
	if e != nil {
		return e
	}
	defer dstRoot.Close()
	stage, e := makeExchangeStage(dstRoot)
	if e != nil {
		return e
	}
	defer dstRoot.RemoveAll(stage)
	if e = remoteRun(ctx, c, c.DucklionArgs("files", "read", src), nil, func(r io.Reader) error { return extractTarRoot(r, dstRoot, stage, exchangePayloadRoot) }); e != nil {
		return e
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	payload := filepath.Join(stage, exchangePayloadRoot)
	payloadInfo, err := dstRoot.Lstat(payload)
	if err != nil {
		return err
	}
	if old, err := dstRoot.Lstat(name); err == nil {
		if overwrite && old.IsDir() != payloadInfo.IsDir() {
			return fmt.Errorf("refusing overwrite of different destination type")
		}
		if !overwrite {
			return fmt.Errorf("destination already exists")
		}
		if old.IsDir() {
			return fmt.Errorf("refusing non-atomic directory overwrite")
		}
		if !old.Mode().IsRegular() {
			return fmt.Errorf("refusing overwrite of non-regular destination")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if !overwrite {
		if i, err := dstRoot.Lstat(payload); err == nil && !i.IsDir() {
			if err = dstRoot.Link(payload, name); err != nil {
				return err
			}
			return nil
		}
	}
	if overwrite {
		return dstRoot.Rename(payload, name)
	}
	dir, err := dstRoot.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return renameNoReplaceAt(dir, payload, name)
}
func localToRemote(ctx context.Context, src string, c Client, dst, name string, overwrite bool) error {
	pipeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() { err := tarOne(pipeCtx, src, filepath.Base(src), pw); _ = pw.CloseWithError(err); errc <- err }()
	args := c.DucklionArgs("files", "write", dst, name)
	if overwrite {
		args = append(args, "--overwrite")
	}
	e := remoteRun(ctx, c, args, pr, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
	if e != nil {
		cancel()
		_ = pr.CloseWithError(e)
		<-errc
		return e
	}
	return <-errc
}
func remotePipe(ctx context.Context, sc Client, src string, dc Client, dst, name string, overwrite bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	ch := make(chan error, 1)
	go func() {
		// Relay archive entries as they arrive, but hold the completion footer until
		// the source SSH command has exited successfully. This prevents a source
		// command that fails after producing a valid looking tar from committing.
		tw := tar.NewWriter(pw)
		footer := false
		bytes := int64(0)
		entries := 0
		relayErr := remoteRun(ctx, sc, sc.DucklionArgs("files", "read", src), nil, func(r io.Reader) error {
			br := bufio.NewReader(r)
			tr := tar.NewReader(br)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					if _, trailing := br.ReadByte(); trailing != io.EOF {
						return fmt.Errorf("archive has trailing data")
					}
					break
				}
				if err != nil {
					return err
				}
				if !safeTarPath(h.Name) {
					return fmt.Errorf("unsafe archive path")
				}
				if err := checkArchiveEntry(&entries, h.Name); err != nil {
					return err
				}
				if h.Name == exchangeFooter {
					if h.Typeflag != tar.TypeReg || h.Size != 0 {
						return fmt.Errorf("invalid completion footer")
					}
					if footer {
						return fmt.Errorf("duplicate completion footer")
					}
					footer = true
					continue
				}
				if footer {
					return fmt.Errorf("archive data after completion footer")
				}
				if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
					return fmt.Errorf("unsupported archive entry")
				}
				if h.Typeflag == tar.TypeReg && (h.Size < 0 || h.Size > exchangeLimit || bytes > exchangeLimit-h.Size) {
					return fmt.Errorf("archive exceeds size limit")
				}
				if err = tw.WriteHeader(h); err != nil {
					return err
				}
				if h.Typeflag == tar.TypeReg && h.Size > 0 {
					var n int64
					if n, err = io.CopyN(tw, tr, h.Size); err != nil {
						return err
					}
					bytes += n
				}
			}
			if !footer {
				return fmt.Errorf("incomplete transfer")
			}
			return nil
		})
		if relayErr != nil {
			_ = pw.CloseWithError(relayErr)
			ch <- relayErr
			return
		}
		if err := tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
			_ = pw.CloseWithError(err)
			ch <- err
			return
		}
		if err := tw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			ch <- err
			return
		}
		_ = pw.Close()
		ch <- nil
	}()
	args := dc.DucklionArgs("files", "write", dst, name)
	if overwrite {
		args = append(args, "--overwrite")
	}
	e := remoteRun(ctx, dc, args, pr, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
	if e != nil {
		_ = pr.CloseWithError(e)
		cancel()
		if relayErr := <-ch; relayErr != nil && !errors.Is(relayErr, context.Canceled) {
			return relayErr
		}
		return e
	}
	return <-ch
}
func extractTarRoot(r io.Reader, dstRoot *os.Root, stage, expectedRoot string) error {
	stageRoot, err := dstRoot.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer stageRoot.Close()
	br := bufio.NewReader(r)
	tr := tar.NewReader(br)
	root := ""
	complete := false
	bytes := int64(0)
	entries := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			if _, trailing := br.ReadByte(); trailing != io.EOF {
				return fmt.Errorf("archive has trailing data")
			}
			if !complete {
				return fmt.Errorf("incomplete transfer")
			}
			break
		}
		if e != nil {
			return e
		}
		if !safeTarPath(h.Name) {
			return fmt.Errorf("unsafe archive path")
		}
		if err := checkArchiveEntry(&entries, h.Name); err != nil {
			return err
		}
		if h.Name == exchangeFooter {
			if h.Typeflag != tar.TypeReg || h.Size != 0 {
				return fmt.Errorf("invalid completion footer")
			}
			if complete {
				return fmt.Errorf("duplicate completion footer")
			}
			complete = true
			continue
		}
		if complete {
			return fmt.Errorf("archive data after completion footer")
		}
		parts := strings.Split(filepath.ToSlash(h.Name), "/")
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root || expectedRoot != "" && root != expectedRoot {
			return fmt.Errorf("archive root mismatch")
		}
		p := filepath.FromSlash(h.Name)
		if h.Typeflag == tar.TypeDir {
			if e = stageRoot.MkdirAll(p, 0700); e != nil {
				return e
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported archive entry")
		}
		if e = stageRoot.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if h.Size < 0 || h.Size > exchangeLimit || bytes > exchangeLimit-h.Size {
			return fmt.Errorf("archive exceeds size limit")
		}
		f, e := stageRoot.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|os.FileMode(h.Mode)&0100)
		if e != nil {
			return e
		}
		n, copyErr := io.Copy(f, io.LimitReader(tr, h.Size+1))
		bytes += n
		if copyErr == nil && n != h.Size {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr != nil {
			e = copyErr
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return e
		}
	}
	if root == "" {
		return fmt.Errorf("empty archive")
	}
	return nil
}
func checkArchiveEntry(entries *int, name string) error {
	if *entries >= exchangeMaxEntries {
		return fmt.Errorf("archive exceeds entry limit")
	}
	if len(strings.Split(filepath.ToSlash(name), "/")) > exchangeMaxPathComponents {
		return fmt.Errorf("archive path exceeds component limit")
	}
	*entries++
	return nil
}

func safeTarPath(n string) bool {
	clean := filepath.Clean(n)
	return n != "" && n != "." && n != ".." && !filepath.IsAbs(n) && clean == n && !strings.HasPrefix(clean, ".."+string(filepath.Separator)) && !strings.Contains(n, "\\")
}
