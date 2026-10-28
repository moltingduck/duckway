//go:build !linux

package client

import (
	"fmt"
	"net"
	"os"
	"os/exec"
)

func codexDaemonVerificationSupported() bool { return false }
func daemonSocketRefused(error) bool         { return false }
func codexDaemonPeerEnvironment(net.Conn) ([]string, error) {
	return nil, fmt.Errorf("daemon environment verification unavailable")
}
func configureDaemonProbe(*exec.Cmd) {}
func cleanupDaemonProbe(*exec.Cmd)   {}

func codexDaemonProcessEnvironment(int) ([]string, bool, error) {
	return nil, true, fmt.Errorf("daemon process verification unavailable")
}

func openDaemonOwner(string) (*os.File, error) { return nil, fmt.Errorf("unsupported platform") }
func closeDaemonOwner(f *os.File)              { _ = f.Close() }
