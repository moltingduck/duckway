package runtime

import (
	"bytes"
	"context"
	"errors"
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
