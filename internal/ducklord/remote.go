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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type RemoteSession struct {
	Client                string                                `json:"client,omitempty"`
	InstanceID            string                                `json:"instance_id,omitempty"`
	SessionID             string                                `json:"session_id,omitempty"`
	Name                  string                                `json:"name"`
	Kind                  string                                `json:"kind,omitempty"`
	Status                string                                `json:"status"`
	AgentType             string                                `json:"agent_type"`
	ProjectName           string                                `json:"project_name,omitempty"`
	Cwd                   string                                `json:"cwd"`
	TmuxSession           string                                `json:"tmux_session"`
	LastLine              string                                `json:"last_line,omitempty"`
	TailHash              string                                `json:"tail_hash,omitempty"`
	Group                 string                                `json:"group,omitempty"`
	Updated               bool                                  `json:"updated,omitempty"`
	Unread                bool                                  `json:"unread,omitempty"`
	Error                 string                                `json:"error,omitempty"`
	WriterKind            string                                `json:"writer_kind,omitempty"`
	WriterID              string                                `json:"writer_id,omitempty"`
	OwnershipEpoch        uint64                                `json:"ownership_epoch,omitempty"`
	RuntimeGeneration     uint64                                `json:"runtime_generation,omitempty"`
	TaskState             string                                `json:"task_state,omitempty"`
	AdapterState          string                                `json:"adapter_state,omitempty"`
	ExitSuccess           *bool                                 `json:"exit_success,omitempty"`
	ExitReason            string                                `json:"exit_reason,omitempty"`
	ActivitySequences     map[model.NotificationCategory]uint64 `json:"activity_sequences,omitempty"`
	RetainedOutputBytes   int64                                 `json:"retained_output_bytes,omitempty"`
	RetainedOutputUntilMS int64                                 `json:"retained_output_until_ms,omitempty"`
}

type RemoteProject struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

type RemoteDirectoryStatus struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Created bool   `json:"created,omitempty"`
}

type RemoteAgent struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
}

type SessionUpdate struct {
	Client           string
	InstanceID       string
	Revision         uint64
	Sessions         []RemoteSession
	State            string
	Gap              bool
	ChangedSessionID string
	Generation       uint64
	Err              error
}

type Runner struct {
	mu             sync.Mutex
	owner          string
	generation     uint64
	bridges        map[string]*daemon.Client
	connectMu      map[string]*sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	outputSlots    chan struct{}
	outputStarted  bool
	preview        *Runner
	processID      string
	connectionRole protocol.DucklordConnectionRole
}

func NewRunner() *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{bridges: make(map[string]*daemon.Client), connectMu: make(map[string]*sync.Mutex), ctx: ctx, cancel: cancel,
		processID: uuid.NewString(), connectionRole: protocol.ConnectionControl,
		outputSlots: make(chan struct{}, DefaultRawOutputSubscriptions)}
}

// SetOutputSubscriptionLimit configures the process-wide raw-output budget.
// It must be called before any subscriptions are opened.
func (r *Runner) SetOutputSubscriptionLimit(limit int) error {
	if limit < 1 || limit > 100 {
		return fmt.Errorf("raw output subscription limit must be between 1 and 100")
	}
	r.mu.Lock()
	if r.outputStarted {
		r.mu.Unlock()
		return fmt.Errorf("raw output subscription limit cannot change after subscriptions have started")
	}
	r.outputSlots = make(chan struct{}, limit)
	r.mu.Unlock()
	return nil
}

func (r *Runner) acquireOutputSlot(ctx context.Context) (func(), error) {
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return nil, io.ErrClosedPipe
	}
	r.outputStarted = true
	slots := r.outputSlots
	r.mu.Unlock()
	select {
	case slots <- struct{}{}:
		if r.ctx.Err() != nil {
			<-slots
			return nil, io.ErrClosedPipe
		}
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.ctx.Done():
		return nil, io.ErrClosedPipe
	}
}

// SetOwner selects the Ducklord principal sent in every daemon handshake.
func (r *Runner) SetOwner(owner string) {
	r.mu.Lock()
	var stale []*daemon.Client
	var stalePreview *Runner
	if r.owner != owner {
		r.generation++
		for key, client := range r.bridges {
			stale = append(stale, client)
			delete(r.bridges, key)
		}
		stalePreview = r.preview
		r.preview = nil
	}
	r.owner = owner
	r.mu.Unlock()
	for _, client := range stale {
		_ = client.Close()
	}
	if stalePreview != nil {
		_ = stalePreview.Close()
	}
}

