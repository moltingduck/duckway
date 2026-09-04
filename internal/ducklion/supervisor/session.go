package supervisor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

var (
	ErrClosed             = errors.New("supervisor is closed")
	ErrPendingInput       = errors.New("PTY input remains pending")
	ErrInvalidPTYSize     = errors.New("invalid PTY size")
	ErrInputSequence      = errors.New("invalid input sequence")
	ErrInputIndeterminate = errors.New("PTY input state is indeterminate")
)

type Options struct {
	SessionID         model.SessionID
	RuntimeGeneration uint64
	OwnershipEpoch    uint64
	AgentType         string
	CWD               string
	Command           []string
	Rows              uint16
	Cols              uint16
	OutputCapacity    int
}

type Session struct {
	mu                 sync.Mutex
	agentMu            sync.Mutex
	id                 model.SessionID
	generation         uint64
	epoch              uint64
	pending            int
	lastSequence       uint64
	inputIndeterminate bool
	closed             bool
	pty                *os.File
	cmd                *exec.Cmd
	output             *duckruntime.OutputHub
	input              *duckruntime.InputPump
	captureDone        chan struct{}
	agentCaptureDone   chan struct{}
	adapterRead        *os.File
	preparedTasks      map[string]preparedAgentTask
	committedTasks     map[string][32]byte
	activeAgentTask    string
	agentEvents        map[string][]protocol.SupervisorAgentEvent
	agentEventAcks     map[string]uint64
	agentEventBytes    int
	agentEventCount    int
	agentEventNotify   chan struct{}
	agentAckOrder      []string
}

type preparedAgentTask struct {
	digest [32]byte
	prompt []byte
	owner  model.Owner
}

const (
	maxRetainedAgentEvents = 128
	maxRetainedAgentBytes  = 512 << 10
	maxAgentAckTombstones  = 1024
)

func Start(options Options) (*Session, error) {
	if _, err := model.ParseSessionID(string(options.SessionID)); err != nil {
		return nil, err
	}
	if options.RuntimeGeneration == 0 || len(options.Command) == 0 {
		return nil, fmt.Errorf("runtime generation and command are required")
	}
	if options.Rows == 0 {
		options.Rows = 40
	}
	if options.Cols == 0 {
		options.Cols = 120
	}
	if options.Rows < 5 || options.Rows > 200 || options.Cols < 20 || options.Cols > 500 {
		return nil, ErrInvalidPTYSize
	}
	command := append([]string(nil), options.Command...)
	if options.AgentType != "" {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			return nil, fmt.Errorf("locate Ducklion adapter hook: %w", executableErr)
		}
		switch strings.ToLower(options.AgentType) {
		case "codex":
			if filepath.Base(command[0]) == "codex" {
				notifyJSON, _ := json.Marshal([]string{executable, "__ducklion_agent_hook_v1"})
				command = append([]string{command[0], "-c", "notify=" + string(notifyJSON)}, command[1:]...)
			}
		case "claude", "claude_code":
			if filepath.Base(command[0]) == "claude" {
				hookCommand := shellQuote(executable) + " __ducklion_agent_hook_v1"
				hook := []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": hookCommand}}}}
				settings := map[string]any{"hooks": map[string]any{"Stop": hook, "StopFailure": hook}}
				settingsJSON, _ := json.Marshal(settings)
				command = append([]string{command[0], "--settings", string(settingsJSON)}, command[1:]...)
			}
		}
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = options.CWD
	cmd.Env = supervisedEnvironment()
	var adapterRead, adapterWrite *os.File
	if options.AgentType != "" {
		var pipeErr error
		adapterRead, adapterWrite, pipeErr = os.Pipe()
		if pipeErr != nil {
			return nil, fmt.Errorf("create agent adapter pipe: %w", pipeErr)
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, adapterWrite)
		cmd.Env = append(cmd.Env, "DUCKLION_AGENT_EVENT_FD=3")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: options.Rows, Cols: options.Cols})
	if err != nil {
		if adapterRead != nil {
			_ = adapterRead.Close()
			_ = adapterWrite.Close()
		}
		return nil, fmt.Errorf("start supervised PTY: %w", err)
	}
	if adapterWrite != nil {
		_ = adapterWrite.Close()
	}
	session := &Session{id: options.SessionID, generation: options.RuntimeGeneration, epoch: options.OwnershipEpoch,
		pty: ptmx, cmd: cmd, output: duckruntime.NewOutputHub(options.OutputCapacity), captureDone: make(chan struct{}),
		preparedTasks: make(map[string]preparedAgentTask), committedTasks: make(map[string][32]byte),
		agentEvents: make(map[string][]protocol.SupervisorAgentEvent)}
	session.agentEventAcks = make(map[string]uint64)
	session.agentEventNotify = make(chan struct{}, 1)
	session.input = duckruntime.NewInputPump((*inputGate)(session), ptmx, 64)
	go session.capture()
	if adapterRead != nil {
		session.adapterRead = adapterRead
		session.agentCaptureDone = make(chan struct{})
		go func() {
			defer close(session.agentCaptureDone)
			session.captureAgentEvents(adapterRead)
		}()
	}
	return session, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// QueueAgentEvent retains adapter output in the supervisor process until the
