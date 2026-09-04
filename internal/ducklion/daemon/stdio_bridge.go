package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// BridgeStdio is the SSH-facing transport shim. It deliberately treats every
// byte as opaque so protocol frames are never logged, decoded, or normalized.
func BridgeStdio(ctx context.Context, socketPath string, stdin io.ReadCloser, stdout io.Writer) error {
	defer stdin.Close()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect Ducklion daemon: %w", err)
	}
	conn := connection.(*net.UnixConn)
	defer conn.Close()
	type copyResult struct {
		direction string
		err       error
	}
	result := make(chan copyResult, 2)
	go func() {
		_, copyErr := io.Copy(conn, stdin)
		_ = conn.CloseWrite()
		result <- copyResult{direction: "input", err: copyErr}
	}()
	go func() {
		_, copyErr := io.Copy(stdout, conn)
		result <- copyResult{direction: "output", err: copyErr}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case completed := <-result:
			if completed.err != nil && !errors.Is(completed.err, net.ErrClosed) {
				return fmt.Errorf("bridge %s: %w", completed.direction, completed.err)
			}
			if completed.direction == "output" {
				return nil
			}
		}
	}
}