func (r *Runner) Close() error {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	clients := make([]*daemon.Client, 0, len(r.bridges))
	preview := r.preview
	r.preview = nil
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
	if preview != nil {
		if err := preview.Close(); err != nil && closeErr == nil {
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
	client, err := daemon.ConnectDucklordContext(ctx, stream, owner, r.processID, uuid.NewString(), r.connectionRole)
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
	Stdin             io.WriteCloser
	Stdout            io.ReadCloser
	Done              <-chan error
	Resize            func(rows, cols uint16) error
	ResizeBarrier     func(rows, cols uint16) (uint64, error)
	RuntimeGeneration uint64
	StartOffset       uint64
	ReplayEndOffset   uint64
	ExactResume       bool
	OutputOffset      func() uint64
	cmd               *exec.Cmd
}

// ControlSession is the writer-only half of an interactive PTY. It captures
// immutable ownership and runtime fences and deliberately has no output
// reader, so focusing a pane does not consume a second output subscription.
type ControlSession struct {
	Stdin             io.WriteCloser
	Done              <-chan error
	ResizeBarrier     func(rows, cols uint16) (uint64, error)
	ClientKey         string
	InstanceID        string
	SessionID         string
	OwnershipEpoch    uint64
	RuntimeGeneration uint64
}

type AttachResume struct {
	RuntimeGeneration uint64
	OutputOffset      uint64
}

// OutputStream is the read-only half of a PTY attachment. It deliberately has
// no input or resize methods so background pool members cannot accidentally
// gain writer capabilities or couple their lifetime to UI focus.
type OutputStream struct {
	subscription      *daemon.OutputSubscription
	owner             *Runner
	closeOnce         sync.Once
	closeErr          error
	InstanceID        string
	SessionID         string
	RuntimeGeneration uint64
	StartOffset       uint64
	ReplayEndOffset   uint64
	ExactResume       bool
}

type OutputFrame struct {
	Data        []byte
	StartOffset uint64
	EndOffset   uint64
}

func (s *OutputStream) ReadContext(ctx context.Context) (OutputFrame, error) {
	if s == nil || s.subscription == nil {
		return OutputFrame{}, io.ErrClosedPipe
	}
	event, err := s.subscription.ReadContext(ctx)
	if err != nil {
		return OutputFrame{}, err
	}
	if event.Frame.Gap {
		return OutputFrame{}, fmt.Errorf("PTY output stream is not contiguous")
	}
	data := append([]byte(nil), event.Frame.Data...)
	return OutputFrame{Data: data, StartOffset: event.Frame.Offset, EndOffset: event.Frame.Offset + uint64(len(data))}, nil
}

func (s *OutputStream) Close() error {
	if s == nil || s.subscription == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.subscription.Close()
		if s.owner != nil {
			s.closeErr = errors.Join(s.closeErr, s.owner.Close())
		}
	})
	return s.closeErr
}

type subscriptionReader struct {
	subscription *daemon.OutputSubscription
	mu           sync.Mutex
	pending      []byte
	offset       uint64
	closed       bool
}

