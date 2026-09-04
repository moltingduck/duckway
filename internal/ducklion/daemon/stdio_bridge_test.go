package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type pipeStream struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

type blockingStream struct {
	closed chan struct{}
	once   sync.Once
}

func (s *blockingStream) Read([]byte) (int, error)       { <-s.closed; return 0, io.ErrClosedPipe }
func (s *blockingStream) Write(data []byte) (int, error) { return len(data), nil }
func (s *blockingStream) Close() error                   { s.once.Do(func() { close(s.closed) }); return nil }

func (s *pipeStream) Read(data []byte) (int, error)  { return s.reader.Read(data) }
func (s *pipeStream) Write(data []byte) (int, error) { return s.writer.Write(data) }
func (s *pipeStream) Close() error {
	_ = s.writer.Close()
	return s.reader.Close()
}

func TestBridgeStdioCopiesOpaqueBytesWithoutDecoration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	want := []byte{0, 1, 2, '\n', 0xff, 0x1b, '[', 'A'}
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		data, err := io.ReadAll(conn)
		if err == nil {
			_, err = conn.Write(data)
		}
		serverDone <- err
	}()
	var output bytes.Buffer
	if err := BridgeStdio(context.Background(), path, io.NopCloser(bytes.NewReader(want)), &output); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("bridge output=%v want=%v", output.Bytes(), want)
	}
}

func TestBridgeStdioCarriesRealDucklionProtocol(t *testing.T) {
	server, err := Open(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	bridgeInputReader, clientWriter := io.Pipe()
	clientReader, bridgeOutputWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- BridgeStdio(ctx, server.SocketPath(), bridgeInputReader, bridgeOutputWriter) }()
	client, err := Connect(&pipeStream{reader: clientReader, writer: clientWriter}, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := client.ListSessions()
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	_ = client.Close()
	cancel()
	select {
	case <-bridgeDone:
	case <-time.After(time.Second):
		t.Fatal("stdio bridge did not stop")
	}
	_ = server.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestConnectContextClosesUnresponsiveStream(t *testing.T) {
	stream := &blockingStream{closed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := ConnectContext(ctx, stream, "laptop"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connect error=%v", err)
	}
	select {
	case <-stream.closed:
	default:
		t.Fatal("unresponsive stream was not closed")
	}
}
