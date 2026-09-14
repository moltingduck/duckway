package daemon

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func TestForwardForegroundWaitsForControlReady(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	client := &SupervisorClient{codec: bridge.NewCodec(clientConn, clientConn, bridge.DefaultMaxFrame),
		identity:           protocol.SupervisorRegistered{SessionID: "ABC123", RuntimeGeneration: 1},
		supportsForeground: true, controlReady: make(chan struct{})}
	server := bridge.NewCodec(serverConn, serverConn, bridge.DefaultMaxFrame)
	requestCh := make(chan protocol.Request, 2)
	serverDone := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 2; attempt++ {
			var request protocol.Request
			if err := server.Read(&request); err != nil {
				serverDone <- err
				return
			}
			requestCh <- request
			response := protocol.Response{ID: request.ID, Result: json.RawMessage(`{"ok":true}`)}
			if attempt == 0 {
				response = protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrStaleGeneration, Message: "runtime is still recovering"}}
			}
			if err := server.Write(response); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	forwardDone := make(chan error, 1)
	go func() { forwardDone <- forwardForeground(ctx, client, nil) }()
	select {
	case request := <-requestCh:
		t.Fatalf("foreground reported before control registration: %s", request.Type)
	case <-time.After(75 * time.Millisecond):
	}
	close(client.controlReady)
	select {
	case request := <-requestCh:
		if request.Type != "supervisor.foreground" || request.SessionID != "ABC123" {
			t.Fatalf("unexpected foreground request: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground report did not start after control readiness")
	}
	select {
	case request := <-requestCh:
		if request.Type != "supervisor.foreground" {
			t.Fatalf("application-level rejection was not retried: %+v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground report did not retry after application-level rejection")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground receipt was not sent")
	}
	cancel()
	select {
	case <-forwardDone:
	case <-time.After(time.Second):
		t.Fatal("foreground forwarder did not stop after cancellation")
	}
}