func (r *subscriptionReader) Read(buffer []byte) (int, error) {
	for {
		r.mu.Lock()
		if len(r.pending) > 0 {
			n := copy(buffer, r.pending)
			r.pending = r.pending[n:]
			r.offset += uint64(n)
			r.mu.Unlock()
			return n, nil
		}
		if r.closed {
			r.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		r.mu.Unlock()
		event, err := r.subscription.Read()
		if err != nil {
			var ended *daemon.OutputStreamEnded
			if errors.As(err, &ended) && ended.Reason == "runtime_disconnected" {
				return 0, io.EOF
			}
			return 0, err
		}
		r.mu.Lock()
		r.pending = append(r.pending[:0], event.Frame.Data...)
		r.mu.Unlock()
	}
}

func (r *subscriptionReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	return r.subscription.Close()
}

func (r *subscriptionReader) Offset() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.offset
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
			sessions = append(sessions, remoteSessionFromSummary(c, client.InstanceID(), summary))
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

// WatchSessionUpdates maintains one durable revision subscription for a host.
// Every delivered update contains a complete replacement snapshot so UI state
// never observes a partially applied sequence of invalidations.
func (r *Runner) WatchSessionUpdates(ctx context.Context, c Client) <-chan SessionUpdate {
	updates := make(chan SessionUpdate, 8)
	go func() {
		defer close(updates)
		var revision uint64
		var instanceID string
		var generation uint64
		for {
			client, err := r.bridgeClient(ctx, c)
			if err != nil {
				if !sendSessionUpdate(ctx, updates, SessionUpdate{Client: c.Name, InstanceID: instanceID, Revision: revision, Generation: generation, State: "reconnecting", Err: err}) || !waitContext(ctx, 500*time.Millisecond) {
					return
				}
				continue
			}
			if instanceID != "" && client.InstanceID() != instanceID {
				revision = 0
			}
			instanceID = client.InstanceID()
			generation++
			subscription, err := client.SubscribeSessionEvents(revision)
			if err != nil {
				r.discardBridge(bridgeKey(c), client)
				if !sendSessionUpdate(ctx, updates, SessionUpdate{Client: c.Name, InstanceID: instanceID, Revision: revision, Generation: generation, State: "reconnecting", Err: err}) || !waitContext(ctx, 500*time.Millisecond) {
					return
				}
				continue
			}
			metadata := subscription.Metadata()
			revision = metadata.SnapshotRevision
			sessions := remoteSessionsFromSummaries(c, metadata.InstanceID, metadata.Sessions)
			if !sendSessionUpdate(ctx, updates, SessionUpdate{Client: c.Name, InstanceID: metadata.InstanceID, Revision: revision, Generation: generation, Sessions: sessions, State: "live", Gap: metadata.Gap}) {
				_ = subscription.Close()
				return
			}
			for {
				event, readErr := subscription.Read()
				if readErr != nil {
					_ = subscription.Close()
					r.discardBridge(bridgeKey(c), client)
					_ = sendSessionUpdate(ctx, updates, SessionUpdate{Client: c.Name, InstanceID: metadata.InstanceID, Revision: revision, Generation: generation, State: "reconnecting", Err: readErr})
					break
				}
				summaries, listErr := client.ListSessions()
				if listErr != nil {
					_ = subscription.Close()
					r.discardBridge(bridgeKey(c), client)
					_ = sendSessionUpdate(ctx, updates, SessionUpdate{Client: c.Name, InstanceID: metadata.InstanceID, Revision: revision, Generation: generation, State: "reconnecting", Err: listErr})
					break
				}
				revision = event.Revision
				if !sendSessionUpdate(ctx, updates, SessionUpdate{Client: c.Name, InstanceID: metadata.InstanceID, Revision: revision,
					Generation: generation, Sessions: remoteSessionsFromSummaries(c, metadata.InstanceID, summaries), State: "live", ChangedSessionID: event.SessionID}) {
					_ = subscription.Close()
					return
				}
			}
		}
	}()
	return updates
}

func remoteSessionsFromSummaries(c Client, instanceID string, summaries []protocol.SessionSummary) []RemoteSession {
	sessions := make([]RemoteSession, 0, len(summaries))
	for _, summary := range summaries {
		sessions = append(sessions, remoteSessionFromSummary(c, instanceID, summary))
	}
	return sessions
}

func remoteSessionFromSummary(c Client, instanceID string, summary protocol.SessionSummary) RemoteSession {
	session := RemoteSession{Client: c.Name, InstanceID: instanceID, SessionID: summary.SessionID, Name: summary.Handle, Kind: string(summary.Kind), Status: string(summary.Status),
		AgentType: summary.AgentType, ProjectName: summary.ProjectName, Cwd: summary.CWD, Group: c.Group, OwnershipEpoch: summary.OwnershipEpoch, RuntimeGeneration: summary.RuntimeGeneration,
		TaskState: string(summary.TaskState), AdapterState: string(summary.AdapterState), ExitSuccess: summary.ExitSuccess, ExitReason: summary.ExitReason,
		ActivitySequences: cloneActivitySequences(summary.ActivitySequences), RetainedOutputBytes: summary.RetainedOutputBytes, RetainedOutputUntilMS: summary.RetainedOutputUntilMS}
	if summary.Writer != nil {
		session.WriterKind = string(summary.Writer.Kind)
		session.WriterID = summary.Writer.ID
	}
	return session
}

func cloneActivitySequences(source map[model.NotificationCategory]uint64) map[model.NotificationCategory]uint64 {
	if source == nil {
		return nil
	}
	cloned := make(map[model.NotificationCategory]uint64, len(source))
	for category, sequence := range source {
		cloned[category] = sequence
	}
	return cloned
}

func sendSessionUpdate(ctx context.Context, updates chan<- SessionUpdate, update SessionUpdate) bool {
	select {
	case updates <- update:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) Read(ctx context.Context, c Client, name string, lines int) (string, error) {
	if r != nil && r.hasOwner() {
		client, err := r.bridgeClient(ctx, c)
		if err != nil {
			return "", err
		}
		sessions, err := client.ListSessionsContext(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				r.discardBridge(bridgeKey(c), client)
			}
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
		release, slotErr := r.acquireOutputSlot(ctx)
		if slotErr != nil {
			return "", slotErr
		}
		defer release()
		stream, err := client.SubscribeOutputTailContext(ctx, selected.SessionID, selected.RuntimeGeneration, 1<<20)
		if err != nil {
			return "", err
		}
		defer stream.Close()
		metadata := stream.Metadata()
		var snapshot bytes.Buffer
		for uint64(snapshot.Len()) < metadata.EndOffset-metadata.StartOffset {
			event, readErr := stream.ReadContext(ctx)
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

// ReadPreview isolates short-lived, cancellable preview snapshots from the
// shared multiplex bridge used by session watchers and live attachments.
// daemon.Client cancellation is deliberately fail-closed at the connection
// level, so a preview timeout must only close its own bridge.
func (r *Runner) ReadPreview(ctx context.Context, c Client, name string, lines int) (string, error) {
	if r == nil || !r.hasOwner() {
		return r.Read(ctx, c, name, lines)
	}
	r.mu.Lock()
	preview := r.preview
	if preview == nil {
		preview = NewRunner()
		preview.processID = r.processID
		preview.connectionRole = protocol.ConnectionObserver
		preview.SetOwner(r.owner)
		r.preview = preview
	}
	r.mu.Unlock()
	text, err := preview.Read(ctx, c, name, lines)
	if err != nil && ctx.Err() != nil {
		r.mu.Lock()
		if r.preview == preview {
			r.preview = nil
		}
		r.mu.Unlock()
		_ = preview.Close()
	}
	return text, err
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
		// Interactive PTYs expect the Enter key as carriage return. A line feed
		// is only output-side newline data in raw-mode TUIs such as Codex and
		// Claude Code, so it can leave prompts visibly typed but unsubmitted.
		return client.SendInputContext(ctx, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, terminalSubmitLine(text))
	}
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "send", name, text)
	return err
}

func terminalSubmitLine(text string) []byte {
	return []byte(text + "\r")
}

func (r *Runner) Start(ctx context.Context, c Client, args []string) (string, error) {
	if r != nil && r.hasOwner() {
		create, err := parseDaemonCreate(args)
		if err != nil {
			return "", err
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
					return "", fmt.Errorf("session %s stopped during launch: %s", created.SessionID, created.ExitReason)
				}
				return created.SessionID, nil
			}
			lastErr = err
			var remoteErr *daemon.RemoteError
			if errors.As(err, &remoteErr) {
				return "", fmt.Errorf("create session on %s: %w", c.Name, err)
			}
			r.discardBridge(bridgeKey(c), client)
		}
		return "", fmt.Errorf("create session on %s has unknown outcome (operation %s): %w", c.Name, operationID, lastErr)
	}
	_, err := sshOutput(ctx, c, append([]string{"start"}, args...)...)
	return "", err
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
		case "--project-name":
			if i+1 >= len(args) {
				return create, fmt.Errorf("--project-name requires a value")
			}
			create.ProjectName = args[i+1]
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
		_, err := r.Lifecycle(ctx, c, name, protocol.SessionLifecycleEnd, protocol.SessionLifecycleImmediate)
		return err
	}
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "stop", name)
	return err
}

