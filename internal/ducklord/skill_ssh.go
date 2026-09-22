package ducklord

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const skillSSHTimeout = 30 * time.Second
const maxSkillSSHStderrBytes = 4 << 10
const maxSkillListBytes = 64 << 10

const uploadSkillScript = "upload"
const listSkillsScript = "list"

// ListSkillsSSH lists valid skill directories from one agent target. The
// bounded response avoids allowing a remote host to consume unbounded memory.
func ListSkillsSSH(ctx context.Context, client Client, target string) ([]string, error) {
	if !filepath.IsAbs(target) {
		return nil, fmt.Errorf("skill target path must be absolute")
	}
	out := &skillSSHStderr{limit: maxSkillListBytes}
	if err := runSkillSSH(ctx, client, listSkillsScript, target, "", nil, out); err != nil {
		return nil, err
	}
	if out.truncated {
		return nil, fmt.Errorf("skill list response exceeds %d bytes", maxSkillListBytes)
	}
	seen := make(map[string]bool)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		name := strings.TrimSpace(line)
		if name != "" && SafeIdentifier(name) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

const downloadSkillScript = "download"

// UploadSkillSSH validates and streams a managed skill to an absolute remote
// target. The remote replacement happens only after the staged tree has a
// SKILL.md manifest and failed transfers leave the live tree untouched.
func UploadSkillSSH(ctx context.Context, client Client, repository, identifier, target string) (SkillInfo, error) {
	if !filepath.IsAbs(target) {
		return SkillInfo{}, fmt.Errorf("skill target path must be absolute")
	}
	if !SafeIdentifier(identifier) {
		return SkillInfo{}, fmt.Errorf("unsafe skill identifier %q", identifier)
	}
	info, err := InspectSkill(filepath.Join(repository, identifier), identifier)
	if err != nil {
		return SkillInfo{}, err
	}
	data, err := skillTar(filepath.Join(repository, identifier))
	if err != nil {
		return SkillInfo{}, err
	}
	data, err = prefixSkillTar(data, identifier)
	if err != nil {
		return SkillInfo{}, err
	}
	if err := runSkillSSH(ctx, client, uploadSkillScript, target, identifier, bytes.NewReader(data), io.Discard); err != nil {
		return SkillInfo{}, err
	}
	return info, nil
}

func prefixSkillTar(data []byte, identifier string) ([]byte, error) {
	in := tar.NewReader(bytes.NewReader(data))
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for {
		h, err := in.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		cp := *h
		cp.Name = path.Join(identifier, h.Name)
		if err = tw.WriteHeader(&cp); err != nil {
			return nil, err
		}
		if _, err = io.Copy(tw, in); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// DownloadSkillSSH archives a remote skill into a local staging directory,
// validates it, and atomically imports it into the managed repository.
func DownloadSkillSSH(ctx context.Context, client Client, repository, identifier, target string) (SkillInfo, error) {
	preview, err := PrepareDownloadSkillSSH(ctx, client, repository, identifier, target)
	if err != nil {
		return SkillInfo{}, err
	}
	return CommitSkillPreview(&preview)
}

// PrepareDownloadSkillSSH downloads and validates a remote skill into a
// private staging directory. The managed skill is unchanged until commit.
func PrepareDownloadSkillSSH(ctx context.Context, client Client, repository, identifier, target string) (SkillPreview, error) {
	if !filepath.IsAbs(target) {
		return SkillPreview{}, fmt.Errorf("skill target path must be absolute")
	}
	if !SafeIdentifier(identifier) {
		return SkillPreview{}, fmt.Errorf("unsafe skill identifier %q", identifier)
	}
	if err := ensureRepository(repository); err != nil {
		return SkillPreview{}, err
	}
	stage, err := os.MkdirTemp(repository, ".skill-download-")
	if err != nil {
		return SkillPreview{}, err
	}
	defer os.RemoveAll(stage)
	archive := new(bytes.Buffer)
	limited := &skillArchiveBuffer{Buffer: archive, Limit: maxSkillArchiveBytes}
	if err := runSkillSSH(ctx, client, downloadSkillScript, target, identifier, nil, limited); err != nil {
		return SkillPreview{}, err
	}
	if limited.Exceeded {
		return SkillPreview{}, fmt.Errorf("skill archive response exceeds %d bytes", maxSkillArchiveBytes)
	}
	source := filepath.Join(stage, identifier)
	if err := extractSSHSKillTar(bytes.NewReader(archive.Bytes()), source, identifier); err != nil {
		return SkillPreview{}, err
	}
	return PrepareSkillPreview(repository, identifier, source)
}

func runSkillSSH(parent context.Context, client Client, script, target, identifier string, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(parent, skillSSHTimeout)
	defer cancel()
	parts := client.SSHCommandParts()
	remote := []string{"/usr/local/bin/ducklion", "skill", script, target}
	if identifier != "" {
		remote = append(remote, identifier)
	}
	args := SSHArgs(client, false, remote...)
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], args...)...)
	cmd.Stdin, cmd.Stdout = stdin, stdout
	stderr := &skillSSHStderr{limit: maxSkillSSHStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ssh skill transfer to %s: %w", client.Name, ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ssh skill transfer to %s: %s", client.Name, msg)
	}
	return nil
}

