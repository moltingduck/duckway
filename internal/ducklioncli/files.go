package ducklioncli

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type fileCLIEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

const exchangePayloadRoot = "__duckway_payload"
const exchangeFooter = "__duckway_complete__"

func runFiles(args []string, in io.Reader, out io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: ducklion files {list|read|write} PATH [NAME]")
	}
	op, p := args[0], args[1]
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("file path must be a clean absolute path")
	}
	switch op {
	case "list":
		if len(args) != 2 {
			return fmt.Errorf("usage: ducklion files list PATH")
		}
		r, e := os.OpenRoot(p)
		if e != nil {
			return e
		}
		defer r.Close()
		es, e := fs.ReadDir(r.FS(), ".")
		if e != nil {
			return e
		}
		result := make([]fileCLIEntry, 0, len(es))
		for _, x := range es {
			i, e := x.Info()
			if e != nil {
				return e
			}
			if x.Type()&os.ModeSymlink != 0 || !x.Type().IsRegular() && !x.IsDir() {
				continue
			}
			result = append(result, fileCLIEntry{Name: x.Name(), IsDir: x.IsDir(), Size: i.Size()})
			if len(result) > 100000 {
				return fmt.Errorf("directory listing exceeds entry limit")
			}
		}
		return json.NewEncoder(out).Encode(result)
	case "read":
		if len(args) != 2 {
			return fmt.Errorf("usage: ducklion files read PATH")
		}
		return writeTar(p, out)
	case "write":
		if (len(args) != 3 && len(args) != 4) || !safeFileName(args[2]) || len(args) == 4 && args[3] != "--overwrite" {
			return fmt.Errorf("usage: ducklion files write DIRECTORY NAME")
		}
		return readTar(in, p, args[2], len(args) == 4)
	default:
		return fmt.Errorf("unknown files operation %q", op)
	}
}
func safeFileName(s string) bool {
	return s != "" && s != "." && s != ".." && filepath.Base(s) == s && !strings.ContainsAny(s, "/\\")
}
func writeTar(src string, out io.Writer) error {
	tw := tar.NewWriter(out)
	err := filepath.Walk(src, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if i.Mode()&os.ModeSymlink != 0 || !i.Mode().IsRegular() && !i.IsDir() {
			return fmt.Errorf("unsupported file %q", p)
		}
		rel, _ := filepath.Rel(src, p)
		name := exchangePayloadRoot
		if rel != "." {
			name = filepath.ToSlash(filepath.Join(exchangePayloadRoot, rel))
		}
		h := &tar.Header{Name: name, Mode: int64(i.Mode().Perm()), Size: i.Size()}
		if i.IsDir() {
			h.Typeflag = tar.TypeDir
		}
		if e = tw.WriteHeader(h); e != nil {
			return e
		}
		if i.Mode().IsRegular() {
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			n, copyErr := io.Copy(tw, io.LimitReader(f, 1<<30+1))
			if copyErr == nil && n > 1<<30 {
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
func readTar(in io.Reader, dst, name string, overwrite bool) error {
	if i, e := os.Lstat(dst); e != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		if e != nil {
			return e
		}
		return fmt.Errorf("destination is not a real directory")
	}
	stage, e := os.MkdirTemp(dst, ".exchange-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	tr := tar.NewReader(in)
	rootSeen := false
	complete := false
	bytes := int64(0)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if h.Name == exchangeFooter {
			complete = true
			continue
		}
		if complete {
			return fmt.Errorf("archive data after completion footer")
		}
		if h.Name == "" || filepath.IsAbs(h.Name) || strings.Contains(h.Name, "\\") || strings.HasPrefix(filepath.Clean(h.Name), ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path")
		}
		parts := strings.Split(filepath.ToSlash(h.Name), "/")
		if !rootSeen {
			if len(parts) != 1 || parts[0] != exchangePayloadRoot {
				return fmt.Errorf("archive root does not match destination name")
			}
			rootSeen = true
		}
		if parts[0] != exchangePayloadRoot {
			return fmt.Errorf("archive contains multiple roots")
		}
		target := filepath.Join(stage, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(target, filepath.Clean(stage)+string(filepath.Separator)) && target != stage {
			return fmt.Errorf("archive escapes destination")
		}
		if h.Typeflag == tar.TypeDir {
			if e = os.MkdirAll(target, 0700); e != nil {
				return e
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported archive entry")
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		f, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		if h.Size < 0 || h.Size > 1<<30 || bytes > 1<<30-h.Size {
			_ = f.Close()
			return fmt.Errorf("archive exceeds size limit")
		}
		var n int64
		n, e = io.Copy(f, io.LimitReader(tr, h.Size+1))
		bytes += n
		if e == nil && n != h.Size {
			e = io.ErrUnexpectedEOF
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return e
		}
	}
	if !rootSeen || !complete {
		return fmt.Errorf("empty archive")
	}
	dest := filepath.Join(dst, name)
	if old, err := os.Lstat(dest); err == nil {
		if !overwrite {
			return fmt.Errorf("destination already exists")
		}
		if old.IsDir() {
			return fmt.Errorf("refusing non-atomic directory overwrite")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	payload := filepath.Join(stage, exchangePayloadRoot)
	if !overwrite {
		if i, err := os.Lstat(payload); err == nil && !i.IsDir() {
			if err = os.Link(payload, dest); err != nil {
				return err
			}
			return nil
		}
	}
	return os.Rename(payload, dest)
}
