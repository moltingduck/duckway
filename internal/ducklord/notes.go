package ducklord

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// NoteEntry is a level-two markdown heading and the body following it.
type NoteEntry struct{ Title, Body, Preview string }

// NotesScope identifies the independent notebooks exposed by the Notes pane.
// Project notebooks retain the historical projects/<id>/notes.md layout.
type NotesScope string

const (
	NotesGlobal  NotesScope = "global"
	NotesProject NotesScope = "project"
	NotesSession NotesScope = "session"
)

func (s NotesScope) valid() bool { return s == NotesGlobal || s == NotesProject || s == NotesSession }

// ScopedNotesPath returns the durable path for a notebook. IDs are directory
// names, so path traversal and symlink replacement are rejected by the same
// descriptor based checks used by the legacy project API.
func ScopedNotesPath(root string, scope NotesScope, identity string) (string, error) {
	if !scope.valid() {
		return "", errors.New("invalid notes scope")
	}
	if scope == NotesGlobal {
		if strings.TrimSpace(identity) != "" {
			return "", errors.New("global notes do not have an identity")
		}
		p := filepath.Join(root, "notes.md")
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("notes file is symlink")
		}
		return p, nil
	}
	if err := validateNoteIdentity(identity); err != nil {
		return "", err
	}
	dir := "projects"
	if scope == NotesSession {
		dir = "sessions"
	}
	for _, p := range []string{filepath.Join(root, dir), filepath.Join(root, dir, identity)} {
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("notes path contains symlink")
		}
	}
	p := filepath.Join(root, dir, identity, "notes.md")
	if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("notes file is symlink")
	}
	return p, nil
}

// SessionNotesIdentity encodes both stable session identity components into a
// single safe directory name. The encoding is reversible and avoids sharing a
// notebook when two hosts expose the same session ID.
func SessionNotesIdentity(identity SessionIdentity) (string, error) {
	key := identity.Key()
	if key == "" {
		return "", errors.New("invalid session identity")
	}
	return base64.RawURLEncoding.EncodeToString([]byte(key)), nil
}

// NotesPathForScope is a descriptive alias for callers that already use the
// historical NotesPath name.
func NotesPathForScope(root string, scope NotesScope, identity string) (string, error) {
	return ScopedNotesPath(root, scope, identity)
}

func validateNoteIdentity(id string) error {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return errors.New("invalid notes identity")
	}
	return nil
}

// The scoped helpers are the public storage API for new callers. They share
// the legacy parser/format and optimistic revision semantics.
func LoadScopedNotes(root string, scope NotesScope, identity string) ([]NoteEntry, error) {
	return scopedSnapshot(root, scope, identity, false)
}
func SaveScopedNotes(root string, scope NotesScope, identity, markdown string) error {
	return scopedSave(root, scope, identity, markdown)
}

