package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	sessionStateVersion = 1
	sessionStateFile    = "agent-sessions.json"

	SessionStatusRunning = "running"
	SessionStatusStopped = "stopped"
	SessionStatusStale   = "stale"
)

type SessionState struct {
	Version  int             `json:"version"`
	Sessions []SessionRecord `json:"sessions"`
}

type SessionRecord struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	AgentType   string `json:"agent_type"`
	Cwd         string `json:"cwd"`
	TmuxSession string `json:"tmux_session"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	Status      string `json:"status"`
}

type SessionStartOptions struct {
	Name      string
	Kind      string
	AgentType string
	Cwd       string
	Command   []string
}

type SessionManager struct {
	configDir string
	tmux      sessionTmux
}

type sessionTmux interface {
	HasSession(name string) bool
	NewSession(name, cwd string, command []string) error
	Send(name, text string) error
	Capture(name string, lines int) (string, error)
	Kill(name string) error
}

func NewSessionManager(configDir string, tmux sessionTmux) *SessionManager {
	if tmux == nil {
		tmux = realSessionTmux{}
	}
	return &SessionManager{configDir: configDir, tmux: tmux}
}

func (m *SessionManager) Load() (*SessionState, error) {
	data, err := os.ReadFile(m.statePath())
	if os.IsNotExist(err) {
		return &SessionState{Version: sessionStateVersion, Sessions: []SessionRecord{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse session state: %w", err)
	}
	if state.Version == 0 {
		state.Version = sessionStateVersion
	}
	if state.Version != sessionStateVersion {
		return nil, fmt.Errorf("unsupported session state version %d", state.Version)
	}
	if state.Sessions == nil {
		state.Sessions = []SessionRecord{}
	}
	return &state, nil
}

func (m *SessionManager) List() ([]SessionRecord, error) {
	state, err := m.Load()
	if err != nil {
		return nil, err
	}
	out := make([]SessionRecord, len(state.Sessions))
	copy(out, state.Sessions)
	for i := range out {
		if out[i].Status == SessionStatusRunning && !m.tmux.HasSession(out[i].TmuxSession) {
			out[i].Status = SessionStatusStale
		}
	}
	return out, nil
}

func (m *SessionManager) Start(opts SessionStartOptions) (*SessionRecord, error) {
	name, err := validateSessionName(opts.Name)
	if err != nil {
		return nil, err
	}
	if opts.Kind == "" {
		opts.Kind = "terminal"
	}
	if opts.AgentType == "" {
		opts.AgentType = "shell"
	}
	cwd, err := validateSessionCwd(opts.Cwd)
	if err != nil {
		return nil, err
	}
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("command is required")
	}
	state, err := m.Load()
	if err != nil {
		return nil, err
	}
	tmuxName := terminalTmuxSessionName(name)
	filtered := state.Sessions[:0]
	for _, rec := range state.Sessions {
		if rec.Name != name {
			filtered = append(filtered, rec)
			continue
		}
		if rec.Status == SessionStatusRunning && m.tmux.HasSession(rec.TmuxSession) {
			return nil, fmt.Errorf("session %q is already running", name)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec := SessionRecord{
		ID:          newSessionID(),
		Name:        name,
		Kind:        opts.Kind,
		AgentType:   opts.AgentType,
		Cwd:         cwd,
		TmuxSession: tmuxName,
		CreatedAt:   now,
		UpdatedAt:   now,
		Status:      SessionStatusRunning,
	}
	if err := m.tmux.NewSession(tmuxName, cwd, opts.Command); err != nil {
		return nil, err
	}
	state.Sessions = append(filtered, rec)
	if err := m.save(state); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (m *SessionManager) Send(name, text string) error {
	rec, err := m.lookup(name)
	if err != nil {
		return err
	}
	if !m.tmux.HasSession(rec.TmuxSession) {
		return fmt.Errorf("session %q is not running", name)
	}
	return m.tmux.Send(rec.TmuxSession, text)
}

func (m *SessionManager) Read(name string, lines int) (string, error) {
	rec, err := m.lookup(name)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 120
	}
	return m.tmux.Capture(rec.TmuxSession, lines)
}

func (m *SessionManager) Stop(name string) error {
	state, err := m.Load()
	if err != nil {
		return err
	}
	for i := range state.Sessions {
		if state.Sessions[i].Name == name || state.Sessions[i].ID == name {
			if m.tmux.HasSession(state.Sessions[i].TmuxSession) {
				if err := m.tmux.Kill(state.Sessions[i].TmuxSession); err != nil {
					return err
				}
			}
			state.Sessions[i].Status = SessionStatusStopped
			state.Sessions[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			return m.save(state)
		}
	}
	return fmt.Errorf("unknown session %q", name)
}

func (m *SessionManager) AttachArgs(name string) ([]string, error) {
	rec, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	return []string{"attach", "-t", rec.TmuxSession}, nil
}

func (m *SessionManager) lookup(name string) (*SessionRecord, error) {
	state, err := m.Load()
	if err != nil {
		return nil, err
	}
	for i := range state.Sessions {
		if state.Sessions[i].Name == name || state.Sessions[i].ID == name {
			rec := state.Sessions[i]
			return &rec, nil
		}
	}
	return nil, fmt.Errorf("unknown session %q", name)
}

func (m *SessionManager) save(state *SessionState) error {
	if state.Version == 0 {
		state.Version = sessionStateVersion
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.configDir, 0700); err != nil {
		return err
	}
	return writeFileAtomic(m.statePath(), data, 0600)
}

func (m *SessionManager) statePath() string {
	return filepath.Join(m.configDir, sessionStateFile)
}

func validateSessionName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("session name is required")
	}
	if len(name) > 64 {
		return "", fmt.Errorf("invalid session name %q", name)
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return "", fmt.Errorf("invalid session name %q", name)
	}
	return name, nil
}

func validateSessionCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	eval, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("invalid cwd: %w", err)
	}
	info, err := os.Stat(eval)
	if err != nil {
		return "", fmt.Errorf("invalid cwd: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("invalid cwd: not a directory")
	}
	return eval, nil
}

func terminalTmuxSessionName(name string) string {
	return "duckway-term-" + name
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(b[:])
}

type realSessionTmux struct{}

func (realSessionTmux) HasSession(name string) bool {
	return tmuxHasSession(name)
}

func (realSessionTmux) NewSession(name, cwd string, command []string) error {
	args := []string{"new-session", "-d", "-s", name, "-n", name, "-c", cwd}
	args = append(args, command...)
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %w (%s)", err, string(out))
	}
	return nil
}

func (realSessionTmux) Send(name, text string) error {
	if text != "" {
		if err := tmuxSendKeys(name, "-l", text); err != nil {
			return err
		}
	}
	return tmuxSendKeys(name, "Enter")
}

func (realSessionTmux) Capture(name string, lines int) (string, error) {
	start := fmt.Sprintf("-%d", lines)
	out, err := exec.Command("tmux", "capture-pane", "-p", "-t", name, "-S", start).Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return string(out), nil
}

func (realSessionTmux) Kill(name string) error {
	_ = exec.Command("tmux", "kill-session", "-t", name).Run()
	return nil
}
