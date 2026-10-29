package clientplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type RunOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Env    []string
}

// Run starts a foreground child without a shell. The caller supplies inherited
// proxy/CA environment; only non-secret host metadata is added here. The child
// owns a process group so cancellation also cleans up ordinary descendants.
func (h Host) Run(ctx context.Context, name string, args []string, opts RunOptions) (int, error) {
	if err := ValidateName(name); err != nil {
		return 1, err
	}
	lock, err := h.lock(ctx, false)
	if err != nil {
		return 1, err
	}
	locked := true
	defer func() {
		if locked {
			unlock(lock)
		}
	}()
	installed, err := h.read(name)
	if err != nil {
		return 1, err
	}
	executable := filepath.Join(h.root(), name, "releases", installed.Release, "executable")
	if err := regularFile(executable); err != nil {
		return 1, err
	}
	f, err := os.Open(executable)
	if err != nil {
		return 1, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, installed.Artifact.Size+1))
	f.Close()
	if err != nil || n != installed.Artifact.Size || hex.EncodeToString(digest.Sum(nil)) != installed.Artifact.SHA256 {
		return 1, fmt.Errorf("installed plugin checksum or size mismatch")
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	// Make the executable absolute because ConfigDir may be relative and exec
	// must never search PATH for installed plugin executables.
	executable, err = filepath.Abs(executable)
	if err != nil {
		return 1, err
	}
	cmd := exec.Command(executable, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr
	cmd.Env = pluginEnv(opts.Env, h, installed)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	restore, err := foregroundTerminal(cmd, opts.Stdin)
	if err != nil {
		return 1, err
	}
	defer restore()
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("start plugin: %w", err)
	}
	unlock(lock)
	locked = false
	done := make(chan struct{})
	canceled := make(chan struct{})
	go func() {
		defer close(canceled)
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	err = cmd.Wait()
	close(done)
	<-canceled
	// Foreground plugins may spawn helpers, but must not leave detached services.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if ctx.Err() != nil {
		return 130, ctx.Err()
	}
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	return 1, fmt.Errorf("wait for plugin: %w", err)
}

func pluginEnv(base []string, h Host, p Installed) []string {
	if base == nil {
		base = os.Environ()
	}
	values := map[string]string{
		"DUCKWAY_PLUGIN_API_VERSION": "1",
		"DUCKWAY_PLUGIN_NAME":        p.Manifest.Name,
		"DUCKWAY_PLUGIN_SOURCE":      p.Source,
		"DUCKWAY_PLUGIN_CONFIG_DIR":  filepath.Join(h.root(), p.Manifest.Name, "data"),
	}
	values["DUCKWAY_PLUGIN_ORIGIN"] = p.ServerOrigin
	env := make([]string, 0, len(base)+len(values))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "DUCKWAY_PLUGIN_") && key != "DUCKWAY_TOKEN" {
			env = append(env, kv)
		}
	}
	for _, key := range []string{"DUCKWAY_PLUGIN_API_VERSION", "DUCKWAY_PLUGIN_NAME", "DUCKWAY_PLUGIN_SOURCE", "DUCKWAY_PLUGIN_CONFIG_DIR", "DUCKWAY_PLUGIN_ORIGIN"} {
		env = append(env, key+"="+values[key])
	}
	return env
}

// The child receives the terminal foreground process group when stdin is the
// controlling terminal. Without this handoff an interactive plugin would stop
// with SIGTTIN as soon as it reads. Existing Ducklion PTYs remain the supervisor.
func foregroundTerminal(cmd *exec.Cmd, stdin io.Reader) (func(), error) {
	f, ok := stdin.(*os.File)
	if !ok {
		return func() {}, nil
	}
	pgid, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return func() {}, nil
	} // a pipe, redirected file, or non-controlling PTY
	if pgid != syscall.Getpgrp() {
		return nil, fmt.Errorf("plugin must run in the terminal foreground")
	}
	cmd.SysProcAttr.Foreground = true
	cmd.SysProcAttr.Ctty = int(f.Fd())
	return func() { restoreTerminal(int(f.Fd()), pgid) }, nil
}