// RestoreScopedNotes restores a previously captured raw notebook, or removes
// it when exists is false. All operations are rooted through directory
// descriptors and use no-follow opens, so rollback cannot follow a replaced
// notebook symlink.
func RestoreScopedNotes(root string, scope NotesScope, identity string, raw []byte, exists bool) error {
	notesMu.Lock()
	defer notesMu.Unlock()
	if exists {
		unlock, err := lockScopedNotes(root, scope, identity, true)
		if err != nil {
			return err
		}
		defer unlock()
		return saveScopedRaw(root, scope, identity, string(raw))
	}
	fd, err := scopedParent(root, scope, identity, false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(fd)
	return unix.Unlinkat(fd, "notes.md", 0)
}

// ReadScopedNotesRaw reads the raw notebook without following symlinks.
func ReadScopedNotesRaw(root string, scope NotesScope, identity string) ([]byte, error) {
	notesMu.Lock()
	defer notesMu.Unlock()
	return readScopedRaw(root, scope, identity, false)
}
func ScopedNotesRevision(root string, scope NotesScope, identity string) ([32]byte, error) {
	_, rev, err := scopedSnapshotRevision(root, scope, identity, false)
	return rev, err
}
func ScopedNotesSnapshot(root string, scope NotesScope, identity string) ([]NoteEntry, [32]byte, error) {
	return scopedSnapshotRevision(root, scope, identity, false)
}
func AppendScopedNoteIfRevision(root string, scope NotesScope, identity string, expected [32]byte, addition NoteEntry) ([]NoteEntry, error) {
	if err := validateNoteTitle(strings.TrimSpace(addition.Title)); err != nil {
		return nil, err
	}
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockScopedNotes(root, scope, identity, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	raw, err := readScopedRaw(root, scope, identity, false)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, err
	}
	if sha256.Sum256(raw) != expected {
		return nil, errors.New("notes changed while editing; reopen and retry")
	}
	entries := append(ParseNotes(string(raw)), NoteEntry{Title: strings.TrimSpace(addition.Title), Body: strings.TrimSpace(addition.Body)})
	if err = saveScopedRaw(root, scope, identity, FormatNotes(entries)); err != nil {
		return nil, err
	}
	return ParseNotes(FormatNotes(entries)), nil
}
func ReplaceScopedNoteIfRevision(root string, scope NotesScope, identity string, index int, expected *[32]byte, replacement NoteEntry) ([]NoteEntry, error) {
	if err := validateNoteTitle(strings.TrimSpace(replacement.Title)); err != nil {
		return nil, err
	}
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockScopedNotes(root, scope, identity, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	raw, err := readScopedRaw(root, scope, identity, false)
	if err != nil {
		return nil, err
	}
	entries := ParseNotes(string(raw))
	if index < 0 || index >= len(entries) {
		return nil, errors.New("note index out of range")
	}
	if expected != nil && sha256.Sum256(raw) != *expected {
		return nil, errors.New("notes changed while editing; reopen and retry")
	}
	entries[index] = NoteEntry{Title: strings.TrimSpace(replacement.Title), Body: strings.TrimSpace(replacement.Body)}
	if err = saveScopedRaw(root, scope, identity, FormatNotes(entries)); err != nil {
		return nil, err
	}
	return ParseNotes(FormatNotes(entries)), nil
}

func scopedSnapshot(root string, scope NotesScope, identity string, create bool) ([]NoteEntry, error) {
	e, _, err := scopedSnapshotRevision(root, scope, identity, create)
	return e, err
}
func scopedSnapshotRevision(root string, scope NotesScope, identity string, create bool) ([]NoteEntry, [32]byte, error) {
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockScopedNotes(root, scope, identity, create)
	if errors.Is(err, unix.ENOENT) {
		return nil, sha256.Sum256(nil), nil
	}
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer unlock()
	raw, err := readScopedRaw(root, scope, identity, create)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return ParseNotes(string(raw)), sha256.Sum256(raw), nil
}
func scopedSave(root string, scope NotesScope, identity, markdown string) error {
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockScopedNotes(root, scope, identity, true)
	if err != nil {
		return err
	}
	defer unlock()
	return saveScopedRaw(root, scope, identity, markdown)
}

var notesMu sync.Mutex

// maxScopedNotesBytes bounds every raw notebook read before it reaches the
// markdown parser or a transfer payload.
const maxScopedNotesBytes = 1 << 20

// FormatNotes writes entries in the portable Notes format used by the editor.
func FormatNotes(entries []NoteEntry) string {
	var b strings.Builder
	for _, entry := range entries {
		b.WriteString("## ")
		b.WriteString(strings.TrimSpace(entry.Title))
		b.WriteString("\n")
		if body := strings.TrimSpace(entry.Body); body != "" {
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ReplaceNote updates one entry while preserving all other parsed entries.
func ReplaceNote(root, projectID string, index int, replacement NoteEntry) ([]NoteEntry, error) {
	return replaceNoteIfRevision(root, projectID, index, nil, replacement)
}

// NotesRevision returns a digest of the raw notebook contents. Callers can use
// it to detect edits made by another process while an external editor is open.
func NotesRevision(root, projectID string) ([32]byte, error) {
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockNotesFile(root, projectID, false)
	if errors.Is(err, unix.ENOENT) {
		return sha256.Sum256(nil), nil
	}
	if err != nil {
		return [32]byte{}, err
	}
	defer unlock()
	b, err := readNotesRaw(root, projectID)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// NotesSnapshot returns parsed entries and their raw-content revision from one
// locked read, so callers can safely use an entry index across an editor.
func NotesSnapshot(root, projectID string) ([]NoteEntry, [32]byte, error) {
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockNotesFile(root, projectID, false)
	if errors.Is(err, unix.ENOENT) {
		return nil, sha256.Sum256(nil), nil
	}
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer unlock()
	raw, err := readNotesRaw(root, projectID)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return ParseNotes(string(raw)), sha256.Sum256(raw), nil
}

// ReplaceNoteIfRevision updates an entry only if the raw notebook still has
// the supplied revision. A nil revision skips the optimistic check.
func ReplaceNoteIfRevision(root, projectID string, index int, expected *[32]byte, replacement NoteEntry) ([]NoteEntry, error) {
	return replaceNoteIfRevision(root, projectID, index, expected, replacement)
}

// AppendNoteIfRevision appends an entry only if the raw notebook still has the
// supplied revision. It reloads the notebook while holding the write lock so
// an editor cannot overwrite a concurrent process's changes with a snapshot.
func AppendNoteIfRevision(root, projectID string, expected [32]byte, addition NoteEntry) ([]NoteEntry, error) {
	title := strings.TrimSpace(addition.Title)
	if err := validateNoteTitle(title); err != nil {
		return nil, err
	}
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockNotesFile(root, projectID, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	raw, err := readNotesRaw(root, projectID)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(raw); got != expected {
		return nil, errors.New("notes changed while editing; reopen and retry")
	}
	entries := ParseNotes(string(raw))
	entries = append(entries, NoteEntry{Title: title, Body: strings.TrimSpace(addition.Body)})
	formatted := FormatNotes(entries)
	if err := saveNotesRaw(root, projectID, formatted); err != nil {
		return nil, err
	}
	return ParseNotes(formatted), nil
}

func validateNoteTitle(title string) error {
	if strings.TrimSpace(title) == "" || strings.ContainsAny(title, "\r\n") {
		return errors.New("note title must be nonempty and single-line")
	}
	return nil
}

func replaceNoteIfRevision(root, projectID string, index int, expected *[32]byte, replacement NoteEntry) ([]NoteEntry, error) {
	title := strings.TrimSpace(replacement.Title)
	if err := validateNoteTitle(title); err != nil {
		return nil, err
	}
	notesMu.Lock()
	defer notesMu.Unlock()
	unlock, err := lockNotesFile(root, projectID, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := loadNotesRaw(root, projectID)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(entries) {
		return nil, errors.New("note index out of range")
	}
	if expected != nil {
		current, err := readNotesRaw(root, projectID)
		if err != nil {
			return nil, err
		}
		if got := sha256.Sum256(current); got != *expected {
			return nil, errors.New("notes changed while editing; reopen and retry")
		}
	}
	entries[index] = NoteEntry{Title: title, Body: strings.TrimSpace(replacement.Body)}
	if err := saveNotesRaw(root, projectID, FormatNotes(entries)); err != nil {
		return nil, err
	}
	return ParseNotes(FormatNotes(entries)), nil
}

func ParseNotes(markdown string) []NoteEntry {
	var out []NoteEntry
	var cur *NoteEntry
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(cur.Body)
			cur.Preview = strings.Join(strings.Fields(cur.Body), " ")
			if utf8.RuneCountInString(cur.Preview) > 120 {
				runes := []rune(cur.Preview)
				cur.Preview = string(runes[:120]) + "…"
			}
			out = append(out, *cur)
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "### ") {
			flush()
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if title == "" {
				cur = nil
				continue
			}
			cur = &NoteEntry{Title: title}
			continue
		}
		if cur != nil {
			if cur.Body != "" {
				cur.Body += "\n"
			}
			cur.Body += line
		}
	}
	flush()
	return out
}

func NotesPath(root, projectID string) (string, error) {
	if strings.TrimSpace(projectID) == "" || projectID == "." || projectID == ".." || filepath.Base(projectID) != projectID || strings.ContainsAny(projectID, `/\\`) {
		return "", errors.New("invalid project ID")
	}
	projects := filepath.Join(root, "projects")
	dir := filepath.Join(projects, projectID)
	for p := filepath.Clean(root); p != filepath.Dir(p); p = filepath.Dir(p) {
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("notes path contains symlink")
		}
	}
	for _, p := range []string{projects, dir} {
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("notes path contains symlink")
		}
	}
	p := filepath.Join(dir, "notes.md")
	if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("notes file is symlink")
	}
	return p, nil
}

func LoadNotes(root, projectID string) ([]NoteEntry, error) {
	notesMu.Lock()
	defer notesMu.Unlock()
	return loadNotesUnlocked(root, projectID)
}
func loadNotesUnlocked(root, projectID string) ([]NoteEntry, error) {
	unlock, err := lockNotesFile(root, projectID, false)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	return loadNotesRaw(root, projectID)
}
func loadNotesRaw(root, projectID string) ([]NoteEntry, error) {
	b, e := readNotesRaw(root, projectID)
	if e != nil {
		return nil, e
	}
	return ParseNotes(string(b)), nil
}
func readNotesRaw(root, projectID string) ([]byte, error) {
	if _, e := NotesPath(root, projectID); e != nil {
		return nil, e
	}
	f, e := openNotes(root, projectID, unix.O_RDONLY, 0, false)
	if errors.Is(e, unix.ENOENT) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if e = unix.Fchmod(int(f.Fd()), 0600); e != nil {
		return nil, e
	}
	b, e := io.ReadAll(f)
	if e != nil {
		return nil, e
	}
	return b, nil
}
func SaveNotes(root, projectID, markdown string) error {
	notesMu.Lock()
	defer notesMu.Unlock()
	return saveNotesUnlocked(root, projectID, markdown)
}

// LockNotes serializes a caller's direct notebook edits with Notes storage.
// The returned function releases the advisory cross-process lock.
func LockNotes(root, projectID string) (func(), error) {
	return lockNotesFile(root, projectID, true)
}
func saveNotesUnlocked(root, projectID, markdown string) error {
	unlock, err := lockNotesFile(root, projectID, true)
	if err != nil {
		return err
	}
	defer unlock()
	return saveNotesRaw(root, projectID, markdown)
}
func saveNotesRaw(root, projectID, markdown string) error {
	if _, e := NotesPath(root, projectID); e != nil {
		return e
	}
	projectFD, e := openProjectDir(root, projectID, true)
	if e != nil {
		return e
	}
	defer unix.Close(projectFD)
	var tmpName string
	var fd int
	for attempt := 0; attempt < 10; attempt++ {
		var random [12]byte
		if _, e = rand.Read(random[:]); e != nil {
			return e
		}
		tmpName = fmt.Sprintf(".notes.md.%x", random)
		fd, e = unix.Openat(projectFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if !errors.Is(e, unix.EEXIST) {
			break
		}
	}
	if e != nil {
		return e
	}
	f := os.NewFile(uintptr(fd), tmpName)
	if _, e = f.WriteString(markdown); e == nil {
		e = unix.Fsync(fd)
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		_ = unix.Unlinkat(projectFD, tmpName, 0)
		return e
	}
	if e = unix.Renameat(projectFD, tmpName, projectFD, "notes.md"); e != nil {
		_ = unix.Unlinkat(projectFD, tmpName, 0)
		return e
	}
	return unix.Fsync(projectFD)
}

func lockNotesFile(root, projectID string, exclusive bool) (func(), error) {
	projectFD, err := openProjectDir(root, projectID, exclusive)
	if err != nil {
		return nil, err
	}
	lockFD, err := unix.Openat(projectFD, ".notes.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	unix.Close(projectFD)
	if err != nil {
		return nil, err
	}
	if err = unix.Fchmod(lockFD, 0600); err == nil {
		kind := unix.LOCK_SH
		if exclusive {
			kind = unix.LOCK_EX
		}
		err = unix.Flock(lockFD, kind)
	}
	if err != nil {
		unix.Close(lockFD)
		return nil, err
	}
	return func() {
		_ = unix.Flock(lockFD, unix.LOCK_UN)
		_ = unix.Close(lockFD)
	}, nil
}

// PrepareNotes creates the Notes file through the no-follow descriptor path
// and returns its canonical path for programs such as vim that require one.
func PrepareNotes(root, projectID string) (string, error) {
	p, err := NotesPath(root, projectID)
	if err != nil {
		return "", err
	}
	f, err := openNotes(root, projectID, unix.O_RDWR|unix.O_CREAT, 0600, true)
	if err != nil {
		return "", err
	}
	if err = unix.Fchmod(int(f.Fd()), 0600); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	// Keep permissions private even when the file already existed.
	// openNotes returns a descriptor and applies no policy beyond no-follow.
	return p, nil
}

// OpenNotes opens the Notes file through the no-follow descriptor path. The
// caller owns the returned descriptor and may pass it to another process.
func OpenNotes(root, projectID string) (*os.File, error) {
	if _, err := NotesPath(root, projectID); err != nil {
		return nil, err
	}
	f, err := openNotes(root, projectID, unix.O_RDWR|unix.O_CREAT, 0600, true)
	if err != nil {
		return nil, err
	}
	if err := unix.Fchmod(int(f.Fd()), 0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// LockScopedNotes serializes direct edits to a scoped notebook.
func LockScopedNotes(root string, scope NotesScope, identity string) (func(), error) {
	return lockScopedNotes(root, scope, identity, true)
}

// PrepareScopedNotes creates a scoped notebook through the no-follow
// descriptor path and returns its canonical path for editors.
func PrepareScopedNotes(root string, scope NotesScope, identity string) (string, error) {
	p, err := ScopedNotesPath(root, scope, identity)
	if err != nil {
		return "", err
	}
	f, err := openScopedNotes(root, scope, identity, unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return "", err
	}
	if err = unix.Fchmod(int(f.Fd()), 0600); err != nil {
		_ = f.Close()
		return "", err
	}
	return p, f.Close()
}

// OpenScopedNotes opens a scoped notebook through the no-follow descriptor path.
func OpenScopedNotes(root string, scope NotesScope, identity string) (*os.File, error) {
	if _, err := ScopedNotesPath(root, scope, identity); err != nil {
		return nil, err
	}
	f, err := openScopedNotes(root, scope, identity, unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, err
	}
	if err = unix.Fchmod(int(f.Fd()), 0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openScopedNotes(root string, scope NotesScope, identity string, flags int) (*os.File, error) {
	fd, err := scopedParent(root, scope, identity, true)
	if err != nil {
		return nil, err
	}
	fileFD, err := unix.Openat(fd, "notes.md", flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	unix.Close(fd)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fileFD), "notes.md"), nil
}

// openNotes opens notes.md through directory descriptors. O_NOFOLLOW on every
// component prevents a symlink replacement between validation and use.
func openNotes(root, projectID string, flags, mode int, create bool) (*os.File, error) {
	projectFD, err := openProjectDir(root, projectID, create)
	if err != nil {
		return nil, err
	}
	defer unix.Close(projectFD)
	if err = unix.Fchmod(projectFD, 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(projectFD, "notes.md", flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "notes.md"), nil
}

func openProjectDir(root, projectID string, create bool) (int, error) {
	rootFD, err := secureDirectory(root, create)
	if err != nil {
		return -1, err
	}
	projectsFD, err := unix.Openat(rootFD, "projects", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil && create && errors.Is(err, unix.ENOENT) {
		if e := unix.Mkdirat(rootFD, "projects", 0700); e != nil && !errors.Is(e, unix.EEXIST) {
			unix.Close(rootFD)
			return -1, e
		}
		projectsFD, err = unix.Openat(rootFD, "projects", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	unix.Close(rootFD)
	if err != nil {
		return -1, err
	}
	projectFD, err := unix.Openat(projectsFD, projectID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil && create && errors.Is(err, unix.ENOENT) {
		if e := unix.Mkdirat(projectsFD, projectID, 0700); e != nil && !errors.Is(e, unix.EEXIST) {
			unix.Close(projectsFD)
			return -1, e
		}
		projectFD, err = unix.Openat(projectsFD, projectID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	unix.Close(projectsFD)
	if err != nil {
		return -1, err
	}
	if err = unix.Fchmod(projectFD, 0700); err != nil {
		unix.Close(projectFD)
		return -1, err
	}
	return projectFD, nil
}

// secureDirectory walks every component with O_NOFOLLOW. This also protects
// roots whose parent directory is attacker-controlled.
func secureDirectory(path string, create bool) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator)), string(filepath.Separator))
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil && create && errors.Is(openErr, unix.ENOENT) {
			if mkErr := unix.Mkdirat(fd, part, 0700); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				unix.Close(fd)
				return -1, mkErr
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			unix.Close(fd)
			return -1, openErr
		}
		unix.Close(fd)
		fd = next
	}
	return fd, nil
}

// OSC52Copy returns a system clipboard (OSC 52) escape sequence for body text only.
func OSC52Copy(body string) string {
	return fmt.Sprintf("\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(body)))
}

func scopedParent(root string, scope NotesScope, identity string, create bool) (int, error) {
	if _, err := ScopedNotesPath(root, scope, identity); err != nil {
		return -1, err
	}
	rootFD, err := secureDirectory(root, create)
	if err != nil {
		return -1, err
	}
	if scope == NotesGlobal {
		return rootFD, nil
	}
	dir := "projects"
	if scope == NotesSession {
		dir = "sessions"
	}
	parent, err := unix.Openat(rootFD, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil && create && errors.Is(err, unix.ENOENT) {
		if err = unix.Mkdirat(rootFD, dir, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(rootFD)
			return -1, err
		}
		parent, err = unix.Openat(rootFD, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	unix.Close(rootFD)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Openat(parent, identity, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil && create && errors.Is(err, unix.ENOENT) {
		if err = unix.Mkdirat(parent, identity, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(parent)
			return -1, err
		}
		fd, err = unix.Openat(parent, identity, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	unix.Close(parent)
	if err != nil {
		return -1, err
	}
	_ = unix.Fchmod(fd, 0700)
	return fd, nil
}

func lockScopedNotes(root string, scope NotesScope, identity string, exclusive bool) (func(), error) {
	fd, err := scopedParent(root, scope, identity, exclusive)
	if err != nil {
		return nil, err
	}
	lockName := ".notes.lock"
	lockFD, err := unix.Openat(fd, lockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	unix.Close(fd)
	if err != nil {
		return nil, err
	}
	_ = unix.Fchmod(lockFD, 0600)
	kind := unix.LOCK_SH
	if exclusive {
		kind = unix.LOCK_EX
	}
	if err = unix.Flock(lockFD, kind); err != nil {
		unix.Close(lockFD)
		return nil, err
	}
	return func() { _ = unix.Flock(lockFD, unix.LOCK_UN); _ = unix.Close(lockFD) }, nil
}

func readScopedRaw(root string, scope NotesScope, identity string, create bool) ([]byte, error) {
	fd, err := scopedParent(root, scope, identity, create)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	f, err := unix.Openat(fd, "notes.md", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(f), "notes.md")
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxScopedNotesBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxScopedNotesBytes {
		return nil, fmt.Errorf("scoped notes exceed size limit (%d bytes)", maxScopedNotesBytes)
	}
	return raw, nil
}
func saveScopedRaw(root string, scope NotesScope, identity, markdown string) error {
	fd, err := scopedParent(root, scope, identity, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var name string
	var rawfd int
	for i := 0; i < 10; i++ {
		var r [12]byte
		if _, err = rand.Read(r[:]); err != nil {
			return err
		}
		name = fmt.Sprintf(".notes.md.%x", r)
		rawfd, err = unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if !errors.Is(err, unix.EEXIST) {
			break
		}
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(rawfd), name)
	if _, err = f.WriteString(markdown); err == nil {
		err = unix.Fsync(rawfd)
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err != nil {
		_ = unix.Unlinkat(fd, name, 0)
		return err
	}
	if err = unix.Renameat(fd, name, fd, "notes.md"); err != nil {
		_ = unix.Unlinkat(fd, name, 0)
		return err
	}
	return unix.Fsync(fd)
}
