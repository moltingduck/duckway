package main

import (
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
)

// localAgentProbe reports whether a ducklion daemon accepts connections on this
// machine under this OS user. Tests replace it.
var localAgentProbe = func() (string, bool) {
	sock := filepath.Join(daemon.DefaultRoot(), "ducklion.sock")
	info, err := os.Stat(sock)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return "", false
	}
	conn, err := net.DialTimeout("unix", sock, 150*time.Millisecond)
	if err != nil {
		return "", false
	}
	_ = conn.Close()
	return sock, true
}

// localAgentWarning is non-empty when agent sessions may be running locally as
// the same OS user, who could read the approver login file and approve their own
// requests. It is a warning only.
func localAgentWarning() string {
	if _, ok := localAgentProbe(); !ok {
		return ""
	}
	return "warning: a local ducklion daemon runs as this user; agents here can read the approver login file and approve their own requests. Log in only on an account or host agents cannot read."
}
