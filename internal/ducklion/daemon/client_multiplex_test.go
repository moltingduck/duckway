package daemon

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type writeBlockingStream struct {
	closed chan struct{}
	once   sync.Once
}

func (s *writeBlockingStream) Read([]byte) (int, error) { <-s.closed; return 0, io.ErrClosedPipe }
func (s *writeBlockingStream) Write([]byte) (int, error) {
	<-s.closed
	return 0, io.ErrClosedPipe
}
func (s *writeBlockingStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func TestSlowLocalOutputSubscriberReceivesTerminalError(t *testing.T) {
	c := &Client{subscriptions: make(map[string]*OutputSubscription), orphanEvents: make(map[string][]protocol.OutputEvent),
		ignoredSubscriptions: make(map[string]bool), done: make(chan struct{})}
	metadata := protocol.OutputSubscribeResult{SubscriptionID: "sub", RuntimeID: "runtime", InstanceID: "instance", SessionID: "ABC123", RuntimeGeneration: 1}
	subscription := &OutputSubscription{client: c, metadata: metadata, events: make(chan outputResult, 1), terminalDone: make(chan struct{})}
	c.subscriptions[metadata.SubscriptionID] = subscription
	c.dispatchOutput(protocol.OutputEvent{Type: "output", SubscriptionID: "sub", Frame: protocol.OutputFrame{Data: []byte("a")}})
	c.dispatchOutput(protocol.OutputEvent{Type: "output", SubscriptionID: "sub", Frame: protocol.OutputFrame{Offset: 1, Data: []byte("b")}})
	if _, err := subscription.Read(); err == nil || !strings.Contains(err.Error(), "lagged") {
		t.Fatalf("slow subscriber error = %v", err)
	}
}

func TestIgnoredOutputSubscriptionsStayBounded(t *testing.T) {
	c := &Client{ignoredSubscriptions: make(map[string]bool)}
	c.stateMu.Lock()
	for i := 0; i < 1000; i++ {
		c.markIgnoredLocked(strings.Repeat("x", i%10) + string(rune(i+100)))
	}
	c.stateMu.Unlock()
	if len(c.ignoredSubscriptions) > 256 || len(c.ignoredOrder) > 256 {
		t.Fatalf("ignored subscriptions map=%d order=%d", len(c.ignoredSubscriptions), len(c.ignoredOrder))
	}
}

func TestCallContextDeadlineInterruptsBlockedWrite(t *testing.T) {
	stream := &writeBlockingStream{closed: make(chan struct{})}
	c := &Client{conn: stream, codec: bridge.NewCodec(stream, stream, bridge.DefaultMaxFrame), pending: make(map[string]chan responseResult),
		writeGate: make(chan struct{}, 1), done: make(chan struct{})}
	c.writeGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.CallContext(ctx, protocol.Request{ID: "blocked", Type: "status"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call error=%v", err)
	}
	select {
	case <-stream.closed:
	default:
		t.Fatal("timed out call did not close transport")
	}
}
