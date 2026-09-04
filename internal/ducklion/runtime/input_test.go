package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

type fakeGate struct {
	mu          sync.Mutex
	pending     int
	completed   int
	validateErr error
}

func (g *fakeGate) ReserveInput(context.Context, InputFrame) error {
	g.mu.Lock()
	g.pending++
	g.mu.Unlock()
	return nil
}

func (g *fakeGate) ValidateInput(context.Context, InputFrame) error { return g.validateErr }

func (g *fakeGate) CompleteInput(_ context.Context, _ InputFrame, _ error) {
	g.mu.Lock()
	g.pending--
	g.completed++
	g.mu.Unlock()
}

type oneByteWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type partialErrorWriter struct{ wrote bytes.Buffer }

func (w *partialErrorWriter) Write(data []byte) (int, error) {
	_ = w.wrote.WriteByte(data[0])
	return 1, io.ErrUnexpectedEOF
}

func (w *oneByteWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(data[:1])
}

func TestInputPumpReservesSerializesAndHandlesShortWrites(t *testing.T) {
	gate := &fakeGate{}
	writer := &oneByteWriter{}
	pump := NewInputPump(gate, writer, 4)
	defer pump.Close()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "laptop"}
	for i, data := range []string{"abc", "DEF"} {
		frame := InputFrame{Sequence: uint64(i + 1), Principal: "terminal:laptop", Owner: owner, OwnershipEpoch: 2, RuntimeGeneration: 3, Data: []byte(data)}
		if err := pump.Submit(context.Background(), frame); err != nil {
			t.Fatal(err)
		}
	}
	gate.mu.Lock()
	pending, completed := gate.pending, gate.completed
	gate.mu.Unlock()
	if pending != 0 || completed != 2 || writer.buf.String() != "abcDEF" {
		t.Fatalf("pending=%d completed=%d output=%q", pending, completed, writer.buf.String())
	}
}

func TestInputPumpRevalidatesBeforeWriting(t *testing.T) {
	gate := &fakeGate{validateErr: model.ErrStaleEpoch}
	writer := &oneByteWriter{}
	pump := NewInputPump(gate, writer, 1)
	defer pump.Close()
	err := pump.Submit(context.Background(), InputFrame{Data: []byte("forbidden")})
	if !errors.Is(err, model.ErrStaleEpoch) || writer.buf.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, writer.buf.String())
	}
}

func TestInputPumpRejectsAfterCloseWithoutLeakingReservation(t *testing.T) {
	gate := &fakeGate{}
	pump := NewInputPump(gate, &oneByteWriter{}, 1)
	pump.Close()
	for i := 0; i < 100; i++ {
		if err := pump.Submit(context.Background(), InputFrame{Data: []byte("x")}); !errors.Is(err, ErrInputPumpClosed) {
			t.Fatalf("submit %d error=%v", i, err)
		}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.pending != 0 || gate.completed != 100 {
		t.Fatalf("pending=%d completed=%d", gate.pending, gate.completed)
	}
}

func TestInputPumpReportsCommittedPrefix(t *testing.T) {
	gate := &fakeGate{}
	writer := &partialErrorWriter{}
	pump := NewInputPump(gate, writer, 1)
	defer pump.Close()
	err := pump.Submit(context.Background(), InputFrame{Data: []byte("abc")})
	var partial *PartialWriteError
	if !errors.As(err, &partial) || partial.Written != 1 || writer.wrote.String() != "a" {
		t.Fatalf("error=%v written=%q", err, writer.wrote.String())
	}
}