// Lifecycle executes one durable session mutation and follows its immutable
// receipt across SSH/bridge reconnects until Ducklion reports completion.
func (r *Runner) Lifecycle(ctx context.Context, c Client, ref string, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error) {
	if r == nil || !r.hasOwner() {
		return protocol.SessionLifecycleResult{}, fmt.Errorf("ducklord owner is not configured")
	}
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return protocol.SessionLifecycleResult{}, err
	}
	selected, err := resolveSession(client, ref)
	if err != nil {
		return protocol.SessionLifecycleResult{}, err
	}
	return r.lifecycleSelected(ctx, c, client, client.InstanceID(), selected, operation, mode)
}

// LifecycleSelected executes a lifecycle mutation against the exact session
// revision shown to the user. Unlike Lifecycle it deliberately does not
// resolve a fresh session by handle or ID, so a confirmation cannot silently
// advance to a newer ownership epoch or runtime generation.
func (r *Runner) LifecycleSelected(ctx context.Context, c Client, selected RemoteSession, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error) {
	if r == nil || !r.hasOwner() {
		return protocol.SessionLifecycleResult{}, fmt.Errorf("ducklord owner is not configured")
	}
	if selected.SessionID == "" || selected.Client != c.Name {
		return protocol.SessionLifecycleResult{}, fmt.Errorf("invalid selected session identity")
	}
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return protocol.SessionLifecycleResult{}, err
	}
	summary := protocol.SessionSummary{
		SessionID: selected.SessionID, Kind: model.SessionKind(selected.Kind),
		OwnershipEpoch: selected.OwnershipEpoch, RuntimeGeneration: selected.RuntimeGeneration,
	}
	return r.lifecycleSelected(ctx, c, client, selected.InstanceID, summary, operation, mode)
}