// CC confirms that it was delivered. This intentionally does not write the
// response to Ducklion's persistent state.
func (s *Session) QueueAgentEvent(event protocol.SupervisorAgentEvent) error {
	if !protocol.ValidTaskID(event.TaskID) || event.Sequence == 0 {
		return fmt.Errorf("invalid agent event")
	}
	if (event.Kind == "completed" || event.Kind == "failed") && event.OutputEnd == 0 && s.output != nil {
		_, event.OutputEnd = s.output.Bounds()
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agentEventAcks == nil {
		s.agentEventAcks = make(map[string]uint64)
	}
	if event.Sequence <= s.agentEventAcks[event.TaskID] {
		return nil
	}
	events := s.agentEvents[event.TaskID]
	eventBytes := len(event.Summary) + len(event.Response)
	if s.agentEventCount >= maxRetainedAgentEvents || s.agentEventBytes+eventBytes+len(event.TaskID)+64 > maxRetainedAgentBytes {
		return fmt.Errorf("agent event retention capacity reached")
	}
	if len(events) != 0 {
		last := events[len(events)-1]
		if event.Sequence <= last.Sequence {
			if event == last {
				return nil
			}
			return fmt.Errorf("agent event sequence is not contiguous")
		}
		if event.Sequence != last.Sequence+1 {
			return fmt.Errorf("agent event sequence is not contiguous")
		}
	} else if event.Sequence != 1 {
		return fmt.Errorf("agent event sequence must start at one")
	}
	s.agentEvents[event.TaskID] = append(events, event)
	s.agentEventBytes += eventBytes
	s.agentEventCount++
	if s.agentEventNotify != nil {
		select {
		case s.agentEventNotify <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *Session) PendingAgentEvents() []protocol.SupervisorAgentEvent {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agentEventAcks == nil {
		s.agentEventAcks = make(map[string]uint64)
	}
	var pending []protocol.SupervisorAgentEvent
	for _, events := range s.agentEvents {
		pending = append(pending, events...)
	}
	return pending
}

func (s *Session) AgentEventNotify() <-chan struct{} { return s.agentEventNotify }

func (s *Session) FailActiveAgentTask(summary string) bool {
	s.agentMu.Lock()
	s.mu.Lock()
	taskID := s.activeAgentTask
	s.mu.Unlock()
	events := s.agentEvents[taskID]
	sequence := uint64(len(events)) + s.agentEventAcks[taskID] + 1
	s.agentMu.Unlock()
	if taskID == "" {
		return false
	}
	if len(summary) > 500 {
		summary = summary[:500]
	}
	return s.QueueAgentEvent(protocol.SupervisorAgentEvent{TaskID: taskID, Sequence: sequence, Kind: "failed", Summary: summary}) == nil
}

func (s *Session) AckAgentEvent(taskID string, sequence uint64) error {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	events := s.agentEvents[taskID]
	if sequence <= s.agentEventAcks[taskID] {
		return nil
	}
	if len(events) == 0 || sequence > events[len(events)-1].Sequence {
		return fmt.Errorf("agent event is unavailable")
	}
	cut := 0
	for cut < len(events) && events[cut].Sequence <= sequence {
		s.agentEventBytes -= len(events[cut].Summary) + len(events[cut].Response)
		s.agentEventCount--
		events[cut].Summary = ""
		events[cut].Response = ""
		cut++
	}
	if cut == len(events) {
		delete(s.agentEvents, taskID)
	} else {
		s.agentEvents[taskID] = events[cut:]
	}
	if _, exists := s.agentEventAcks[taskID]; !exists {
		s.agentAckOrder = append(s.agentAckOrder, taskID)
	}
	s.agentEventAcks[taskID] = sequence
	for len(s.agentAckOrder) > maxAgentAckTombstones {
		oldest := s.agentAckOrder[0]
		s.agentAckOrder = s.agentAckOrder[1:]
		delete(s.agentEventAcks, oldest)
	}
	if cut > 0 && (events[cut-1].Kind == "completed" || events[cut-1].Kind == "failed") {
		s.mu.Lock()
		delete(s.committedTasks, taskID)
		s.mu.Unlock()
	}
	return nil
}

func (s *Session) PrepareAgentTask(taskID string, digest [32]byte, prompt []byte, owner model.Owner, epoch, generation uint64) error {
	if !protocol.ValidTaskID(taskID) || len(prompt) == 0 || len(prompt) > protocol.MaxAgentPromptBytes || sha256.Sum256(prompt) != digest || owner.Validate() != nil || owner.Kind != model.OwnerCC {
		return fmt.Errorf("invalid agent task")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if generation != s.generation {
		return model.ErrStaleGeneration
	}
	if epoch != s.epoch {
		return model.ErrStaleEpoch
	}
	if committed, ok := s.committedTasks[taskID]; ok {
		if committed != digest {
			return model.ErrTaskActive
		}
		return nil
	}
	if prepared, ok := s.preparedTasks[taskID]; ok {
		if prepared.digest != digest || prepared.owner != owner {
			return model.ErrTaskActive
		}
		return nil
	}
	if len(s.preparedTasks) != 0 {
		return model.ErrTaskActive
	}
	s.preparedTasks[taskID] = preparedAgentTask{digest: digest, prompt: append([]byte(nil), prompt...), owner: owner}
	return nil
}

func (s *Session) AgentTaskStatus(taskID string, digest [32]byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if committed, ok := s.committedTasks[taskID]; ok {
		if committed != digest {
			return "", model.ErrTaskActive
		}
		return "committed", nil
	}
	if prepared, ok := s.preparedTasks[taskID]; ok {
		if prepared.digest != digest {
			return "", model.ErrTaskActive
		}
		return "prepared", nil
	}
	return "absent", nil
}

func (s *Session) CommitAgentTask(ctx context.Context, taskID string, digest [32]byte, owner model.Owner, epoch, generation uint64) error {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.mu.Lock()
	if committed, ok := s.committedTasks[taskID]; ok {
		s.mu.Unlock()
		if committed != digest {
			return model.ErrTaskActive
		}
		return nil
	}
	prepared, ok := s.preparedTasks[taskID]
	if !ok || prepared.digest != digest || prepared.owner != owner {
		s.mu.Unlock()
		return fmt.Errorf("agent task was not prepared")
	}
	prompt := append([]byte(nil), prepared.prompt...)
	s.activeAgentTask = taskID
	s.mu.Unlock()
	// Bracketed paste preserves multiline prompts in native agent TUIs; the
	// trailing carriage return explicitly submits the prepared turn.
	data := make([]byte, 0, len(prompt)+16)
	data = append(data, "\x1b[200~"...)
	data = append(data, prompt...)
	data = append(data, "\x1b[201~\r"...)
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- s.SubmitInput(ctx, duckruntime.InputFrame{Sequence: s.nextAgentSequence(), Owner: owner, OwnershipEpoch: epoch, RuntimeGeneration: generation, Data: data})
	}()
	var err error
	select {
	case err = <-writeResult:
	case <-time.After(10 * time.Second):
		s.mu.Lock()
		s.inputIndeterminate = true
		s.mu.Unlock()
		_ = s.Terminate(true)
		err = ErrInputIndeterminate
	}
	for i := range data {
		data[i] = 0
	}
	for i := range prompt {
		prompt[i] = 0
	}
	if err != nil {
		s.mu.Lock()
		if s.activeAgentTask == taskID {
			s.activeAgentTask = ""
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	prepared = s.preparedTasks[taskID]
	for i := range prepared.prompt {
		prepared.prompt[i] = 0
	}
	delete(s.preparedTasks, taskID)
	s.committedTasks[taskID] = digest
	s.mu.Unlock()
	return nil
}

func (s *Session) captureAgentEvents(reader *os.File) {
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 4096)
	scanner.Buffer(buffer, 300<<10)
	for scanner.Scan() {
		var event protocol.SupervisorAgentEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		s.agentMu.Lock()
		if event.TaskID == "" {
			s.mu.Lock()
			event.TaskID = s.activeAgentTask
			s.mu.Unlock()
		}
		events := s.agentEvents[event.TaskID]
		if event.Sequence == 0 {
			event.Sequence = uint64(len(events)) + s.agentEventAcks[event.TaskID] + 1
		}
		s.agentMu.Unlock()
		if event.TaskID == "" {
			continue
		}
		if s.QueueAgentEvent(event) == nil && (event.Kind == "completed" || event.Kind == "failed") {
			s.mu.Lock()
			if s.activeAgentTask == event.TaskID {
				s.activeAgentTask = ""
			}
			s.mu.Unlock()
		}
	}
	if scanner.Err() != nil {
		s.agentMu.Lock()
		s.mu.Lock()
		taskID := s.activeAgentTask
		s.mu.Unlock()
		events := s.agentEvents[taskID]
		sequence := uint64(len(events)) + s.agentEventAcks[taskID] + 1
		s.agentMu.Unlock()
		if taskID != "" {
			_ = s.QueueAgentEvent(protocol.SupervisorAgentEvent{TaskID: taskID, Sequence: sequence, Kind: "failed", Summary: "Agent adapter event stream failed"})
		}
	}
}

func (s *Session) nextAgentSequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSequence + 1
}

func (s *Session) AbortAgentTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prepared, ok := s.preparedTasks[taskID]; ok {
		for i := range prepared.prompt {
			prepared.prompt[i] = 0
		}
		delete(s.preparedTasks, taskID)
	}
}

func supervisedEnvironment() []string {
	allowed := []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL", "USER", "LOGNAME", "SHELL", "TMPDIR", "COLORTERM"}
	environment := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok && !strings.ContainsRune(value, 0) {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func (s *Session) Output() *duckruntime.OutputHub { return s.output }

func (s *Session) SubmitInput(ctx context.Context, frame duckruntime.InputFrame) error {
	return s.input.Submit(ctx, frame)
}

func (s *Session) UpdateOwnership(epoch, generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if generation != s.generation {
		return model.ErrStaleGeneration
	}
	if s.pending != 0 {
		return ErrPendingInput
	}
	s.epoch = epoch
	return nil
}

func (s *Session) Resize(rows, cols uint16, epoch, generation uint64) error {
	if rows < 5 || rows > 200 || cols < 20 || cols > 500 {
		return ErrInvalidPTYSize
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if generation != s.generation {
		return model.ErrStaleGeneration
	}
	if epoch != s.epoch {
		return model.ErrStaleEpoch
	}
	return pty.Setsize(s.pty, &pty.Winsize{Rows: rows, Cols: cols})
}

func (s *Session) Wait() error {
	err := s.cmd.Wait()
	<-s.captureDone // capture drains the slave's final output before returning
	if s.adapterRead != nil {
		_ = s.adapterRead.Close()
		if s.agentCaptureDone != nil {
			<-s.agentCaptureDone
		}
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.input.Close()
	s.output.Close()
	_ = s.pty.Close()
	return err
}

// Terminate asks the entire supervised process group to exit. Wait must still
// be called exactly once to reap the child and finish PTY/output cleanup.
func (s *Session) Terminate(force bool) error {
	s.mu.Lock()
	if s.closed || s.cmd == nil || s.cmd.Process == nil {
		s.mu.Unlock()
		return nil
	}
	pid := s.cmd.Process.Pid
	s.mu.Unlock()
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if force {
		s.mu.Lock()
		if s.pty != nil {
			_ = s.pty.Close()
		}
		s.mu.Unlock()
	}
	return nil
}

func (s *Session) capture() {
	defer close(s.captureDone)
	buffer := make([]byte, 32<<10)
	for {
		n, err := s.pty.Read(buffer)
		if n > 0 {
			s.output.Publish(buffer[:n])
		}
		if err != nil {
			return
		}
	}
}

type inputGate Session

func (s *inputGate) ReserveInput(_ context.Context, frame duckruntime.InputFrame) error {
	session := (*Session)(s)
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return ErrClosed
	}
	if frame.RuntimeGeneration != session.generation {
		return model.ErrStaleGeneration
	}
	if frame.OwnershipEpoch != session.epoch {
		return model.ErrStaleEpoch
	}
	if session.inputIndeterminate {
		return ErrInputIndeterminate
	}
	if frame.Sequence == 0 || frame.Sequence != session.lastSequence+1 {
		return ErrInputSequence
	}
	session.lastSequence = frame.Sequence
	session.pending++
	return nil
}

func (s *inputGate) ValidateInput(_ context.Context, frame duckruntime.InputFrame) error {
	session := (*Session)(s)
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return ErrClosed
	}
	if frame.RuntimeGeneration != session.generation {
		return model.ErrStaleGeneration
	}
	if frame.OwnershipEpoch != session.epoch {
		return model.ErrStaleEpoch
	}
	return nil
}

func (s *inputGate) CompleteInput(_ context.Context, _ duckruntime.InputFrame, result error) {
	session := (*Session)(s)
	session.mu.Lock()
	var partial *duckruntime.PartialWriteError
	if errors.As(result, &partial) {
		session.inputIndeterminate = true
	}
	if session.pending > 0 {
		session.pending--
	}
	session.mu.Unlock()
}
