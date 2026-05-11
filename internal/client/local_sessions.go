package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LocalSession is one claude-code session discovered on disk. Claude stores
// these as JSONL files under ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl;
// each line is an event (user/assistant message, tool call, queue op, etc.).
// We extract just enough metadata to let a human pick one in Discord.
type LocalSession struct {
	SessionID    string    `json:"session_id"`
	Cwd          string    `json:"cwd"`
	LastActive   time.Time `json:"last_active"`
	MessageCount int       `json:"message_count"` // user + assistant turns
	FirstMessage string    `json:"first_message"` // truncated preview of the user's first turn
	BoundTo      string    `json:"bound_to,omitempty"` // CC channel handle, if already bound
}

// claudeProjectsRoot returns the default location of claude session files.
// Overridable for tests.
func claudeProjectsRoot() (string, error) {
	return ClaudeProjectsRoot()
}

// ClaudeProjectsRoot is the exported form. Returns ~/.claude/projects.
// Used by cmd/client's `duckway cc bind` so it doesn't have to duplicate
// the home-dir + path logic.
func ClaudeProjectsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// ListLocalSessions scans rootDir (claude's projects directory) and returns
// one LocalSession per *.jsonl file, newest first.
//
// `bound` is the channel_handle → session_id map from CCSessionStore; sessions
// appearing as values get BoundTo set so the caller can filter them.
//
// Bad / unreadable files are skipped silently — they shouldn't break the
// listing.
func ListLocalSessions(rootDir string, bound map[string]string) ([]LocalSession, error) {
	// Build reverse map: session_id → handle.
	reverse := map[string]string{}
	for handle, sid := range bound {
		reverse[sid] = handle
	}

	entries, err := os.ReadDir(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no claude usage yet
		}
		return nil, fmt.Errorf("read %s: %w", rootDir, err)
	}

	var out []LocalSession
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := filepath.Join(rootDir, entry.Name())
		files, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(projectDir, f.Name())
			sess, ok := readSessionMetadata(path)
			if !ok {
				continue
			}
			sess.BoundTo = reverse[sess.SessionID]
			out = append(out, sess)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActive.After(out[j].LastActive)
	})
	return out, nil
}

// readSessionMetadata extracts the bits we need from one session file.
// Reads at most the first ~64 lines (enough to find the first user message
// and a cwd) plus file mtime for LastActive.
func readSessionMetadata(path string) (LocalSession, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return LocalSession{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return LocalSession{}, false
	}
	defer f.Close()

	sess := LocalSession{
		SessionID:  strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		LastActive: info.ModTime(),
	}

	scanner := bufio.NewScanner(f)
	// Claude session lines can include long tool outputs — bump the buffer.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	type evt struct {
		Type    string          `json:"type"`
		Cwd     string          `json:"cwd"`
		Message json.RawMessage `json:"message"`
	}
	type msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}

	read := 0
	for scanner.Scan() {
		read++
		// Bound how far we walk for the preview; full count of turns still
		// requires reading the whole file (see below).
		var e evt
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if sess.Cwd == "" && e.Cwd != "" {
			sess.Cwd = e.Cwd
		}
		if e.Type == "user" || e.Type == "assistant" {
			sess.MessageCount++
			if e.Type == "user" && sess.FirstMessage == "" && len(e.Message) > 0 {
				var m msg
				if err := json.Unmarshal(e.Message, &m); err == nil {
					sess.FirstMessage = previewContent(m.Content)
				}
			}
		}
		if read > 4096 {
			// Safety bound — past this point we trust the partial count
			// (most sessions are < 4k events, and the preview/cwd are
			// always at the top).
			break
		}
	}
	// If cwd never appeared in the events, fall back to decoding the
	// directory name. Claude encodes cwd as path-with-`/`-as-`-`.
	if sess.Cwd == "" {
		dirName := filepath.Base(filepath.Dir(path))
		sess.Cwd = "/" + strings.ReplaceAll(strings.TrimPrefix(dirName, "-"), "-", "/")
	}
	if sess.FirstMessage == "" {
		sess.FirstMessage = "(no user message recorded)"
	}
	return sess, true
}

// previewContent flattens claude's message.content (either a plain string or
// an array of content parts) into a short single-line preview.
func previewContent(raw json.RawMessage) string {
	// Try plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return truncatePreview(s)
	}
	// Otherwise it's an array of {type, text|content|...} parts.
	var parts []map[string]interface{}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	for _, p := range parts {
		if t, _ := p["text"].(string); t != "" {
			return truncatePreview(t)
		}
	}
	return ""
}

func truncatePreview(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 120
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

