package ducklion

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	stateVersion = 1

	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusStale   = "stale"
)

type State struct {
	Version  int      `json:"version"`
	Sessions []Record `json:"sessions"`
}

type Record struct {
	Name      string `json:"name"`
	AgentType string `json:"agent_type"`
	Cwd       string `json:"cwd"`
	PID       int    `json:"pid"`
	Socket    string `json:"socket"`
	Log       string `json:"log"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Status    string `json:"status"`
}

type StartOptions struct {
	Name      string
	AgentType string
	Cwd       string
	Command   []string
}

type Manager struct {
	root string
	exe  string
}

func DefaultRoot() string {
	if d := strings.TrimSpace(os.Getenv("DUCKLION_ROOT")); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ducklion")
}

func NewManager(root, exe string) *Manager {
	if strings.TrimSpace(root) == "" {
		root = DefaultRoot()
	}
	return &Manager{root: root, exe: exe}
}

func (m *Manager) List() ([]Record, error) {
	unlock, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := m.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]Record, len(state.Sessions))
	copy(out, state.Sessions)
	changed := false
	for i := range out {
		if out[i].Status == StatusRunning && !processAlive(out[i].PID) {
			out[i].Status = StatusStale
			for j := range state.Sessions {
				if state.Sessions[j].Name == out[i].Name {
					state.Sessions[j].Status = StatusStale
					state.Sessions[j].UpdatedAt = now()
					changed = true
					break
				}
			}
		}
	}
	if changed {
		if err := m.saveLocked(state); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (m *Manager) Start(opts StartOptions) (*Record, error) {
	name, err := ValidateName(opts.Name)
	if err != nil {
		return nil, err
	}
	if opts.AgentType == "" {
		opts.AgentType = "shell"
	}
	cwd, err := validateCwd(opts.Cwd)
	if err != nil {
		return nil, err
	}
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("command is required")
	}
	exe := m.exe
	if exe == "" {
		exe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate ducklion executable: %w", err)
		}
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
	filtered := state.Sessions[:0]
	for _, rec := range state.Sessions {
		if rec.Name != name {
			filtered = append(filtered, rec)
			continue
		}
		if rec.Status == StatusRunning && processAlive(rec.PID) {
			return nil, fmt.Errorf("session %q is already running", name)
		}
	}
	sessionDir := filepath.Join(m.root, "sessions", name)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return nil, err
	}
	rec := Record{
		Name:      name,
		AgentType: opts.AgentType,
		Cwd:       cwd,
		Socket:    filepath.Join(sessionDir, "control.sock"),
		Log:       filepath.Join(sessionDir, "output.log"),
		CreatedAt: now(),
		UpdatedAt: now(),
		Status:    StatusRunning,
	}
	_ = os.Remove(rec.Socket)
	errFile, err := os.OpenFile(filepath.Join(sessionDir, "supervisor.err"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	defer errFile.Close()
	args := []string{"__supervise", "--name", rec.Name, "--agent", rec.AgentType, "--cwd", rec.Cwd, "--socket", rec.Socket, "--log", rec.Log, "--"}
	args = append(args, opts.Command...)
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = errFile
	cmd.Env = append(os.Environ(), "DUCKLION_SUPERVISE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ducklion supervisor: %w", err)
	}
	rec.PID = cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		killProcessGroup(rec.PID)
		return nil, fmt.Errorf("release ducklion supervisor: %w", err)
	}
	if err := waitForSocket(rec.Socket, 2*time.Second); err != nil {
		killProcessGroup(rec.PID)
		return nil, err
	}
	state.Sessions = append(filtered, rec)
	if err := m.saveLocked(state); err != nil {
		killProcessGroup(rec.PID)
		return nil, err
	}
	return &rec, nil
}

func (m *Manager) Read(name string, lines int) (string, error) {
	rec, err := m.lookup(name)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 120
	}
	return tailFile(rec.Log, lines)
}

func (m *Manager) Send(name, text string) error {
	rec, err := m.lookup(name)
	if err != nil {
		return err
	}
	return controlSend(rec.Socket, controlRequest{Op: "send", Text: text})
}

func (m *Manager) Attach(name string, in io.Reader, out io.Writer) error {
	rec, err := m.lookup(name)
	if err != nil {
		return err
	}
	conn, err := net.Dial("unix", rec.Socket)
	if err != nil {
		return fmt.Errorf("connect session socket: %w", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(controlRequest{Op: "attach"}); err != nil {
		return err
	}
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(out, conn)
		done <- err
	}()
	go func() {
		done <- copyAttachInput(conn, in)
	}()
	err = <-done
	if err == errDetach {
		return nil
	}
	return err
}

func (m *Manager) Stop(name string) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := m.loadLocked()
	if err != nil {
		return err
	}
	for i := range state.Sessions {
		if state.Sessions[i].Name != name {
			continue
		}
		_ = controlSend(state.Sessions[i].Socket, controlRequest{Op: "stop"})
		if processAlive(state.Sessions[i].PID) {
			killProcessGroup(state.Sessions[i].PID)
		}
		state.Sessions[i].Status = StatusStopped
		state.Sessions[i].UpdatedAt = now()
		return m.saveLocked(state)
	}
	return fmt.Errorf("unknown session %q", name)
}

func (m *Manager) lookup(name string) (Record, error) {
	name, err := ValidateName(name)
	if err != nil {
		return Record{}, err
	}
	records, err := m.List()
	if err != nil {
		return Record{}, err
	}
	for _, rec := range records {
		if rec.Name == name {
			if rec.Status != StatusRunning {
				return Record{}, fmt.Errorf("session %q is %s", name, rec.Status)
			}
			return rec, nil
		}
	}
	return Record{}, fmt.Errorf("unknown session %q", name)
}

func (m *Manager) lock() (func(), error) {
	if err := os.MkdirAll(m.root, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.root, "sessions.lock"), os.O_CREATE|os.O_RDWR, 0600)
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

func (m *Manager) loadLocked() (*State, error) {
	data, err := os.ReadFile(m.statePath())
	if os.IsNotExist(err) {
		return &State{Version: stateVersion, Sessions: []Record{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse ducklion state: %w", err)
	}
	if state.Version == 0 {
		state.Version = stateVersion
	}
	if state.Version != stateVersion {
		return nil, fmt.Errorf("unsupported ducklion state version %d", state.Version)
	}
	if state.Sessions == nil {
		state.Sessions = []Record{}
	}
	return &state, nil
}

func (m *Manager) saveLocked(state *State) error {
	if state.Version == 0 {
		state.Version = stateVersion
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, m.statePath())
}

func (m *Manager) statePath() string {
	return filepath.Join(m.root, "sessions.json")
}

func ValidateName(name string) (string, error) {
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

func validateCwd(cwd string) (string, error) {
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

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("ducklion supervisor socket did not become ready: %w", lastErr)
}

func tailFile(path string, lines int) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) <= lines {
		return string(data), nil
	}
	return strings.Join(parts[len(parts)-lines:], "\n"), nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

type SupervisorOptions struct {
	Name      string
	AgentType string
	Cwd       string
	Socket    string
	Log       string
	Command   []string
}

type controlRequest struct {
	Op   string `json:"op"`
	Text string `json:"text,omitempty"`
}

func RunSupervisor(opts SupervisorOptions) error {
	if _, err := ValidateName(opts.Name); err != nil {
		return err
	}
	cwd, err := validateCwd(opts.Cwd)
	if err != nil {
		return err
	}
	if len(opts.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Socket), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.Log), 0700); err != nil {
		return err
	}
	_ = os.Remove(opts.Socket)
	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
	cmd.Dir = cwd
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start pty command: %w", err)
	}
	defer func() { _ = ptmx.Close() }()

	logFile, err := os.OpenFile(opts.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	listener, err := net.Listen("unix", opts.Socket)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(opts.Socket)
	}()
	if err := os.Chmod(opts.Socket, 0600); err != nil {
		return err
	}

	done := make(chan error, 1)
	supervisor := &supervisor{pty: ptmx, proc: cmd.Process, log: logFile, listeners: map[net.Conn]bool{}}
	go supervisor.capture()
	go supervisor.accept(listener)
	go func() { done <- cmd.Wait() }()
	return <-done
}

type supervisor struct {
	mu        sync.Mutex
	pty       *os.File
	proc      *os.Process
	log       *os.File
	listeners map[net.Conn]bool
}

func (s *supervisor) capture() {
	buf := make([]byte, 4096)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			_, _ = s.log.Write(chunk)
			for conn := range s.listeners {
				_, _ = conn.Write(chunk)
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *supervisor) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *supervisor) handle(conn net.Conn) {
	defer conn.Close()
	var req controlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	switch req.Op {
	case "send":
		_, _ = s.pty.Write([]byte(req.Text))
		_, _ = s.pty.Write([]byte{'\r'})
	case "attach":
		s.mu.Lock()
		s.listeners[conn] = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.listeners, conn)
			s.mu.Unlock()
		}()
		_, _ = io.Copy(s.pty, conn)
	case "stop":
		if s.proc != nil {
			_ = syscall.Kill(-s.proc.Pid, syscall.SIGTERM)
		}
		_ = s.pty.Close()
	}
}

func controlSend(socket string, req controlRequest) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("connect session socket: %w", err)
	}
	defer conn.Close()
	return json.NewEncoder(conn).Encode(req)
}

var errDetach = fmt.Errorf("detach")

func copyAttachInput(dst io.Writer, src io.Reader) error {
	reader := bufio.NewReader(src)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if b == 0x1d {
			return errDetach
		}
		if _, err := dst.Write([]byte{b}); err != nil {
			return err
		}
	}
}

func ParseSupervisorArgs(args []string) (SupervisorOptions, error) {
	var opts SupervisorOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			opts.Command = append([]string(nil), args[i+1:]...)
			i = len(args)
		case "--name":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--name requires a value")
			}
			opts.Name = args[i+1]
			i++
		case "--agent":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--agent requires a value")
			}
			opts.AgentType = args[i+1]
			i++
		case "--cwd":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--cwd requires a value")
			}
			opts.Cwd = args[i+1]
			i++
		case "--socket":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--socket requires a value")
			}
			opts.Socket = args[i+1]
			i++
		case "--log":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--log requires a value")
			}
			opts.Log = args[i+1]
			i++
		default:
			return opts, fmt.Errorf("unknown supervisor option: %s", args[i])
		}
	}
	if opts.Name == "" || opts.Cwd == "" || opts.Socket == "" || opts.Log == "" {
		return opts, fmt.Errorf("missing supervisor option")
	}
	if len(opts.Command) == 0 {
		return opts, fmt.Errorf("command is required after --")
	}
	return opts, nil
}

func ParsePositiveInt(s string, max int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > max {
		return 0, fmt.Errorf("invalid integer")
	}
	return n, nil
}
