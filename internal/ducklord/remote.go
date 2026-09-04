package ducklord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
)

type RemoteSession struct {
	Client            string `json:"client,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	Name              string `json:"name"`
	Kind              string `json:"kind,omitempty"`
	Status            string `json:"status"`
	AgentType         string `json:"agent_type"`
	Cwd               string `json:"cwd"`
	TmuxSession       string `json:"tmux_session"`
	LastLine          string `json:"last_line,omitempty"`
	TailHash          string `json:"tail_hash,omitempty"`
	Group             string `json:"group,omitempty"`
	Updated           bool   `json:"updated,omitempty"`
	Error             string `json:"error,omitempty"`
	WriterKind        string `json:"writer_kind,omitempty"`
	WriterID          string `json:"writer_id,omitempty"`
	OwnershipEpoch    uint64 `json:"ownership_epoch,omitempty"`
	RuntimeGeneration uint64 `json:"runtime_generation,omitempty"`
	TaskState         string `json:"task_state,omitempty"`
	AdapterState      string `json:"adapter_state,omitempty"`
}

type RemoteProject struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

type Runner struct {
	mu         sync.Mutex
	owner      string
	generation uint64
	bridges    map[string]*daemon.Client
	connectMu  map[string]*sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
}

func NewRunner() *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{bridges: make(map[string]*daemon.Client), connectMu: make(map[string]*sync.Mutex), ctx: ctx, cancel: cancel}
}

// SetOwner selects the Ducklord principal sent in every daemon handshake.
func (r *Runner) SetOwner(owner string) {
	r.mu.Lock()
	var stale []*daemon.Client
	if r.owner != owner {
		r.generation++
		for key, client := range r.bridges {
			stale = append(stale, client)
			delete(r.bridges, key)
		}
	}
	r.owner = owner
	r.mu.Unlock()
	for _, client := range stale {
		_ = client.Close()
	}
}

func (r *Runner) Close() error {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	clients := make([]*daemon.Client, 0, len(r.bridges))
	for key, client := range r.bridges {
		clients = append(clients, client)
		delete(r.bridges, key)
	}
	r.mu.Unlock()
	var closeErr error
	for _, client := range clients {
		if err := client.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (r *Runner) discardBridge(key string, client *daemon.Client) {
	r.mu.Lock()
	if r.bridges[key] == client {
		delete(r.bridges, key)
	}
	r.mu.Unlock()
	_ = client.Close()
}

func bridgeKey(c Client) string {
	return strings.Join([]string{c.Name, c.Host, c.User, c.SSH, c.Ducklion}, "\x00")
}

func (r *Runner) bridgeClient(ctx context.Context, c Client) (*daemon.Client, error) {
	key := bridgeKey(c)
	r.mu.Lock()
	if r.owner == "" {
		r.mu.Unlock()
		return nil, fmt.Errorf("ducklord owner name is not configured")
	}
	if client := r.bridges[key]; client != nil {
		r.mu.Unlock()
		return client, nil
	}
	owner := r.owner
	generation := r.generation
	runnerCtx := r.ctx
	keyMu := r.connectMu[key]
	if keyMu == nil {
		keyMu = &sync.Mutex{}
		r.connectMu[key] = keyMu
	}
	r.mu.Unlock()

	keyMu.Lock()
	defer keyMu.Unlock()
	r.mu.Lock()
	if client := r.bridges[key]; client != nil {
		r.mu.Unlock()
		return client, nil
	}
	if r.owner != owner || r.generation != generation {
		r.mu.Unlock()
		return nil, fmt.Errorf("ducklord owner changed while connecting")
	}
	r.mu.Unlock()
	args := SSHArgs(c, false, c.DucklionArgs("bridge", "--stdio")...)
	sshParts := c.SSHCommandParts()
	cmd := exec.CommandContext(runnerCtx, sshParts[0], append(sshParts[1:], args...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := &tailBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Ducklion bridge to %s: %w", c.Name, err)
	}
	stream := &commandStream{reader: stdout, writer: stdin, command: cmd, stderr: stderr}
	client, err := daemon.ConnectContext(ctx, stream, owner)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("connect Ducklion bridge to %s: %w", c.Name, err)
	}
	r.mu.Lock()
	if r.owner != owner || r.generation != generation {
		r.mu.Unlock()
		_ = client.Close()
		return nil, fmt.Errorf("ducklord owner changed while connecting")
	}
	r.bridges[key] = client
	r.mu.Unlock()
	return client, nil
}

func (r *Runner) hasOwner() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner != ""
}

type commandStream struct {
	reader  io.ReadCloser
	writer  io.WriteCloser
	command *exec.Cmd
	stderr  *tailBuffer
	once    sync.Once
}

type tailBuffer struct {
	bytes.Buffer
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	_, _ = b.Buffer.Write(p)
	if b.Len() > b.limit {
		data := append([]byte(nil), b.Bytes()[b.Len()-b.limit:]...)
		b.Reset()
		_, _ = b.Buffer.Write(data)
	}
	return n, nil
}

func (s *commandStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *commandStream) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *commandStream) Close() error {
	var closeErr error
	s.once.Do(func() {
		_ = s.writer.Close()
		_ = s.reader.Close()
		if s.command.Process != nil {
			_ = s.command.Process.Kill()
		}
		if err := s.command.Wait(); err != nil && s.stderr.Len() > 0 {
			closeErr = fmt.Errorf("ducklion bridge: %s", strings.TrimSpace(s.stderr.String()))
		}
	})
	return closeErr
}

type AttachSession struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Done   <-chan error
	cmd    *exec.Cmd
}

func (r *Runner) Sessions(ctx context.Context, c Client, tailLines int) ([]RemoteSession, error) {
	if r != nil && r.hasOwner() {
		client, err := r.bridgeClient(ctx, c)
		if err != nil {
			return nil, err
		}
		summaries, err := client.ListSessions()
		if err != nil {
			r.discardBridge(bridgeKey(c), client)
			return nil, err
		}
		sessions := make([]RemoteSession, 0, len(summaries))
		for _, summary := range summaries {
			session := RemoteSession{Client: c.Name, SessionID: summary.SessionID, Name: summary.Handle, Kind: string(summary.Kind), Status: string(summary.Status), AgentType: summary.AgentType,
				Cwd: summary.CWD, Group: c.Group, OwnershipEpoch: summary.OwnershipEpoch, RuntimeGeneration: summary.RuntimeGeneration,
				TaskState: string(summary.TaskState), AdapterState: string(summary.AdapterState)}
			if summary.Writer != nil {
				session.WriterKind = string(summary.Writer.Kind)
				session.WriterID = summary.Writer.ID
			}
			sessions = append(sessions, session)
		}
		return sessions, nil
	}
	if tailLines <= 0 {
		tailLines = 8
	}
	out, err := sshOutput(ctx, c, "list", "--json", "--tail-lines", strconv.Itoa(tailLines))
	if err != nil {
		return nil, err
	}
	var sessions []RemoteSession
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, fmt.Errorf("parse ducklion sessions from %s: %w", c.Name, err)
	}
	for i := range sessions {
		sessions[i].Client = c.Name
		sessions[i].Group = c.Group
	}
	return sessions, nil
}

func (*Runner) Read(ctx context.Context, c Client, name string, lines int) (string, error) {
	if !SafeIdentifier(name) {
		return "", fmt.Errorf("invalid session name %q", name)
	}
	if lines <= 0 {
		lines = 120
	}
	out, err := sshOutput(ctx, c, "read", name, "--lines", strconv.Itoa(lines))
	return string(out), err
}

func (*Runner) Send(ctx context.Context, c Client, name, text string) error {
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "send", name, text)
	return err
}

func (*Runner) Start(ctx context.Context, c Client, args []string) error {
	_, err := sshOutput(ctx, c, append([]string{"start"}, args...)...)
	return err
}

func (*Runner) Stop(ctx context.Context, c Client, name string) error {
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "stop", name)
	return err
}

func (*Runner) Projects(ctx context.Context, c Client) ([]RemoteProject, error) {
	out, err := sshOutput(ctx, c, "projects", "--json")
	if err != nil {
		return nil, err
	}
	var projects []RemoteProject
	if err := json.Unmarshal(out, &projects); err != nil {
		return nil, fmt.Errorf("parse ducklion projects from %s: %w", c.Name, err)
	}
	return projects, nil
}

func (*Runner) Attach(c Client, name string) error {
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	args := SSHArgs(c, true, c.DucklionArgs("attach", name)...)
	sshParts := c.SSHCommandParts()
	cmd := exec.Command(sshParts[0], append(sshParts[1:], args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh attach to %s: %w", c.Name, err)
	}
	return nil
}

func (*Runner) AttachStream(ctx context.Context, c Client, name string) (*AttachSession, error) {
	if !SafeIdentifier(name) {
		return nil, fmt.Errorf("invalid session name %q", name)
	}
	args := SSHArgs(c, false, c.DucklionArgs("attach", name)...)
	sshParts := c.SSHCommandParts()
	cmd := exec.CommandContext(ctx, sshParts[0], append(sshParts[1:], args...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh attach to %s: %w", c.Name, err)
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		done <- err
	}()
	return &AttachSession{Stdin: stdin, Stdout: stdout, Done: done, cmd: cmd}, nil
}

func sshOutput(ctx context.Context, c Client, ducklionArgs ...string) ([]byte, error) {
	return sshOutputRaw(ctx, c, c.DucklionArgs(ducklionArgs...)...)
}

func sshOutputRaw(ctx context.Context, c Client, remoteArgs ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := SSHArgs(c, false, remoteArgs...)
	sshParts := c.SSHCommandParts()
	cmd := exec.CommandContext(ctx, sshParts[0], append(sshParts[1:], args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("ssh to %s timed out", c.Name)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ssh to %s: %s", c.Name, msg)
	}
	return out, nil
}

func SSHArgs(c Client, tty bool, remoteArgs ...string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
	}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, c.Target(), remoteCommand(remoteArgs))
	return args
}

func remoteCommand(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&|;<>(){}[]*?!#~=") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
