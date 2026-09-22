package ducklord

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const projectTransferVersion = 1
const (
	maxProjectTransferBytes = 8 << 20
	maxTransferTabs         = 256
	maxTransferPanes        = 4096
	maxTransferPaneDepth    = 64
	maxTransferSessions     = 4096
	maxTransferStringBytes  = 4096
)

// ProjectTransfer is deliberately limited to local layout and caller supplied
// metadata. It cannot contain credentials, output, or live session objects.
type ProjectTransfer struct {
	Version      int                     `json:"version"`
	Project      LocalProject            `json:"project"`
	ProjectNotes []NoteEntry             `json:"project_notes,omitempty"`
	SessionNotes []ProjectSessionNotes   `json:"session_notes,omitempty"`
	Bookmarks    []ProjectOutputBookmark `json:"bookmarks,omitempty"`
}

// ProjectOutputBookmark contains metadata only; terminal output is never part
// of a transfer document.
type ProjectSessionNotes struct {
	Session SessionIdentity `json:"session"`
	Entries []NoteEntry     `json:"entries,omitempty"`
}

type ProjectOutputBookmark struct {
	Session           SessionIdentity `json:"session"`
	Label             string          `json:"label,omitempty"`
	OutputAnchor      string          `json:"output_anchor,omitempty"`
	OutputFingerprint string          `json:"output_fingerprint,omitempty"`
}

type ProjectImportPreview struct {
	Project       LocalProject
	Unavailable   []SessionIdentity
	NameCollision bool
}

func ExportProject(project LocalProject, projectNotes []NoteEntry, sessionNotes []ProjectSessionNotes, bookmarks []ProjectOutputBookmark) ([]byte, error) {
	l := ProjectLayout{Projects: []LocalProject{{ID: DefaultProjectID, Name: "Default Project"}, project}}
	if project.ID == DefaultProjectID {
		return nil, fmt.Errorf("cannot export Default Project")
	}
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("invalid project: %w", err)
	}
	if err := validateTransferLayout(project); err != nil {
		return nil, err
	}
	if err := validateTransferMetadata(projectNotes, sessionNotes, bookmarks, l.SessionsForProject(project.ID)); err != nil {
		return nil, err
	}
	d := ProjectTransfer{Version: projectTransferVersion, Project: project,
		ProjectNotes: append([]NoteEntry(nil), projectNotes...), SessionNotes: append([]ProjectSessionNotes(nil), sessionNotes...), Bookmarks: append([]ProjectOutputBookmark(nil), bookmarks...)}
	return json.MarshalIndent(d, "", "  ")
}

func DecodeProjectTransfer(data []byte) (ProjectTransfer, error) {
	if len(data) == 0 || len(data) > maxProjectTransferBytes {
		return ProjectTransfer{}, fmt.Errorf("project transfer exceeds size limit")
	}
	var d ProjectTransfer
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("invalid project transfer: %w", err)
	}
	if d.Version != projectTransferVersion {
		return d, fmt.Errorf("unsupported project transfer version %d", d.Version)
	}
	if d.Project.ID == DefaultProjectID {
		return d, fmt.Errorf("cannot import Default Project")
	}
	if err := validateTransferLayout(d.Project); err != nil {
		return d, err
	}
	if err := validateTransferMetadata(d.ProjectNotes, d.SessionNotes, d.Bookmarks, dLayoutSessions(d.Project)); err != nil {
		return d, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return d, fmt.Errorf("trailing project transfer data")
	}
	l := ProjectLayout{Projects: []LocalProject{{ID: DefaultProjectID, Name: "Default Project"}, d.Project}}
	if err := l.Validate(); err != nil {
		return d, fmt.Errorf("invalid project: %w", err)
	}
	return d, nil
}

func validateTransferLayout(p LocalProject) error {
	if len(p.Tabs) > maxTransferTabs || len(p.ID) > maxTransferStringBytes || len(p.Name) > maxTransferStringBytes {
		return fmt.Errorf("project transfer layout exceeds limits")
	}
	count, sessions := 0, 0
	for _, tab := range p.Tabs {
		if len(tab.ID) > maxTransferStringBytes || len(tab.Name) > maxTransferStringBytes {
			return fmt.Errorf("project transfer string exceeds limit")
		}
		if err := validateTransferPane(tab.Root, 1, &count, &sessions); err != nil {
			return err
		}
	}
	return nil
}
func validateTransferPane(p *SessionPane, depth int, count, sessions *int) error {
	if p == nil {
		return nil
	}
	*count++
	if *count > maxTransferPanes || depth > maxTransferPaneDepth {
		return fmt.Errorf("project transfer pane layout exceeds limits")
	}
	if len(p.ID) > maxTransferStringBytes {
		return fmt.Errorf("project transfer string exceeds limit")
	}
	if p.Session != nil {
		*sessions++
		if *sessions > maxTransferSessions {
			return fmt.Errorf("project transfer has too many sessions")
		}
		return nil
	}
	if p.First == nil || p.Second == nil {
		return fmt.Errorf("invalid split pane")
	}
	if err := validateTransferPane(p.First, depth+1, count, sessions); err != nil {
		return err
	}
	return validateTransferPane(p.Second, depth+1, count, sessions)
}