func (r *Runner) lifecycleSelected(ctx context.Context, c Client, client *daemon.Client, expectedInstance string, selected protocol.SessionSummary, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error) {
	mode, err := normalizeLifecycleMode(selected.Kind, operation, mode)
	if err != nil {
		return protocol.SessionLifecycleResult{}, err
	}
	requestID := uuid.NewString()
	request := protocol.SessionLifecycleRequest{Operation: operation, Mode: mode}
	for {
		if expectedInstance == "" || client.InstanceID() != expectedInstance {
			return protocol.SessionLifecycleResult{}, fmt.Errorf("host instance changed; reopen the lifecycle action")
		}
		result, callErr := client.LifecycleSessionWithID(ctx, requestID, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, request)
		if callErr == nil {
			if result.State == protocol.SessionLifecycleCompleted {
				return result, nil
			}
			select {
			case <-ctx.Done():
				return result, fmt.Errorf("stopped waiting; lifecycle request %s remains durable: %w", requestID, ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		var remoteErr *daemon.RemoteError
		if errors.As(callErr, &remoteErr) {
			return protocol.SessionLifecycleResult{}, callErr
		}
		r.discardBridge(bridgeKey(c), client)
		client, callErr = r.bridgeClient(ctx, c)
		if callErr != nil {
			err = callErr
			select {
			case <-ctx.Done():
				return protocol.SessionLifecycleResult{}, fmt.Errorf("session lifecycle outcome unknown (request %s): %w", requestID, err)
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
	}
}

func normalizeLifecycleMode(kind model.SessionKind, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode) (protocol.SessionLifecycleMode, error) {
	if kind != model.KindShell {
		return mode, nil
	}
	if operation == protocol.SessionLifecycleRestart && mode == protocol.SessionLifecycleWait {
		// Restart defaults to wait for agent sessions. Native shell sessions
		// have no task-idle signal, so the CLI default means immediate there.
		return protocol.SessionLifecycleImmediate, nil
	}
	if mode != protocol.SessionLifecycleImmediate {
		return "", fmt.Errorf("shell lifecycle operations are immediate and do not accept wait or force modes")
	}
	return mode, nil
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
	return r.yieldSelected(ctx, c, client, client.InstanceID(), selected, wait)
}

// YieldSelected transfers ownership only for the exact instance/session
// revision displayed by Ducklord's action menu.
func (r *Runner) YieldSelected(ctx context.Context, c Client, selected RemoteSession, wait bool) (protocol.SessionYieldResult, error) {
	if r == nil || !r.hasOwner() {
		return protocol.SessionYieldResult{}, fmt.Errorf("ducklord owner is not configured")
	}
	if selected.InstanceID == "" || selected.SessionID == "" || selected.Client != c.Name {
		return protocol.SessionYieldResult{}, fmt.Errorf("invalid selected session identity")
	}
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return protocol.SessionYieldResult{}, err
	}
	summary := protocol.SessionSummary{SessionID: selected.SessionID, Kind: model.SessionKind(selected.Kind),
		OwnershipEpoch: selected.OwnershipEpoch, RuntimeGeneration: selected.RuntimeGeneration}
	return r.yieldSelected(ctx, c, client, selected.InstanceID, summary, wait)
}

func (r *Runner) yieldSelected(ctx context.Context, c Client, client *daemon.Client, expectedInstance string, selected protocol.SessionSummary, wait bool) (protocol.SessionYieldResult, error) {
	operationID := uuid.NewString()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if expectedInstance == "" || client.InstanceID() != expectedInstance {
			return protocol.SessionYieldResult{}, fmt.Errorf("host instance changed; reopen the yield action")
		}
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

func (*Runner) SuggestProjectPaths(ctx context.Context, c Client, query string) ([]string, error) {
	if err := validateRemoteText("path query", query, 4096, false); err != nil {
		return nil, err
	}
	out, err := sshOutput(ctx, c, "projects", "--suggest", query, "--json")
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(out, &paths); err != nil {
		return nil, fmt.Errorf("parse Ducklion path suggestions from %s: %w", c.Name, err)
	}
	if len(paths) > 20 {
		return nil, fmt.Errorf("ducklion returned too many path suggestions")
	}
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if err := validateRemoteText("suggested path", path, 4096, true); err != nil {
			return nil, err
		}
		if seen[path] {
			return nil, fmt.Errorf("ducklion returned duplicate path suggestion")
		}
		seen[path] = true
	}
	return paths, nil
}

func (*Runner) EnsureDirectory(ctx context.Context, c Client, path string, create bool) (RemoteDirectoryStatus, error) {
	if err := validateRemoteText("directory path", path, 4096, true); err != nil {
		return RemoteDirectoryStatus{}, err
	}
	requested := filepath.Clean(path)
	args := []string{"projects", "--inspect-dir", path, "--json"}
	if create {
		args = []string{"projects", "--create-dir", path, "--json"}
	}
	out, err := sshOutput(ctx, c, args...)
	if err != nil {
		return RemoteDirectoryStatus{}, err
	}
	var status RemoteDirectoryStatus
	if json.Unmarshal(out, &status) != nil || status.Path == "" {
		return RemoteDirectoryStatus{}, fmt.Errorf("parse Ducklion directory status from %s", c.Name)
	}
	if err := validateRemoteText("directory path", status.Path, 4096, true); err != nil {
		return RemoteDirectoryStatus{}, err
	}
	if filepath.Clean(status.Path) != requested {
		return RemoteDirectoryStatus{}, fmt.Errorf("ducklion directory status path mismatch from %s", c.Name)
	}
	if status.Created && !status.Exists {
		return RemoteDirectoryStatus{}, fmt.Errorf("ducklion returned an invalid directory status from %s", c.Name)
	}
	if !create && status.Created {
		return RemoteDirectoryStatus{}, fmt.Errorf("ducklion inspect unexpectedly reported a created directory from %s", c.Name)
	}
	return status, nil
}

func (*Runner) AddProject(ctx context.Context, c Client, path, name string) (RemoteProject, error) {
	if err := validateRemoteText("project path", path, 4096, true); err != nil {
		return RemoteProject{}, err
	}
	if name != "" {
		if err := validateRemoteText("project name", name, 256, false); err != nil {
			return RemoteProject{}, err
		}
	}
	args := []string{"projects", "--add", path, "--json"}
	if name != "" {
		args = append(args, "--name", name)
	}
	out, err := sshOutput(ctx, c, args...)
	if err != nil {
		return RemoteProject{}, err
	}
	var projects []RemoteProject
	if err := json.Unmarshal(out, &projects); err != nil || len(projects) != 1 {
		return RemoteProject{}, fmt.Errorf("parse added Ducklion project from %s", c.Name)
	}
	if err := validateRemoteText("added project path", projects[0].Path, 4096, true); err != nil {
		return RemoteProject{}, err
	}
	if err := validateRemoteText("added project name", projects[0].Name, 256, false); err != nil || strings.TrimSpace(projects[0].Name) == "" {
		return RemoteProject{}, fmt.Errorf("invalid added project returned by %s", c.Name)
	}
	projects[0].Source = "duckway-client"
	return projects[0], nil
}

func validateRemoteText(field, value string, maxBytes int, absolute bool) error {
	if strings.TrimSpace(value) == "" || len(value) > maxBytes {
		return fmt.Errorf("invalid %s", field)
	}
	if absolute && !strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s must be absolute", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return fmt.Errorf("invalid %s", field)
		}
	}
	return nil
}

func (*Runner) Agents(ctx context.Context, c Client, cwd string) ([]RemoteAgent, error) {
	out, err := sshOutput(ctx, c, "agents", "--cwd", cwd, "--json")
	if err != nil {
		return nil, err
	}
	var agents []RemoteAgent
	if err := json.Unmarshal(out, &agents); err != nil {
		return nil, fmt.Errorf("parse ducklion agents from %s: %w", c.Name, err)
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("ducklion reported no available agent types for %s", cwd)
	}
	if len(agents) > 16 {
		return nil, fmt.Errorf("ducklion reported too many agent types for %s", cwd)
	}
	seen := make(map[string]bool, len(agents))
	for _, agent := range agents {
		switch agent.Type {
		case "shell", "zsh", "bash", "sh", "codex", "claude_code":
		default:
			return nil, fmt.Errorf("ducklion reported unsupported agent type %q", agent.Type)
		}
		if seen[agent.Type] || len(agent.Command) == 0 || len(agent.Command) > 16 {
			return nil, fmt.Errorf("ducklion reported invalid agent command for %q", agent.Type)
		}
		seen[agent.Type] = true
		for _, arg := range agent.Command {
			if err := validateRemoteText("agent command", arg, 4096, false); err != nil {
				return nil, err
			}
		}
	}
	return agents, nil
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
		return r.attachDaemonStream(ctx, c, sessionRef, nil)
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

func (r *Runner) AttachStreamFrom(ctx context.Context, c Client, sessionRef string, resume AttachResume) (*AttachSession, error) {
	if r == nil || !r.hasOwner() {
		return r.AttachStream(ctx, c, sessionRef)
	}
	return r.attachDaemonStream(ctx, c, sessionRef, &resume)
}

// OpenControlSession opens only input and resize capabilities for the exact
// session revision returned by Ducklion. Read-only agent viewers never call
// this method and therefore never receive a writer-capable object.
func (r *Runner) OpenControlSession(ctx context.Context, c Client, sessionRef string) (*ControlSession, error) {
	if r == nil || !r.hasOwner() {
		return nil, fmt.Errorf("PTY control requires the Ducklion bridge")
	}
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return nil, err
	}
	selected, err := resolveSessionSummary(ctx, client, sessionRef)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	owner := r.owner
	r.mu.Unlock()
	if selected.Kind != model.KindShell && (selected.Writer == nil || selected.Writer.Kind != model.OwnerTerminal || selected.Writer.ID != owner) {
		return nil, fmt.Errorf("read-only session; yield control to this Ducklord before sending input")
	}
	controlCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer cancel()
		defer reader.Close()
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := reader.Read(buffer)
			if n > 0 {
				if sendErr := client.SendInputContext(controlCtx, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, buffer[:n]); sendErr != nil {
					done <- sendErr
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrClosedPipe) {
					readErr = nil
				}
				done <- readErr
				return
			}
		}
	}()
	resize := func(rows, cols uint16) (uint64, error) {
		result, resizeErr := client.ResizeWithBarrierContext(controlCtx, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, rows, cols)
		return result.OutputOffset, resizeErr
	}
	return &ControlSession{Stdin: writer, Done: done, ResizeBarrier: resize, ClientKey: c.Name, InstanceID: client.InstanceID(), SessionID: selected.SessionID,
		OwnershipEpoch: selected.OwnershipEpoch, RuntimeGeneration: selected.RuntimeGeneration}, nil
}

