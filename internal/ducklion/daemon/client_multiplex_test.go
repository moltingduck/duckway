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
	stream := &writeBlockingStream{closed: make(chan struct{})}
	c := &Client{conn: stream, subscriptions: make(map[string]*OutputSubscription), orphanEvents: make(map[string][]protocol.OutputEvent),
		ignoredSubscriptions: make(map[string]bool), done: make(chan struct{})}
	metadata := protocol.OutputSubscribeResult{SubscriptionID: "sub", RuntimeID: "runtime", InstanceID: "instance", SessionID: "ABC123", RuntimeGeneration: 1}
	subscription := &OutputSubscription{client: c, metadata: metadata, events: make(chan outputResult, 1), terminalDone: make(chan struct{})}
	c.subscriptions[metadata.SubscriptionID] = subscription
	c.dispatchOutput(protocol.OutputEvent{Type: "output", SubscriptionID: "sub", Frame: protocol.OutputFrame{Data: []byte("a")}})
	c.dispatchOutput(protocol.OutputEvent{Type: "output", SubscriptionID: "sub", Frame: protocol.OutputFrame{Offset: 1, Data: []byte("b")}})
	if _, err := subscription.Read(); !errors.Is(err, ErrOutputSubscriberLagged) {
		t.Fatalf("slow subscriber error = %v", err)
	}
}

func TestOutputSubscriberQueueIsByteBounded(t *testing.T) {
	stream := &writeBlockingStream{closed: make(chan struct{})}
	c := &Client{conn: stream, subscriptions: make(map[string]*OutputSubscription), orphanEvents: make(map[string][]protocol.OutputEvent),
		ignoredSubscriptions: make(map[string]bool), done: make(chan struct{})}
	metadata := protocol.OutputSubscribeResult{SubscriptionID: "sub", RuntimeID: "runtime", InstanceID: "instance", SessionID: "ABC123", RuntimeGeneration: 1}
	subscription := &OutputSubscription{client: c, metadata: metadata, events: make(chan outputResult, 64), terminalDone: make(chan struct{})}
	c.subscriptions[metadata.SubscriptionID] = subscription
	frame := make([]byte, 64<<10)
	for index := 0; index < 17; index++ {
		c.dispatchOutput(protocol.OutputEvent{Type: "output", SubscriptionID: "sub", Frame: protocol.OutputFrame{Offset: uint64(index) * uint64(len(frame)), Data: frame}})
	}
	if _, err := subscription.Read(); !errors.Is(err, ErrOutputSubscriberLagged) {
		t.Fatalf("byte-bounded subscriber error=%v", err)
	}
	subscription.queueMu.Lock()
	queued := subscription.queuedBytes
	subscription.queueMu.Unlock()
	if queued > maxQueuedOutputBytesPerSubscription {
		t.Fatalf("queued bytes=%d limit=%d", queued, maxQueuedOutputBytesPerSubscription)
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

func TestPreRegistrationOutputOverflowClosesBridgeInsteadOfHidingGap(t *testing.T) {
	stream := &writeBlockingStream{closed: make(chan struct{})}
	c := &Client{conn: stream, subscriptions: make(map[string]*OutputSubscription), orphanEvents: make(map[string][]protocol.OutputEvent),
		ignoredSubscriptions: make(map[string]bool), done: make(chan struct{})}
	for index := 0; index < 81; index++ {
		c.dispatchOutput(protocol.OutputEvent{Type: "output", SubscriptionID: "early", Frame: protocol.OutputFrame{Offset: uint64(index), Data: []byte("x")}})
	}
	select {
	case <-c.done:
	default:
		t.Fatal("overflowed pre-registration replay window did not close bridge")
	}
	c.stateMu.Lock()
	err := c.readErr
	c.stateMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "safe replay window") {
		t.Fatalf("bridge error=%v", err)
	}
}

func TestPreRegistrationOutputIsByteBounded(t *testing.T) {
	stream := &writeBlockingStream{closed: make(chan struct{})}
	c := &Client{conn: stream, subscriptions: make(map[string]*OutputSubscription), orphanEvents: make(map[string][]protocol.OutputEvent),
		ignoredSubscriptions: make(map[string]bool), done: make(chan struct{})}
	frame := make([]byte, 64<<10)
	for index := 0; index < 17; index++ {
		c.dispatchOutput(protocol.OutputEvent{Type: "output", SubscriptionID: "early", Frame: protocol.OutputFrame{Offset: uint64(index) * uint64(len(frame)), Data: frame}})
	}
	select {
	case <-c.done:
	default:
		t.Fatal("byte-overflowed pre-registration replay window did not close bridge")
	}
	if got := orphanOutputBytes(c.orphanEvents["early"]); got > maxQueuedOutputBytesPerSubscription {
		t.Fatalf("orphan bytes=%d limit=%d", got, maxQueuedOutputBytesPerSubscription)
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