func validateTransferMetadata(notes []NoteEntry, sessionNotes []ProjectSessionNotes, bookmarks []ProjectOutputBookmark, members []SessionIdentity) error {
	if len(notes) > maxTransferSessions || len(sessionNotes) > maxTransferSessions || len(bookmarks) > maxTransferSessions || len(members) > maxTransferSessions {
		return fmt.Errorf("project transfer contains too many entries")
	}
	for _, n := range notes {
		if !validTransferNote(n) {
			return fmt.Errorf("invalid note metadata")
		}
	}
	member := map[SessionIdentity]bool{}
	for _, m := range members {
		member[m] = true
	}
	groups := map[SessionIdentity]bool{}
	for _, sn := range sessionNotes {
		if !member[sn.Session] || groups[sn.Session] {
			return fmt.Errorf("invalid or duplicate session notes identity")
		}
		groups[sn.Session] = true
		if sn.Session.Key() == "" {
			return fmt.Errorf("invalid session notes identity")
		}
		if len(sn.Entries) > maxTransferSessions {
			return fmt.Errorf("invalid note metadata")
		}
		for _, n := range sn.Entries {
			if !validTransferNote(n) {
				return fmt.Errorf("invalid note metadata")
			}
		}
	}
	seen := map[string]bool{}
	for _, b := range bookmarks {
		if b.Session.Key() == "" || !member[b.Session] {
			return fmt.Errorf("invalid output bookmark session")
		}
		for _, value := range []string{b.Label, b.OutputAnchor, b.OutputFingerprint} {
			if !utf8.ValidString(value) || len(value) > 256 {
				return fmt.Errorf("invalid output bookmark metadata")
			}
		}
		key := b.Session.Key() + "\x00" + b.Label + "\x00" + b.OutputAnchor + "\x00" + b.OutputFingerprint
		if seen[key] {
			return fmt.Errorf("duplicate output bookmark")
		}
		seen[key] = true
	}
	return nil
}

func validTransferNote(n NoteEntry) bool {
	return utf8.ValidString(n.Title) && utf8.ValidString(n.Body) && utf8.ValidString(n.Preview) && len(n.Title) <= 256 && len(n.Body) <= 1<<20 && len(n.Preview) <= 256
}

func PreviewProjectImport(data []byte, available map[SessionIdentity]bool, existingNames map[string]bool) (ProjectImportPreview, error) {
	d, err := DecodeProjectTransfer(data)
	if err != nil {
		return ProjectImportPreview{}, err
	}
	p := ProjectImportPreview{Project: d.Project, NameCollision: existingNames[d.Project.Name]}
	seen := map[SessionIdentity]bool{}
	for _, tab := range d.Project.Tabs {
		collectPaneSessions(tab.Root, seen)
	}
	for s := range seen {
		if !available[s] {
			p.Unavailable = append(p.Unavailable, s)
		}
	}
	sort.Slice(p.Unavailable, func(i, j int) bool { return p.Unavailable[i].Key() < p.Unavailable[j].Key() })
	return p, nil
}

func collectPaneSessions(p *SessionPane, out map[SessionIdentity]bool) {
	if p == nil {
		return
	}
	if p.Session != nil {
		out[*p.Session] = true
		return
	}
	collectPaneSessions(p.First, out)
	collectPaneSessions(p.Second, out)
}

// WriteProjectTransfer writes with owner-only permissions and an atomic rename.
func WriteProjectTransfer(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New("empty project transfer")
	}
	dir := filepath.Dir(path)
	parent, err := openTransferParent(dir)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	base := filepath.Base(path)
	var tmpFD int
	var tmpName string
	for i := 0; i < 10; i++ {
		var token [12]byte
		if _, err = rand.Read(token[:]); err != nil {
			return err
		}
		tmpName = fmt.Sprintf(".project-transfer-%x", token)
		tmpFD, err = unix.Openat(parent, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if !errors.Is(err, unix.EEXIST) {
			break
		}
	}
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = unix.Unlinkat(parent, tmpName, 0)
		}
	}()
	tmp := os.NewFile(uintptr(tmpFD), tmpName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = unix.Renameat(parent, tmpName, parent, base); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

// openTransferParent creates and pins every parent component. O_NOFOLLOW is
// applied at each step, so a pre-existing symlink or a later pathname swap
// cannot redirect the temporary file or final rename outside this directory.
func openTransferParent(path string) (int, error) {
	clean := filepath.Clean(path)
	absolute := filepath.IsAbs(clean)
	start := "."
	if absolute {
		start = string(filepath.Separator)
	}
	fd, err := unix.Open(start, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
	for _, part := range parts {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(fd, part, 0700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				unix.Close(fd)
				return -1, mkdirErr
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

func dLayoutSessions(p LocalProject) []SessionIdentity {
	l := ProjectLayout{Projects: []LocalProject{{ID: DefaultProjectID, Name: "Default Project"}, p}}
	return l.SessionsForProject(p.ID)
}