// OpenOutputStream opens only the read side of a remote PTY. Unlike
// AttachStream it never obtains an input pipe, ownership epoch, or resize
// capability, so it is safe for background framebuffer subscriptions.
func (r *Runner) OpenOutputStream(ctx context.Context, c Client, sessionRef string) (*OutputStream, error) {
	return r.openOutputStream(ctx, c, sessionRef, nil)
}

func (r *Runner) OpenOutputStreamFrom(ctx context.Context, c Client, sessionRef string, resume AttachResume) (*OutputStream, error) {
	return r.openOutputStream(ctx, c, sessionRef, &resume)
}

func (r *Runner) openOutputStream(ctx context.Context, c Client, sessionRef string, resume *AttachResume) (*OutputStream, error) {
	if r == nil || !r.hasOwner() {
		return nil, fmt.Errorf("read-only PTY output requires the Ducklion bridge")
	}
	if r.connectionRole != protocol.ConnectionObserver {
		r.mu.Lock()
		owner, processID := r.owner, r.processID
		r.mu.Unlock()
		isolated := NewRunner()
		isolated.processID = processID
		isolated.connectionRole = protocol.ConnectionObserver
		isolated.SetOwner(owner)
		stream, err := isolated.openOutputStream(ctx, c, sessionRef, resume)
		if err != nil {
			_ = isolated.Close()
			return nil, err
		}
		stream.owner = isolated
		return stream, nil
	}
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return nil, err
	}
	selected, err := resolveSessionSummary(ctx, client, sessionRef)
	if err != nil {
		return nil, err
	}
	var subscription *daemon.OutputSubscription
	if resume != nil && resume.RuntimeGeneration == selected.RuntimeGeneration {
		subscription, err = client.SubscribeOutputContext(ctx, selected.SessionID, selected.RuntimeGeneration, resume.OutputOffset)
	} else {
		subscription, err = client.SubscribeOutputTailContext(ctx, selected.SessionID, selected.RuntimeGeneration, 256<<10)
	}
	if err != nil {
		return nil, err
	}
	metadata := subscription.Metadata()
	exact := resume != nil && resume.RuntimeGeneration == selected.RuntimeGeneration && !metadata.Gap && metadata.StartOffset == resume.OutputOffset
	return &OutputStream{subscription: subscription, InstanceID: metadata.InstanceID, SessionID: metadata.SessionID,
		RuntimeGeneration: metadata.RuntimeGeneration, StartOffset: metadata.StartOffset, ReplayEndOffset: metadata.EndOffset, ExactResume: exact}, nil
}

