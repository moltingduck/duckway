package ducklord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
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
	ExitSuccess       *bool  `json:"exit_success,omitempty"`
	ExitReason        string `json:"exit_reason,omitempty"`
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
	Resize func(rows, cols uint16) error
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
				TaskState: string(summary.TaskState), AdapterState: string(summary.AdapterState), ExitSuccess: summary.ExitSuccess, ExitReason: summary.ExitReason}
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

func (r *Runner) Read(ctx context.Context, c Client, name string, lines int) (string, error) {
	if r != nil && r.hasOwner() {
		client, err := r.bridgeClient(ctx, c)
		if err != nil {
			return "", err
		}
		sessions, err := client.ListSessions()
		if err != nil {
			r.discardBridge(bridgeKey(c), client)
			return "", err
		}
		var selected *protocol.SessionSummary
		for i := range sessions {
			if sessions[i].SessionID == name {
				selected = &sessions[i]
				break
			}
			if sessions[i].Handle == name {
				if selected != nil {
					return "", fmt.Errorf("session handle %q is ambiguous; use its session ID", name)
				}
				selected = &sessions[i]
			}
		}
		if selected == nil {
			return "", fmt.Errorf("session %q not found", name)
		}
		stream, err := client.SubscribeOutputTail(selected.SessionID, selected.RuntimeGeneration, 1<<20)
		if err != nil {
			return "", err
		}
		defer stream.Close()
		metadata := stream.Metadata()
		var snapshot bytes.Buffer
		for uint64(snapshot.Len()) < metadata.EndOffset-metadata.StartOffset {
			event, readErr := stream.Read()
			if readErr != nil {
				return "", readErr
			}
			if event.Frame.Gap {
				return "", fmt.Errorf("session output snapshot has a gap")
			}
			snapshot.Write(event.Frame.Data)
		}
		text := snapshot.String()
		if lines > 0 {
			parts := strings.Split(text, "\n")
			if len(parts) > lines+1 {
				text = strings.Join(parts[len(parts)-lines-1:], "\n")
			}
		}
		return text, nil
	}
	if !SafeIdentifier(name) {
		return "", fmt.Errorf("invalid session name %q", name)
	}
	if lines <= 0 {
		lines = 120
	}
	out, err := sshOutput(ctx, c, "read", name, "--lines", strconv.Itoa(lines))
	return string(out), err
}

func (r *Runner) Send(ctx context.Context, c Client, name, text string) error {
	if r != nil && r.hasOwner() {
		client, err := r.bridgeClient(ctx, c)
		if err != nil {
			return err
		}
		sessions, err := client.ListSessions()
		if err != nil {
			r.discardBridge(bridgeKey(c), client)
			return err
		}
		var selected *protocol.SessionSummary
		for i := range sessions {
			if sessions[i].SessionID == name {
				selected = &sessions[i]
				break
			}
			if sessions[i].Handle == name {
				if selected != nil {
					return fmt.Errorf("session handle %q is ambiguous; use its session ID", name)
				}
				selected = &sessions[i]
			}
		}
		if selected == nil {
			return fmt.Errorf("session %q not found", name)
		}
		return client.SendInputContext(ctx, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, []byte(text+"\n"))
	}
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "send", name, text)
	return err
}

func (r *Runner) Start(ctx context.Context, c Client, args []string) error {
	if r != nil && r.hasOwner() {
		create, err := parseDaemonCreate(args)
		if err != nil {
			return err
		}
		operationID := uuid.NewString()
		var lastErr error
		for attempt := 0; attempt < 2; attempt++ {
			client, err := r.bridgeClient(ctx, c)
			if err != nil {
				lastErr = err
				continue
			}
			created, createErr := client.CreateSessionWithID(ctx, operationID, create)
			err = createErr
			if err == nil {
				if created.Status == model.StatusStopped {
					return fmt.Errorf("session %s stopped during launch: %s", created.SessionID, created.ExitReason)
				}
				return nil
			}
			lastErr = err
			var remoteErr *daemon.RemoteError
			if errors.As(err, &remoteErr) {
				return fmt.Errorf("create session on %s: %w", c.Name, err)
			}
			r.discardBridge(bridgeKey(c), client)
		}
		return fmt.Errorf("create session on %s has unknown outcome (operation %s): %w", c.Name, operationID, lastErr)
	}
	_, err := sshOutput(ctx, c, append([]string{"start"}, args...)...)
	return err
}

func parseDaemonCreate(args []string) (protocol.SessionCreate, error) {
	create := protocol.SessionCreate{Kind: model.KindAgent, AgentType: "shell", Rows: 40, Cols: 120}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			create.Command = append([]string(nil), args[i+1:]...)
			i = len(args)
		case "--name", "-n":
			if i+1 >= len(args) {
				return create, fmt.Errorf("--name requires a value")
			}
			create.Handle = args[i+1]
			i++
		case "--agent":
			if i+1 >= len(args) {
				return create, fmt.Errorf("--agent requires a value")
			}
			create.AgentType = args[i+1]
			i++
		case "--kind":
			if i+1 >= len(args) {
				return create, fmt.Errorf("--kind requires a value")
			}
			create.Kind = model.SessionKind(args[i+1])
			i++
			if create.Kind == model.KindShell {
				create.AgentType = ""
			}
		case "--cwd", "-C":
			if i+1 >= len(args) {
				return create, fmt.Errorf("--cwd requires a value")
			}
			create.CWD = args[i+1]
			i++
		default:
			return create, fmt.Errorf("unknown start option: %s", args[i])
		}
	}
	if create.Handle == "" || create.CWD == "" || len(create.Command) == 0 {
		return create, fmt.Errorf("--name, --cwd, and a command after -- are required")
	}
	return create, nil
}