// skillSSHStderr keeps failures useful without allowing an untrusted remote
// command to grow the local process's memory without bound.
type skillSSHStderr struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (s *skillSSHStderr) Write(p []byte) (int, error) {
	original := len(p)
	if s.limit <= s.Len() {
		s.truncated = true
		return original, nil
	}
	if remaining := s.limit - s.Len(); len(p) > remaining {
		p = p[:remaining]
		s.truncated = true
	}
	_, _ = s.Buffer.Write(p)
	return original, nil
}

func skillTar(source string) ([]byte, error) {
	if err := skillPathHasNoSymlinkComponents(source); err != nil {
		return nil, err
	}
	rfd, err := unix.Open(source, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rfd)
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	files := 0
	var total int64
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill contains symlink %q", rel)
		}
		hi, err := entry.Info()
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if entry.IsDir() {
			if err := skillPathHasNoSymlinkComponents(path); err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Name: name, Mode: int64(hi.Mode().Perm()), Typeflag: tar.TypeDir})
		}
		if !hi.Mode().IsRegular() {
			return fmt.Errorf("skill contains non-regular file %q", rel)
		}
		if skillTransferTestHook != nil {
			skillTransferTestHook(rel)
		}
		files++
		if files > MaxSkillFiles {
			return fmt.Errorf("skill exceeds %d files", MaxSkillFiles)
		}
		if err := skillPathHasNoSymlinkComponents(path); err != nil {
			return err
		}
		data, copyErr := readSkillFile(rfd, rel, MaxSkillBytes-total)
		n := int64(len(data))
		if copyErr == nil {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(hi.Mode().Perm()), Size: n, Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			_, copyErr = tw.Write(data)
		}
		if copyErr == nil && n > MaxSkillBytes-total {
			return fmt.Errorf("skill exceeds %d bytes", MaxSkillBytes)
		}
		if copyErr == nil {
			latest, statErr := os.Stat(path)
			if statErr != nil {
				return statErr
			}
			if latest.Size() != hi.Size() {
				return fmt.Errorf("skill source changed during archive")
			}
			total += n
		}
		err = copyErr
		return err
	})
	if err != nil {
		_ = tw.Close()
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type skillArchiveBuffer struct {
	*bytes.Buffer
	Limit    int64
	Exceeded bool
}

func (b *skillArchiveBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.Limit {
		b.Exceeded = true
		return 0, fmt.Errorf("skill archive exceeds limit")
	}
	return b.Buffer.Write(p)
}

func extractSSHSKillTar(input io.Reader, destination, identifier string) error {
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	tr := tar.NewReader(io.LimitReader(input, maxSkillArchiveBytes+1))
	var total int64
	files := 0
	seen := make(map[string]bool)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		prefix := identifier + "/"
		if name == identifier {
			continue
		}
		canonical := name == h.Name || (h.Typeflag == tar.TypeDir && h.Name == name+"/")
		if h.Name == "" || !canonical || path.IsAbs(h.Name) || !strings.HasPrefix(name, prefix) || name == "." || strings.HasPrefix(name, "../") || seen[name] {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		seen[name] = true
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" || rel == "." || path.IsAbs(rel) {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		dest := filepath.Join(destination, filepath.FromSlash(rel))
		if checked, _ := filepath.Rel(destination, dest); checked == ".." || strings.HasPrefix(checked, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dest, 0700); err != nil {
				return err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("skill archive contains non-regular file %q", h.Name)
		}
		if files >= MaxSkillFiles || h.Size < 0 || h.Size > MaxSkillBytes-total {
			return fmt.Errorf("skill archive exceeds limits")
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(f, tr, h.Size)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		total += h.Size
		files++
	}
	return nil
}
