//go:build !linux && !darwin && !freebsd

package daemon

import (
	"fmt"
	"net"
)

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, fmt.Errorf("ducklion peer credential verification is unsupported on this platform")
}
