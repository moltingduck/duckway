package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

var (
	ErrClosed         = errors.New("supervisor is closed")
	ErrPendingInput   = errors.New("PTY input remains pending")
	ErrInvalidPTYSize = errors.New("invalid PTY size")
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
	mu          sync.Mutex
	id          model.SessionID
	generation  uint64
	epoch       uint64
	pending     int
	closed      bool
	pty         *os.File
	cmd         *exec.Cmd
	output      *duckruntime.OutputHub
	input       *duckruntime.InputPump
	captureDone chan struct{}
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
	cmd := exec.Command(options.Command[0], options.Command[1:]...)
	cmd.Dir = options.CWD
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: options.Rows, Cols: options.Cols})
	if err != nil {
		return nil, fmt.Errorf("start supervised PTY: %w", err)
	}
	session := &Session{id: options.SessionID, generation: options.RuntimeGeneration, epoch: options.OwnershipEpoch,
		pty: ptmx, cmd: cmd, output: duckruntime.NewOutputHub(options.OutputCapacity), captureDone: make(chan struct{})}
	session.input = duckruntime.NewInputPump((*inputGate)(session), ptmx, 64)
	go session.capture()
	return session, nil
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
	if rows == 0 || cols == 0 {
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

func (s *inputGate) CompleteInput(_ context.Context, _ duckruntime.InputFrame, _ error) {
	session := (*Session)(s)
	session.mu.Lock()
	if session.pending > 0 {
		session.pending--
	}
	session.mu.Unlock()
}
