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
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion"
)

const (
	sessionStateVersion = 1
	sessionStateFile    = "agent-sessions.json"

	SessionStatusRunning = "running"
	SessionStatusStopped = "stopped"
	SessionStatusStale   = "stale"

	SessionBackendPTY  = "pty"
	SessionBackendTmux = "tmux"
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
	Backend     string `json:"backend,omitempty"`
	PtySession  string `json:"pty_session,omitempty"`
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
	Backend   string
	Command   []string
}

type SessionManager struct {
	configDir string
	backend   sessionBackend
}

type sessionBackend interface {
	Name() string
	TargetName(name string) string
	HasSession(name string) bool
	Start(name, cwd, agentType string, command []string) error
	Send(name, text string) error
	Capture(name string, lines int) (string, error)
	Kill(name string) error
	AttachArgs(name string) []string
}

func NewSessionManager(configDir string, tmux sessionTmux) *SessionManager {
	if tmux != nil {
		return &SessionManager{configDir: configDir, backend: tmuxSessionBackend{tmux: tmux}}
	}
	return &SessionManager{configDir: configDir, backend: newPTYSessionBackend(configDir)}
}

type sessionTmux interface {
	HasSession(name string) bool
	NewSession(name, cwd string, command []string) error
	Send(name, text string) error
	Capture(name string, lines int) (string, error)
	Kill(name string) error
}

