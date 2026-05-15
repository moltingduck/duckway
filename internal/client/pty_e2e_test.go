package client

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)


// TestPTYStartup verifies that respondToDecQueries correctly handles the DEC
// terminal queries that Claude Code's Ink TUI sends at startup. Without
// correct responses, Ink hangs indefinitely. We start claude via PTY, confirm
// at least one query is answered, then kill it.
func TestPTYStartup(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_PTY_E2E") != "1" {
		t.Skip("set DUCKWAY_TEST_PTY_E2E=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not in PATH:", err)
	}

	cmd := exec.Command(bin, "--dangerously-skip-permissions")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: ptyRows, Cols: ptyCols})
	if err != nil {
		t.Fatal("pty start:", err)
	}
	defer func() {
		ptmx.Close()
		_ = cmd.Wait()
	}()

	// Read PTY output for up to 8 seconds, count DEC query responses.
	// PTY fds use raw blocking syscalls that cannot be interrupted by closing
	// the fd from another goroutine. To stop the read, we kill the subprocess
	// after 8s — that causes the PTY slave to close, making the master read
	// return EIO.
	queriesAnswered := 0
	readDone := make(chan struct{})
	buf := make([]byte, 4096)
	go func() {
		defer close(readDone)
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				if resp := respondToDecQueries(buf[:n]); len(resp) > 0 {
					queriesAnswered++
					_, _ = ptmx.Write(resp)
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	time.Sleep(8 * time.Second)
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-readDone

	t.Logf("DEC queries answered: %d", queriesAnswered)
	if queriesAnswered == 0 {
		t.Error("expected Ink to send at least one DEC query; got none — check if ANSI parser is working")
	}
}

// TestCCPrintE2E exercises the full runViaPrint round-trip against the real
// claude binary: send a prompt, receive a session_id and a response, then
// resume the session to verify multi-turn continuity.
func TestCCPrintE2E(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_PTY_E2E") != "1" {
		t.Skip("set DUCKWAY_TEST_PTY_E2E=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not in PATH:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cwd := t.TempDir()

	// Turn 1
	sid, result, isError, err := runViaPrint(ctx, bin, cwd, `reply with exactly the word: hello`, "", nil)
	if err != nil {
		t.Fatal("turn 1 error:", err)
	}
	t.Logf("turn1: session_id=%s isError=%v result=%q", sid, isError, result)
	if sid == "" {
		t.Error("expected non-empty session_id")
	}
	if !strings.Contains(strings.ToLower(result), "hello") {
		t.Errorf("expected 'hello' in turn1 result, got: %q", result)
	}

	// Turn 2 — resume the same session
	_, result2, _, err := runViaPrint(ctx, bin, cwd, `what did I just ask you to say?`, sid, nil)
	if err != nil {
		t.Fatal("turn 2 error:", err)
	}
	t.Logf("turn2: result=%q", result2)
	if !strings.Contains(strings.ToLower(result2), "hello") {
		t.Errorf("expected previous context ('hello') in turn2 result, got: %q", result2)
	}
}
