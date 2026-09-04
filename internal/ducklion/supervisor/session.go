package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	preparedTasks      map[string]preparedAgentTask
	committedTasks     map[string][32]byte
}

type preparedAgentTask struct {
	digest [32]byte
	prompt []byte
	owner  model.Owner
}

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
	cmd := exec.Command(options.Command[0], options.Command[1:]...)
	cmd.Dir = options.CWD
	cmd.Env = supervisedEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: options.Rows, Cols: options.Cols})
	if err != nil {
		return nil, fmt.Errorf("start supervised PTY: %w", err)
	}
	session := &Session{id: options.SessionID, generation: options.RuntimeGeneration, epoch: options.OwnershipEpoch,
		pty: ptmx, cmd: cmd, output: duckruntime.NewOutputHub(options.OutputCapacity), captureDone: make(chan struct{}),
		preparedTasks: make(map[string]preparedAgentTask), committedTasks: make(map[string][32]byte)}
	session.input = duckruntime.NewInputPump((*inputGate)(session), ptmx, 64)
	go session.capture()
	return session, nil
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
