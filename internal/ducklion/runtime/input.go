package runtime

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

var ErrInputPumpClosed = errors.New("input pump is closed")

type InputFrame struct {
	Sequence          uint64
	Principal         string
	Owner             model.Owner
	OwnershipEpoch    uint64
	RuntimeGeneration uint64
	Data              []byte
}

// InputGate reserves authoritative pending-input state before queueing bytes,
// revalidates immediately before the PTY write, and releases after completion.
type InputGate interface {
	ReserveInput(context.Context, InputFrame) error
	ValidateInput(context.Context, InputFrame) error
	CompleteInput(context.Context, InputFrame, error)
}

type inputRequest struct {
	ctx    context.Context
	frame  InputFrame
	result chan error
}

type InputPump struct {
	gate   InputGate
	writer io.Writer
	queue  chan inputRequest
	done   chan struct{}
	once   sync.Once
}

func NewInputPump(gate InputGate, writer io.Writer, queueSize int) *InputPump {
	if queueSize <= 0 {
		queueSize = 64
	}
	p := &InputPump{gate: gate, writer: writer, queue: make(chan inputRequest, queueSize), done: make(chan struct{})}
	go p.run()
	return p
}

func (p *InputPump) Submit(ctx context.Context, frame InputFrame) error {
	if len(frame.Data) == 0 {
		return nil
	}
	frame.Data = append([]byte(nil), frame.Data...)
	if err := p.gate.ReserveInput(ctx, frame); err != nil {
		return err
	}
	request := inputRequest{ctx: ctx, frame: frame, result: make(chan error, 1)}
	select {
	case p.queue <- request:
	case <-ctx.Done():
		p.gate.CompleteInput(ctx, frame, ctx.Err())
		return ctx.Err()
	case <-p.done:
		p.gate.CompleteInput(ctx, frame, ErrInputPumpClosed)
		return ErrInputPumpClosed
	}
	// Once accepted into the queue, wait for the definitive write outcome even
	// if the caller's context is cancelled. Returning an ambiguous timeout could
	// cause a controller to retry bytes that were already delivered to the PTY.
	return <-request.result
}

func (p *InputPump) Close() { p.once.Do(func() { close(p.done) }) }

func (p *InputPump) run() {
	for {
		select {
		case request := <-p.queue:
			err := p.gate.ValidateInput(request.ctx, request.frame)
			if err == nil {
				err = writeFull(p.writer, request.frame.Data)
			}
			p.gate.CompleteInput(request.ctx, request.frame, err)
			request.result <- err
		case <-p.done:
			for {
				select {
				case request := <-p.queue:
					p.gate.CompleteInput(request.ctx, request.frame, ErrInputPumpClosed)
					request.result <- ErrInputPumpClosed
				default:
					return
				}
			}
		}
	}
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
