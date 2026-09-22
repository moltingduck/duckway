package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

func TestForwardOutputSeedsFreshHubAfterReplayGap(t *testing.T) {
	hub := duckruntime.NewOutputHub(8)
	for i := 0; i < 16; i++ {
		hub.Publish([]byte("output"))
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: t.TempDir() + "/supervisor.sock", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan *net.UnixConn, 1)
	go func() { conn, _ := listener.AcceptUnix(); accepted <- conn }()
	conn, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-accepted
	defer serverConn.Close()
	serverCodec := bridge.NewCodec(serverConn, serverConn, bridge.DefaultMaxFrame)
	gapSeen := make(chan struct{}, 1)
	liveSeen := make(chan struct{}, 1)
	go func() {
		for {
			var request protocol.Request
			if serverCodec.Read(&request) != nil {
				return
			}
			var output protocol.SupervisorOutput
			if json.Unmarshal(request.Body, &output) == nil {
				if output.Gap {
					gapSeen <- struct{}{}
				}
				if bytes.Equal(output.Data, []byte("live")) {
					liveSeen <- struct{}{}
				}
				ack, _ := json.Marshal(protocol.SupervisorOutputAck{Offset: output.Offset, Length: uint64(len(output.Data))})
				if serverCodec.Write(protocol.Response{ID: request.ID, Result: ack}) != nil {
					return
				}
			}
		}
	}()
	client := &SupervisorClient{conn: conn, codec: bridge.NewCodec(conn, conn, bridge.DefaultMaxFrame)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.ForwardOutput(ctx, hub) }()
	select {
	case <-gapSeen:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not seed replay gap")
	}
	hub.Publish([]byte("live"))
	select {
	case <-liveSeen:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("live output did not reach fresh supervisor hub")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not stop")
	}
}

func TestForwardOutputReportsLaggedReplay(t *testing.T) {
	hub := duckruntime.NewOutputHub(8)
	for i := 0; i < 16; i++ {
		hub.Publish([]byte("output"))
	}

	err := (&SupervisorClient{}).ForwardOutput(context.Background(), hub)
	if !errors.Is(err, ErrSupervisorOutputLagged) {
		t.Fatalf("ForwardOutput error=%v, want ErrSupervisorOutputLagged", err)
	}
}

func TestSupervisorOutputQueueIsByteBoundedAndRecoversAfterLag(t *testing.T) {
	hub := duckruntime.NewOutputHub(8 << 20)
	_, stream, cancel, err := hub.Subscribe(0, supervisorOutputQueue)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	frame := bytes.Repeat([]byte("x"), supervisorOutputFrameBytes)
	for i := 0; i < supervisorOutputQueue+8; i++ {
		hub.Publish(frame)
	}
	if got := supervisorOutputQueue * supervisorOutputFrameBytes; got > supervisorOutputQueueBytes {
		t.Fatalf("queue byte budget=%d exceeds limit=%d", got, supervisorOutputQueueBytes)
	}

	for i := 0; i < supervisorOutputQueue; i++ {
		if _, ok := <-stream; !ok {
			t.Fatalf("subscriber closed before bounded queue drained at frame %d", i)
		}
	}
	if _, ok := <-stream; ok {
		t.Fatal("lagged subscriber remained open after queue overflow")
	}
	_, recovered, cancelRecovered, err := hub.Subscribe(hubEndOffset(hub), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelRecovered()
	hub.Publish([]byte("recovered"))
	got, ok := <-recovered
	if !ok || string(got.Data) != "recovered" {
		t.Fatalf("recovery frame=%q open=%v", got.Data, ok)
	}
}

func hubEndOffset(hub *duckruntime.OutputHub) uint64 {
	_, end := hub.Bounds()
	return end
}
