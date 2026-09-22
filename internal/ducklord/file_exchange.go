package ducklord

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}
type FileCopyRequest struct {
	Source, Destination FileEndpoint
	Names               []string
	Conflict            string
}
type FileCopyResult struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Skipped     bool   `json:"skipped,omitempty"`
}

const exchangeLimit = int64(1 << 30)
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
				if e.Type()&os.ModeSymlink != 0 || !e.Type().IsRegular() && !e.IsDir() {
					continue
				}
				out = append(out, FileEntry{Name: e.Name(), IsDir: e.IsDir(), Size: i.Size()})
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
	var remoteSourceDirs map[string]bool
	if sameClient(req.Source.Client, req.Destination.Client) && req.Source.Client != nil {
		entries, err := ListFiles(ctx, req.Source)
		if err != nil {
			return nil, err
		}
		remoteSourceDirs = make(map[string]bool, len(entries))
		for _, entry := range entries {
			remoteSourceDirs[entry.Name] = entry.IsDir
		}
	}
	for _, name := range req.Names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if sameClient(req.Source.Client, req.Destination.Client) {
			candidate := filepath.Join(req.Source.Path, name)
			if rel, e := filepath.Rel(candidate, req.Destination.Path); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				if (remoteSourceDirs != nil && remoteSourceDirs[name]) || (remoteSourceDirs == nil && func() bool { si, se := os.Stat(candidate); return se == nil && si.IsDir() }()) {
					return nil, fmt.Errorf("destination is inside source %q", name)
				}
			}
		}
		destName, skip, err := chooseRemoteDestination(ctx, req.Destination, name, req.Conflict)
		if err != nil {
			return nil, err
		}
		if skip {
			results = append(results, FileCopyResult{Name: name, Destination: filepath.Join(req.Destination.Path, name), Skipped: true})
			continue
		}
		if req.Source.Client != nil && req.Destination.Client != nil {
			if err = remotePipe(ctx, *req.Source.Client, filepath.Join(req.Source.Path, name), *req.Destination.Client, req.Destination.Path, destName, req.Conflict == "overwrite"); err != nil {
				return nil, err
			}
		} else if req.Source.Client != nil {
			err = remoteToLocal(ctx, *req.Source.Client, filepath.Join(req.Source.Path, name), req.Destination.Path, destName, req.Conflict == "overwrite")
		} else {
			err = localToRemote(ctx, filepath.Join(req.Source.Path, name), *req.Destination.Client, req.Destination.Path, destName, req.Conflict == "overwrite")
		}
		if err != nil {
			return nil, err
		}
		results = append(results, FileCopyResult{Name: name, Destination: filepath.Join(req.Destination.Path, destName)})
	}
	return results, nil
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
			return nil, ctx.Err()
		default:
		}
		if rel, e := filepath.Rel(filepath.Join(req.Source.Path, n), req.Destination.Path); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if si, se := srcRoot.Lstat(n); se == nil && si.IsDir() {
				return nil, fmt.Errorf("destination is inside source %q", n)
			}
		}
		i, err := srcRoot.Lstat(n)
		if err != nil {
			return nil, err
		}
		if i.Mode()&os.ModeSymlink != 0 || !i.Mode().IsRegular() && !i.IsDir() {
			return nil, fmt.Errorf("unsupported source %q", n)
		}
		dn, skip, err := chooseLocalDestination(dstRoot, n, req.Conflict)
		if err != nil {
			return nil, err
		}
		if skip {
			res = append(res, FileCopyResult{Name: n, Destination: filepath.Join(req.Destination.Path, n), Skipped: true})
			continue
		}
		stage, err := os.MkdirTemp(req.Destination.Path, ".exchange-")
		if err != nil {
			return nil, err
		}
		stagePath := filepath.Join(stage, dn)
		stageRel, err := filepath.Rel(req.Destination.Path, stagePath)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(stage)
		if err = copyTreeRoot(ctx, srcRoot, dstRoot, n, stageRel); err != nil {
			return nil, err
		}
		destPath := filepath.Join(req.Destination.Path, dn)
		if !i.IsDir() && req.Conflict != "overwrite" {
			err = os.Link(stagePath, destPath)
			if err == nil {
				_ = os.Remove(stagePath)
			}
		} else if req.Conflict == "overwrite" {
			err = os.Rename(stagePath, destPath)
		} else {
			err = renameNoReplace(stagePath, destPath)
		}
		if err != nil {
			return nil, err
		}
		res = append(res, FileCopyResult{Name: n, Destination: filepath.Join(req.Destination.Path, dn)})
	}
	return res, nil
}

