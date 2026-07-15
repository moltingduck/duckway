package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/hackerduck/duckway/internal/version"
)

var startDetachedDuckwayCommand = startDetachedDuckwayCommandDefault

func (w *CCWatch) cmdDuckwayVersion(replyHandle string, args []string) {
	if len(args) != 0 {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ usage: `!duckway-version`")
		return
	}
	_ = w.api.PostCC(context.Background(), replyHandle, "duckway "+version.Get())
}

func (w *CCWatch) cmdDuckwayRestart(replyHandle string, args []string) {
	if len(args) != 0 {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ usage: `!duckway-restart`")
		return
	}
	logPath := filepath.Join(w.configDir, "cc-ops.log")
	if err := w.postCCOpsAccepted(replyHandle, "Duckway restart accepted. Local daemons will restart in the background.", logPath); err != nil {
		return
	}
	if err := startDetachedDuckwayCommand(w.configDir, logPath, []string{"restart"}); err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ start restart helper failed: "+err.Error())
	}
}

func (w *CCWatch) cmdDuckwayUpdate(replyHandle string, args []string) {
	restart, err := parseDuckwayUpdateArgs(args)
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ "+err.Error()+"\nUsage: `!duckway-update [--restart]`")
		return
	}
	if w.cfg == nil || w.cfg.ServerURL == "" {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ no server URL in client config; run `duckway init` on the client first.")
		return
	}
	logPath := filepath.Join(w.configDir, "cc-ops.log")
	msg := "Duckway update accepted. The update will run in the background."
	cmdArgs := []string{"update", "--server", w.cfg.ServerURL}
	if restart {
		msg = "Duckway update accepted. Local daemons will restart in the background if the update succeeds."
		cmdArgs = append(cmdArgs, "--restart")
	}
	if err := w.postCCOpsAccepted(replyHandle, msg, logPath); err != nil {
		return
	}
	if err := startDetachedDuckwayCommand(w.configDir, logPath, cmdArgs); err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ start update helper failed: "+err.Error())
	}
}

func (w *CCWatch) postCCOpsAccepted(replyHandle, msg, logPath string) error {
	return w.api.PostCC(context.Background(), replyHandle, msg+"\nLogs: `"+logPath+"`")
}

func parseDuckwayUpdateArgs(args []string) (bool, error) {
	switch len(args) {
	case 0:
		return false, nil
	case 1:
		if args[0] == "--restart" {
			return true, nil
		}
	}
	return false, fmt.Errorf("unsupported arguments for `!duckway-update`: `%s`", shellJoinForLog(args))
}

func startDetachedDuckwayCommandDefault(configDir, logPath string, args []string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logF.Close()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own executable: %w", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.Env = append(os.Environ(), "DUCKWAY_CONFIG_DIR="+configDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start duckway helper: %w", err)
	}
	return cmd.Process.Release()
}