func resolveSessionSummary(ctx context.Context, client *daemon.Client, sessionRef string) (*protocol.SessionSummary, error) {
	sessions, err := client.ListSessionsContext(ctx)
	if err != nil {
		return nil, err
	}
	var selected *protocol.SessionSummary
	for i := range sessions {
		if sessions[i].SessionID == sessionRef {
			copy := sessions[i]
			return &copy, nil
		}
		if sessions[i].Handle == sessionRef {
			if selected != nil {
				return nil, fmt.Errorf("session handle %q is ambiguous; use its session ID", sessionRef)
			}
			copy := sessions[i]
			selected = &copy
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("session %q was not found", sessionRef)
	}
	return selected, nil
}

func (r *Runner) attachDaemonStream(ctx context.Context, c Client, sessionRef string, resume *AttachResume) (*AttachSession, error) {
	client, err := r.bridgeClient(ctx, c)
	if err != nil {
		return nil, err
	}
	selected, err := resolveSessionSummary(ctx, client, sessionRef)
	if err != nil {
		return nil, err
	}
	releaseSlot, err := r.acquireOutputSlot(ctx)
	if err != nil {
		return nil, err
	}
	var subscription *daemon.OutputSubscription
	if resume != nil && resume.RuntimeGeneration == selected.RuntimeGeneration {
		subscription, err = client.SubscribeOutput(selected.SessionID, selected.RuntimeGeneration, resume.OutputOffset)
	} else {
		subscription, err = client.SubscribeOutputTail(selected.SessionID, selected.RuntimeGeneration, 256<<10)
	}
	if err != nil {
		releaseSlot()
		return nil, err
	}
	metadata := subscription.Metadata()
	exactResume := resume != nil && resume.RuntimeGeneration == selected.RuntimeGeneration && !metadata.Gap && metadata.StartOffset == resume.OutputOffset
	outputReader := &subscriptionReader{subscription: subscription, offset: metadata.StartOffset}
	inputReader, inputWriter := io.Pipe()
	attachCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	errResults := make(chan error, 1)
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
		defer releaseSlot()
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
		_ = outputReader.Close()
		done <- finalErr
	}()
	resize := func(rows, cols uint16) error {
		return client.Resize(selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, rows, cols)
	}
	resizeBarrier := func(rows, cols uint16) (uint64, error) {
		result, resizeErr := client.ResizeWithBarrierContext(attachCtx, selected.SessionID, selected.OwnershipEpoch, selected.RuntimeGeneration, rows, cols)
		return result.OutputOffset, resizeErr
	}
	return &AttachSession{Stdin: inputWriter, Stdout: outputReader, Done: done, Resize: resize, RuntimeGeneration: selected.RuntimeGeneration,
		StartOffset: metadata.StartOffset, ReplayEndOffset: metadata.EndOffset, ExactResume: exactResume, OutputOffset: outputReader.Offset, ResizeBarrier: resizeBarrier}, nil
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