func NewTmuxSessionManager(configDir string, tmux sessionTmux) *SessionManager {
	if tmux == nil {
		tmux = realSessionTmux{}
	}
	return &SessionManager{configDir: configDir, backend: tmuxSessionBackend{tmux: tmux}}
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
		if out[i].Status == SessionStatusRunning && !m.backendForRecord(out[i]).HasSession(out[i].TargetSession()) {
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
	backend := m.backend
	if opts.Backend != "" {
		var err error
		backend, err = m.backendByName(opts.Backend)
		if err != nil {
			return nil, err
		}
	}
	cwd, err := validateSessionCwd(opts.Cwd)
	if err != nil {
		return nil, err
	}
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("command is required")
	}

	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := m.loadLocked()
	if err != nil {
		return nil, err
	}
	targetName := backend.TargetName(name)
	filtered := state.Sessions[:0]
	for _, rec := range state.Sessions {
		if rec.Name != name {
			filtered = append(filtered, rec)
			continue
		}
		if rec.Status == SessionStatusRunning && m.backendForRecord(rec).HasSession(rec.TargetSession()) {
			return nil, fmt.Errorf("session %q is already running", name)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec := SessionRecord{
		ID:        newSessionID(),
		Name:      name,
		Kind:      opts.Kind,
		AgentType: opts.AgentType,
		Cwd:       cwd,
		Backend:   backend.Name(),
		CreatedAt: now,
		UpdatedAt: now,
		Status:    SessionStatusRunning,
	}
	if backend.Name() == SessionBackendTmux {
		rec.TmuxSession = targetName
	} else {
		rec.PtySession = targetName
	}
	if err := backend.Start(targetName, cwd, opts.AgentType, opts.Command); err != nil {
		return nil, err
	}
	state.Sessions = append(filtered, rec)
	if err := m.saveLocked(state); err != nil {
		_ = backend.Kill(targetName)
		return nil, err
	}
	return &rec, nil
}

func (m *SessionManager) Send(name, text string) error {
	rec, err := m.lookup(name)
	if err != nil {
		return err
	}
	backend := m.backendForRecord(*rec)
	if !backend.HasSession(rec.TargetSession()) {
		return fmt.Errorf("session %q is not running", name)
	}
	return backend.Send(rec.TargetSession(), text)
}

func (m *SessionManager) Read(name string, lines int) (string, error) {
	rec, err := m.lookup(name)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 120
	}
	return m.backendForRecord(*rec).Capture(rec.TargetSession(), lines)
}

func (m *SessionManager) Stop(name string) error {
	state, err := m.Load()
	if err != nil {
		return err
	}
	for i := range state.Sessions {
		if state.Sessions[i].Name == name || state.Sessions[i].ID == name {
			backend := m.backendForRecord(state.Sessions[i])
			if backend.HasSession(state.Sessions[i].TargetSession()) {
				if err := backend.Kill(state.Sessions[i].TargetSession()); err != nil {
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
	return m.backendForRecord(*rec).AttachArgs(rec.TargetSession()), nil
}

func (r SessionRecord) TargetSession() string {
	if r.Backend == SessionBackendPTY && r.PtySession != "" {
		return r.PtySession
	}
	if r.TmuxSession != "" {
		return r.TmuxSession
	}
	if r.PtySession != "" {
		return r.PtySession
	}
	return terminalSessionName(r.Name)
}

func (r SessionRecord) DisplayBackend() string {
	if r.Backend == "" {
		return SessionBackendTmux
	}
	return r.Backend
}

func (m *SessionManager) backendForRecord(rec SessionRecord) sessionBackend {
	switch rec.Backend {
	case SessionBackendPTY:
		if m.backend.Name() == SessionBackendPTY {
			return m.backend
		}
		return newPTYSessionBackend(m.configDir)
	case SessionBackendTmux, "":
		if m.backend.Name() == SessionBackendTmux {
			return m.backend
		}
		return tmuxSessionBackend{tmux: realSessionTmux{}}
	default:
		return m.backend
	}
}

func (m *SessionManager) backendByName(name string) (sessionBackend, error) {
	switch strings.TrimSpace(name) {
	case "", SessionBackendPTY:
		if m.backend.Name() == SessionBackendPTY {
			return m.backend, nil
		}
		return newPTYSessionBackend(m.configDir), nil
	case SessionBackendTmux:
		if m.backend.Name() == SessionBackendTmux {
			return m.backend, nil
		}
		return tmuxSessionBackend{tmux: realSessionTmux{}}, nil
	default:
		return nil, fmt.Errorf("unknown session backend %q", name)
	}
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
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return m.saveLocked(state)
}

func (m *SessionManager) saveLocked(state *SessionState) error {
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

func (m *SessionManager) loadLocked() (*SessionState, error) {
	return m.Load()
}

func (m *SessionManager) lock() (func(), error) {
	if err := os.MkdirAll(m.configDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.configDir, "agent-sessions.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
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

func terminalSessionName(name string) string {
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

type tmuxSessionBackend struct {
	tmux sessionTmux
}

func (b tmuxSessionBackend) Name() string { return SessionBackendTmux }
func (b tmuxSessionBackend) TargetName(name string) string {
	return terminalTmuxSessionName(name)
}
func (b tmuxSessionBackend) HasSession(name string) bool { return b.tmux.HasSession(name) }
func (b tmuxSessionBackend) Start(name, cwd, _ string, command []string) error {
	return b.tmux.NewSession(name, cwd, command)
}
func (b tmuxSessionBackend) Send(name, text string) error { return b.tmux.Send(name, text) }
func (b tmuxSessionBackend) Capture(name string, lines int) (string, error) {
	return b.tmux.Capture(name, lines)
}
func (b tmuxSessionBackend) Kill(name string) error { return b.tmux.Kill(name) }
func (b tmuxSessionBackend) AttachArgs(name string) []string {
	return []string{"tmux", "attach", "-t", name}
}

type ptySessionBackend struct {
	root string
}

func newPTYSessionBackend(configDir string) ptySessionBackend {
	return ptySessionBackend{root: filepath.Join(configDir, "pty-sessions")}
}

func (b ptySessionBackend) Name() string { return SessionBackendPTY }
func (b ptySessionBackend) TargetName(name string) string {
	return terminalSessionName(name)
}
func (b ptySessionBackend) manager() (*ducklion.Manager, error) {
	exe, err := findDucklionExecutable()
	if err != nil {
		return nil, err
	}
	return ducklion.NewManager(b.root, exe), nil
}
func (b ptySessionBackend) HasSession(name string) bool {
	m, err := b.manager()
	if err != nil {
		return false
	}
	records, err := m.List()
	if err != nil {
		return false
	}
	for _, rec := range records {
		if rec.Name == name && rec.Status == ducklion.StatusRunning {
			return true
		}
	}
	return false
}
func (b ptySessionBackend) Start(name, cwd, agentType string, command []string) error {
	m, err := b.manager()
	if err != nil {
		return err
	}
	_, err = m.Start(ducklion.StartOptions{Name: name, AgentType: agentType, Cwd: cwd, Command: command})
	return err
}
func (b ptySessionBackend) Send(name, text string) error {
	m, err := b.manager()
	if err != nil {
		return err
	}
	return m.Send(name, text)
}
func (b ptySessionBackend) Capture(name string, lines int) (string, error) {
	m, err := b.manager()
	if err != nil {
		return "", err
	}
	return m.Read(name, lines)
}
func (b ptySessionBackend) Kill(name string) error {
	m, err := b.manager()
	if err != nil {
		return err
	}
	return m.Stop(name)
}
func (b ptySessionBackend) AttachArgs(name string) []string {
	return []string{"ducklion", "attach", name}
}

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