func copyTreeRoot(ctx context.Context, srcRoot, dstRoot *os.Root, src, dst string) error {
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
				if err = copyTreeRoot(ctx, srcRoot, dstRoot, filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	out, err := dstRoot.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(f, exchangeLimit+1))
	if err == nil && n > exchangeLimit {
		err = fmt.Errorf("file exceeds transfer limit")
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
func chooseLocalDestination(r *os.Root, n, policy string) (string, bool, error) {
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
	for x := 1; ; x++ {
		d := fmt.Sprintf("%s (%d)", n, x)
		if _, e = r.Lstat(d); os.IsNotExist(e) {
			return d, false, nil
		}
	}
}
func chooseRemoteDestination(ctx context.Context, e FileEndpoint, n, policy string) (string, bool, error) {
	es, err := ListFiles(ctx, FileEndpoint{Client: e.Client, Path: e.Path})
	if err != nil {
		return "", false, err
	}
	for _, x := range es {
		if x.Name == n {
			if policy == "skip" {
				return n, true, nil
			}
			if policy == "overwrite" && x.IsDir {
				return "", false, fmt.Errorf("refusing non-atomic directory overwrite")
			}
			if policy == "overwrite" {
				return n, false, nil
			}
			for i := 1; ; i++ {
				d := fmt.Sprintf("%s (%d)", n, i)
				found := false
				for _, y := range es {
					if y.Name == d {
						found = true
						break
					}
				}
				if !found {
					return d, false, nil
				}
			}
		}
	}
	return n, false, nil
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

func tarOne(src, name string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.Walk(src, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if i.Mode()&os.ModeSymlink != 0 || !i.Mode().IsRegular() && !i.IsDir() {
			return fmt.Errorf("unsupported source %q", p)
		}
		rel, _ := filepath.Rel(filepath.Dir(src), p)
		h := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(i.Mode().Perm()), Size: i.Size(), ModTime: time.Unix(0, 0)}
		if i.IsDir() {
			h.Typeflag = tar.TypeDir
		}
		if p == src {
			h.Name = exchangePayloadRoot
		} else {
			rel, _ := filepath.Rel(src, p)
			h.Name = filepath.ToSlash(filepath.Join(exchangePayloadRoot, rel))
		}
		if e = tw.WriteHeader(h); e != nil {
			return e
		}
		if i.Mode().IsRegular() {
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			n, copyErr := io.Copy(tw, io.LimitReader(f, exchangeLimit+1))
			if copyErr == nil && n > exchangeLimit {
				copyErr = fmt.Errorf("file exceeds transfer limit")
			}
			e = copyErr
			f.Close()
			return e
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	return tw.Close()
}
func remoteToLocal(ctx context.Context, c Client, src, dst, name string, overwrite bool) error {
	stage, e := os.MkdirTemp(dst, ".exchange-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	if e = remoteRun(ctx, c, c.DucklionArgs("files", "read", src), nil, func(r io.Reader) error { return extractTar(r, stage, exchangePayloadRoot) }); e != nil {
		return e
	}
	payload := filepath.Join(stage, exchangePayloadRoot)
	if old, err := os.Lstat(filepath.Join(dst, name)); err == nil {
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
		if i, err := os.Lstat(payload); err == nil && !i.IsDir() {
			if err = os.Link(payload, filepath.Join(dst, name)); err != nil {
				return err
			}
			return nil
		}
	}
	if overwrite {
		return os.Rename(payload, filepath.Join(dst, name))
	}
	return renameNoReplace(payload, filepath.Join(dst, name))
}
func localToRemote(ctx context.Context, src string, c Client, dst, name string, overwrite bool) error {
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() { err := tarOne(src, filepath.Base(src), pw); pw.CloseWithError(err); errc <- err }()
	args := c.DucklionArgs("files", "write", dst, name)
	if overwrite {
		args = append(args, "--overwrite")
	}
	e := remoteRun(ctx, c, args, pr, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
	if e != nil {
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
		relayErr := remoteRun(ctx, sc, sc.DucklionArgs("files", "read", src), nil, func(r io.Reader) error {
			tr := tar.NewReader(io.LimitReader(r, exchangeLimit+1))
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				if !safeTarPath(h.Name) {
					return fmt.Errorf("unsafe archive path")
				}
				if h.Name == exchangeFooter {
					if h.Typeflag != tar.TypeReg || h.Size != 0 {
						return fmt.Errorf("invalid completion footer")
					}
					footer = true
					continue
				}
				if footer {
					return fmt.Errorf("archive data after completion footer")
				}
				if err = tw.WriteHeader(h); err != nil {
					return err
				}
				if h.Size > 0 {
					if _, err = io.CopyN(tw, tr, h.Size); err != nil {
						return err
					}
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
		pr.CloseWithError(e)
		cancel()
		<-ch
		return e
	}
	return <-ch
}
func extractTar(r io.Reader, dst, expectedRoot string) error {
	tr := tar.NewReader(io.LimitReader(r, exchangeLimit+1))
	root := ""
	complete := false
	for {
		h, e := tr.Next()
		if e == io.EOF {
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
		if h.Name == exchangeFooter {
			if h.Typeflag != tar.TypeReg || h.Size != 0 {
				return fmt.Errorf("invalid completion footer")
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
		p := filepath.Join(dst, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, filepath.Clean(dst)+string(filepath.Separator)) {
			return fmt.Errorf("archive escapes destination")
		}
		if h.Typeflag == tar.TypeDir {
			if e = os.MkdirAll(p, 0700); e != nil {
				return e
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported archive entry")
		}
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if h.Size < 0 || h.Size > exchangeLimit {
			return fmt.Errorf("archive exceeds size limit")
		}
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		n, copyErr := io.Copy(f, io.LimitReader(tr, h.Size+1))
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
	return nil
}
func safeTarPath(n string) bool {
	return n != "" && n != "." && n != ".." && !filepath.IsAbs(n) && !strings.HasPrefix(filepath.Clean(n), ".."+string(filepath.Separator)) && !strings.Contains(n, "\\")
}