func (r *Runner) Stop(ctx context.Context, c Client, name string) error {
	if r != nil && r.hasOwner() {
		client, err := r.bridgeClient(ctx, c)
		if err != nil {
			return err
		}
		selected, err := resolveSession(client, name)
		if err != nil {
			return err
		}
		operationID := uuid.NewString()
		for attempt := 0; attempt < 2; attempt++ {
			err = client.StopSessionWithID(ctx, operationID, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration)
			if err == nil {
				return nil
			}
			var remoteErr *daemon.RemoteError
			if errors.As(err, &remoteErr) {
				return err
			}
			r.discardBridge(bridgeKey(c), client)
			client, err = r.bridgeClient(ctx, c)
			if err != nil {
				continue
			}
		}
		return fmt.Errorf("stop session has unknown outcome (operation %s): %w", operationID, err)
	}
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "stop", name)
	return err
}

func (r *Runner) Yield(ctx context.Context, c Client, ref string, wait bool) (protocol.SessionYieldResult, error) {
	if r == nil || !r.hasOwner() {
		return protocol.SessionYieldResult{}, fmt.Errorf("ducklord owner is not configured")
	}
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return protocol.SessionYieldResult{}, err
	}
	selected, err := resolveSession(client, ref)
	if err != nil {
		return protocol.SessionYieldResult{}, err
	}
	operationID := uuid.NewString()
	for attempt := 0; attempt < 2; attempt++ {
		result, callErr := client.YieldSessionWithID(ctx, operationID, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, wait)
		if callErr == nil {
			return result, nil
		}
		err = callErr
		var remoteErr *daemon.RemoteError
		if errors.As(err, &remoteErr) {
			return protocol.SessionYieldResult{}, err
		}
		r.discardBridge(bridgeKey(c), client)
		client, err = r.bridgeClient(ctx, c)
		if err != nil {
			continue
		}
	}
	return protocol.SessionYieldResult{}, fmt.Errorf("yield session has unknown outcome (operation %s): %w", operationID, err)
}

func resolveSession(client *daemon.Client, ref string) (protocol.SessionSummary, error) {
	sessions, err := client.ListSessions()
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	var selected *protocol.SessionSummary
	for i := range sessions {
		if sessions[i].SessionID == ref {
			return sessions[i], nil
		}
		if sessions[i].Handle == ref {
			if selected != nil {
				return protocol.SessionSummary{}, fmt.Errorf("session handle %q is ambiguous; use its session ID", ref)
			}
			copy := sessions[i]
			selected = &copy
		}
	}
	if selected == nil {
		return protocol.SessionSummary{}, fmt.Errorf("session %q not found", ref)
	}
	return *selected, nil
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

func (r *Runner) AttachStream(ctx context.Context, c Client, sessionRef string) (*AttachSession, error) {
	if r != nil && r.hasOwner() {
		return r.attachDaemonStream(ctx, c, sessionRef)
	}
	if !SafeIdentifier(sessionRef) {
		return nil, fmt.Errorf("invalid session name %q", sessionRef)
	}
	args := SSHArgs(c, false, c.DucklionArgs("attach", sessionRef)...)
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

func (r *Runner) attachDaemonStream(ctx context.Context, c Client, sessionRef string) (*AttachSession, error) {
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return nil, err
	}
	sessions, err := client.ListSessions()
	if err != nil {
		r.discardBridge(bridgeKey(c), client)
		return nil, err
	}
	var selected *protocol.SessionSummary
	for i := range sessions {
		if sessions[i].SessionID == sessionRef {
			selected = &sessions[i]
			break
		}
		if sessions[i].Handle == sessionRef {
			if selected != nil {
				return nil, fmt.Errorf("session handle %q is ambiguous; use its session ID", sessionRef)
			}
			selected = &sessions[i]
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("session %q was not found", sessionRef)
	}
	subscription, err := client.SubscribeOutputTail(selected.SessionID, selected.RuntimeGeneration, 256<<10)
	if err != nil {
		return nil, err
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	attachCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	errResults := make(chan error, 2)
	go func() {
		defer outputWriter.Close()
		for {
			event, readErr := subscription.Read()
			if readErr != nil {
				var ended *daemon.OutputStreamEnded
				if errors.As(readErr, &ended) && ended.Reason == "runtime_disconnected" {
					readErr = nil
				}
				errResults <- readErr
				return
			}
			if _, writeErr := outputWriter.Write(event.Frame.Data); writeErr != nil {
				errResults <- writeErr
				return
			}
		}
	}()
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := inputReader.Read(buffer)
			if n > 0 {
				if sendErr := client.SendInputContext(attachCtx, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, buffer[:n]); sendErr != nil {
					errResults <- sendErr
					return
				}
			}
			if readErr != nil {
				errResults <- readErr
				return
			}
		}
	}()
	go func() {
		var finalErr error
		select {
		case finalErr = <-errResults:
			if errors.Is(finalErr, io.EOF) || errors.Is(finalErr, io.ErrClosedPipe) {
				finalErr = nil
			}
		case <-attachCtx.Done():
			finalErr = attachCtx.Err()
			if errors.Is(finalErr, context.Canceled) {
				finalErr = nil
			}
		}
		cancel()
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = subscription.Close()
		_ = outputWriter.Close()
		done <- finalErr
	}()
	resize := func(rows, cols uint16) error {
		return client.Resize(selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, rows, cols)
	}
	return &AttachSession{Stdin: inputWriter, Stdout: outputReader, Done: done, Resize: resize}, nil
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
