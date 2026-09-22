package skill

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxFiles = 1024
const maxBytes int64 = 16 << 20

func safeID(s string) bool { return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\\`) }

func Run(args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) < 2 || len(args) > 3 || (args[0] != "list" && args[0] != "upload" && args[0] != "download" && args[0] != "delete") {
		return fmt.Errorf("usage: ducklion skill {list|upload|download|delete} TARGET [SKILL]")
	}
	op, target := args[0], args[1]
	if !path.IsAbs(target) {
		return fmt.Errorf("skill target path must be absolute")
	}
	if err := noSymlinkComponents(target); err != nil {
		return err
	}
	r, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer r.Close()
	if op == "list" {
		return list(r, out)
	}
	if len(args) < 3 {
		return fmt.Errorf("skill name required")
	}
	id := args[2]
	if !safeID(id) {
		return fmt.Errorf("unsafe skill identifier")
	}
	switch op {
	case "download":
		return download(r, id, out)
	case "delete":
		return deleteSkill(r, id)
	case "upload":
		return upload(r, id, in)
	}
	return nil
}

func validSkill(r *os.Root, id string) error {
	i, err := r.Lstat(id)
	if err != nil {
		return err
	}
	if !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skill must be a directory and not a symlink")
	}
	m, err := r.Lstat(path.Join(id, "SKILL.md"))
	if err != nil || !m.Mode().IsRegular() || m.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skill missing regular SKILL.md")
	}
	return nil
}
func deleteSkill(r *os.Root, id string) error {
	if err := validSkill(r, id); err != nil {
		return err
	}
	return r.RemoveAll(id)
}

func noSymlinkComponents(name string) error {
	clean := filepath.Clean(name)
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill target contains symlink: %s", cur)
		}
	}
	return nil
}

func list(r *os.Root, out io.Writer) error {
	ents, err := fs.ReadDir(r.FS(), ".")
	if err != nil {
		return err
	}
	for _, ent := range ents {
		if !ent.IsDir() || ent.Type()&os.ModeSymlink != 0 {
			continue
		}
		if openErr := validSkill(r, ent.Name()); openErr == nil {
			fmt.Fprintln(out, ent.Name())
		}
	}
	return nil
}

func download(r *os.Root, id string, out io.Writer) error {
	if err := validSkill(r, id); err != nil {
		return fmt.Errorf("requested skill is invalid: %w", err)
	}
	tw := tar.NewWriter(out)
	defer tw.Close()
	return fs.WalkDir(r.FS(), id, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill contains symlink %q", p)
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("skill contains non-regular file %q", p)
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: p, Mode: 0700, Typeflag: tar.TypeDir})
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if e = tw.WriteHeader(&tar.Header{Name: p, Mode: int64(info.Mode().Perm()), Size: info.Size()}); e != nil {
			return e
		}
		f, e := r.Open(p)
		if e != nil {
			return e
		}
		defer f.Close()
		_, e = io.Copy(tw, f)
		return e
	})
}

func upload(r *os.Root, id string, in io.Reader) error {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := ".ducklion-upload-" + hex.EncodeToString(suffix[:])
	if err := r.Mkdir(tmp, 0700); err != nil {
		return fmt.Errorf("create upload staging directory: %w", err)
	}
	defer r.RemoveAll(tmp)
	tr := tar.NewReader(in)
	found := false
	var total int64
	var files int
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		files++
		if files > maxFiles || h.Size < 0 || h.Size > maxBytes-total {
			return fmt.Errorf("skill archive exceeds limits")
		}
		total += h.Size
		if h.Name != id && !strings.HasPrefix(h.Name, id+"/") {
			return fmt.Errorf("unsafe skill archive")
		}
		rel := strings.TrimPrefix(h.Name, id)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		if strings.Contains(rel, "..") || strings.ContainsAny(rel, `\\`) {
			return fmt.Errorf("unsafe skill archive")
		}
		dst := path.Join(tmp, rel)
		if h.FileInfo().IsDir() {
			if err = r.MkdirAll(dst, 0700); err != nil {
				return err
			}
			continue
		}
		if !h.FileInfo().Mode().IsRegular() {
			return fmt.Errorf("unsafe skill archive")
		}
		if err = r.MkdirAll(path.Dir(dst), 0700); err != nil {
			return err
		}
		f, err := r.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return err
		}
		found = found || rel == "SKILL.md"
	}
	if !found {
		return fmt.Errorf("skill archive missing SKILL.md")
	}
	if _, err := r.Lstat(id); err == nil {
		if err := validSkill(r, id); err != nil {
			return fmt.Errorf("existing target is not a skill: %w", err)
		}
		if err := r.RemoveAll(id); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return r.Rename(path.Join(tmp), id)
}
