package ducklioncli

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	return runFilesContext(context.Background(), args, in, out)
}

func runFilesContext(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
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
		dir, e := r.Open(".")
		if e != nil {
			return e
		}
		defer dir.Close()
		result := make([]fileCLIEntry, 0)
		for {
			es, readErr := dir.ReadDir(256)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
			if len(es) == 0 {
				break
			}
			for _, x := range es {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
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
		}
		return json.NewEncoder(out).Encode(result)
	case "read":
		if len(args) != 2 {
			return fmt.Errorf("usage: ducklion files read PATH")
		}
		return writeTar(ctx, p, out)
	case "write":
		if (len(args) != 3 && len(args) != 4) || !safeFileName(args[2]) || len(args) == 4 && args[3] != "--overwrite" {
			return fmt.Errorf("usage: ducklion files write DIRECTORY NAME")
		}
		return readTar(ctx, in, p, args[2], len(args) == 4)
	default:
		return fmt.Errorf("unknown files operation %q", op)
	}
}
func safeFileName(s string) bool {
	return s != "" && s != "." && s != ".." && filepath.Base(s) == s && !strings.ContainsAny(s, "/\\")
}
func writeTar(ctx context.Context, src string, out io.Writer) error {
	root, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer root.Close()
	tw := tar.NewWriter(out)
	bytes := int64(0)
	err = writeTarRoot(ctx, root, filepath.Base(src), exchangePayloadRoot, tw, &bytes)
	if err != nil {
		return err
	}
	if err = tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	return tw.Close()
}

func writeTarRoot(ctx context.Context, root *os.Root, source, name string, tw *tar.Writer, bytes *int64) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	i, err := root.Lstat(source)
	if err != nil {
		return err
	}
	if i.Mode()&os.ModeSymlink != 0 || (!i.Mode().IsRegular() && !i.IsDir()) {
		return fmt.Errorf("unsupported file %q", source)
	}
	h := &tar.Header{Name: name, Mode: int64(i.Mode().Perm()), Size: i.Size()}
	if i.IsDir() {
		h.Typeflag = tar.TypeDir
	}
	if err = tw.WriteHeader(h); err != nil {
		return err
	}
	f, err := root.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
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
				if e := writeTarRoot(ctx, root, filepath.Join(source, entry.Name()), filepath.ToSlash(filepath.Join(name, entry.Name())), tw, bytes); e != nil {
					return e
				}
			}
		}
		return nil
	}
	n, err := io.Copy(tw, io.LimitReader(f, 1<<30+1))
	if err == nil && (n > 1<<30 || *bytes > 1<<30-n) {
		err = fmt.Errorf("file exceeds transfer limit")
	}
	*bytes += n
	return err
}
func readTar(ctx context.Context, in io.Reader, dst, name string, overwrite bool) error {
	dstRoot, e := os.OpenRoot(dst)
	if e != nil {
		return e
	}
	defer dstRoot.Close()
	stage, e := makeStage(dstRoot)
	if e != nil {
		return e
	}
	defer dstRoot.RemoveAll(stage)
	stageRoot, e := dstRoot.OpenRoot(stage)
	if e != nil {
		return e
	}
	defer stageRoot.Close()
	br := bufio.NewReader(in)
	tr := tar.NewReader(br)
	rootSeen := false
	complete := false
	bytes := int64(0)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		h, e := tr.Next()
		if e == io.EOF {
			if _, trailing := br.ReadByte(); trailing != io.EOF {
				return fmt.Errorf("archive has trailing data")
			}
			break
		}
		if e != nil {
			return e
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
		if h.Name == "" || filepath.IsAbs(h.Name) || strings.Contains(h.Name, "\\") || filepath.Clean(h.Name) != h.Name || strings.HasPrefix(filepath.Clean(h.Name), ".."+string(filepath.Separator)) {
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
		target := filepath.FromSlash(h.Name)
		if h.Typeflag == tar.TypeDir {
			if e = stageRoot.MkdirAll(target, 0700); e != nil {
				return e
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported archive entry")
		}
		if e = stageRoot.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		f, e := stageRoot.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if old, err := dstRoot.Lstat(name); err == nil {
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
	payload := filepath.Join(stage, exchangePayloadRoot)
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

func makeStage(root *os.Root) (string, error) {
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
