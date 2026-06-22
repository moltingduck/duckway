package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	ptyRows           = 40
	ptyCols           = 120
	ptyDefaultTimeout = 5 * time.Minute
)

type stopPayload struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

type hookEvent struct {
	Name    string
	Payload string
}

// runViaPTY drives claude in interactive PTY mode, injects the prompt after
// Ink's startup terminal queries quiet down, and captures the result from the
// Stop hook payload.
func runViaPTY(ctx context.Context, bin, cwd, prompt string, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	tmpDir, err := os.MkdirTemp("", "duckway-cc-*")
	if err != nil {
		return "", "", false, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	fifoPath := filepath.Join(tmpDir, "hook.fifo")
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		return "", "", false, fmt.Errorf("mkfifo: %w", err)
	}

	hookScript := filepath.Join(tmpDir, "hook.sh")
	// Hook receives JSON payload on stdin, writes "event\tpayload\n" to FIFO.
	hookContent := "#!/bin/sh\npayload=$(cat)\nprintf '%s\\t%s\\n' \"$1\" \"$payload\" > " + fifoPath + "\n"
	if err := os.WriteFile(hookScript, []byte(hookContent), 0700); err != nil {
		return "", "", false, fmt.Errorf("write hook script: %w", err)
	}

	settingsJSON, err := buildHooksSettings(hookScript)
	if err != nil {
		return "", "", false, fmt.Errorf("build settings json: %w", err)
	}

	args := []string{
		"--dangerously-skip-permissions",
		"--settings", settingsJSON,
	}
	if sid != "" {
		args = append(args, "--resume", sid)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), extraEnv...)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: ptyRows, Cols: ptyCols})
	if err != nil {
		return "", "", false, fmt.Errorf("pty start: %w", err)
	}

	// Open FIFO O_RDWR so we hold both ends open. This prevents our scanner
	// from blocking waiting for a writer, and prevents hook writes from
	// blocking waiting for a reader.
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		ptmx.Close()
		return "", "", false, fmt.Errorf("open fifo: %w", err)
	}

	// stopCh signals background goroutines to exit without closing writeCh
	// (which would panic if a goroutine is still trying to send to it).
	// cleanup() must be called before wg.Wait() to unblock I/O goroutines.
	stopCh := make(chan struct{})
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			close(stopCh)
			ptmx.Close()  // unblocks ptyReader
			fifo.Close()  // unblocks fifoReader
		})
	}
	defer cleanup()

	hookCh := make(chan hookEvent, 4)
	writeCh := make(chan []byte, 64)

	var wg sync.WaitGroup

	// ptyWriter: serializes all writes to the PTY master to avoid races.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case data := <-writeCh:
				_, _ = ptmx.Write(data)
			case <-stopCh:
				return
			}
		}
	}()

	// ptyReader: drains PTY output and responds to Ink's terminal queries.
	// Without responding to DA1/DA2/XTVERSION/DSR, Ink hangs at startup.
	//
	// Ready detection: Claude Code has no "SessionStart" hook. Instead we
	// watch for the burst of DEC/XTerm queries Ink fires at startup. Once
	// we've replied to at least one query and then seen 600 ms of silence,
	// the TUI is rendered and ready for input. A 4-second fallback fires if
	// Ink never queries (e.g. future headless mode).
	readyCh := make(chan struct{}, 1)
	signalReady := func() {
		select {
		case readyCh <- struct{}{}:
		default:
		}
	}
	// Fallback: signal ready after 5 seconds regardless.
	fallbackTimer := time.AfterFunc(5*time.Second, signalReady)
	defer fallbackTimer.Stop()

	// promptSentFlag lets the ptyReader know when to start tracking response
	// output so it can fire the exit signal after the response quiets down.
	// exitSentFlag prevents re-sending the exit signal when Ink's shutdown
	// sequence itself generates PTY output.
	var promptSentFlag atomic.Bool
	var exitSentFlag atomic.Bool

	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		var startupTimer *time.Timer  // 600ms quiet after startup queries → ready
		var responseTimer *time.Timer // 2s quiet after prompt → send /exit
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				if resp := respondToDecQueries(buf[:n]); len(resp) > 0 {
					select {
					case writeCh <- resp:
					case <-stopCh:
						return
					}
					// Use a 3-second quiet period so Ink has time to fully
				// render its TUI before we inject the prompt.
					if startupTimer == nil {
						startupTimer = time.AfterFunc(3*time.Second, signalReady)
					} else {
						startupTimer.Reset(3 * time.Second)
					}
				}
				// After the prompt is sent (and before we've sent the exit
				// signal), reset the response-done timer on every PTY chunk.
				// 2s of silence → response done → Ctrl+C to quit BubbleTea.
				if promptSentFlag.Load() && !exitSentFlag.Load() {
					if responseTimer == nil {
						responseTimer = time.AfterFunc(2*time.Second, func() {
							if exitSentFlag.Swap(true) {
								return // already sent
							}
							if cmd.Process != nil {
								_ = cmd.Process.Signal(syscall.SIGTERM)
							}
						})
					} else {
						responseTimer.Reset(2 * time.Second)
					}
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// fifoReader: reads hook events line by line from the named pipe.
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(fifo)
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.SplitN(line, "\t", 2)
			ev := hookEvent{Name: parts[0]}
			if len(parts) == 2 {
				ev.Payload = parts[1]
			}
			select {
			case hookCh <- ev:
			case <-stopCh:
				return
			}
		}
	}()

	timeout := time.NewTimer(ptyDefaultTimeout)
	defer timeout.Stop()

	promptSent := false
	for {
		select {
		case <-ctx.Done():
			cleanup()
			wg.Wait()
			return "", "", false, ctx.Err()

		case <-timeout.C:
			cleanup()
			wg.Wait()
			return "", "", false, fmt.Errorf("timed out waiting for claude after %v", ptyDefaultTimeout)

		case <-readyCh:
			if !promptSent {
				promptSent = true
				promptSentFlag.Store(true)
				select {
				case writeCh <- buildPTYInput(prompt):
				case <-stopCh:
				}
			}

		case ev := <-hookCh:
			switch ev.Name {
			case "stop":
				var sp stopPayload
				if jsonErr := json.Unmarshal([]byte(ev.Payload), &sp); jsonErr == nil {
					sessionID = sp.SessionID
					result = sp.LastAssistantMessage
				}
				cleanup()
				wg.Wait()
				return sessionID, result, false, nil
			}
		}
	}
}

// buildHooksSettings produces the inline --settings JSON that registers
// the Stop hook pointing at hookScript.
func buildHooksSettings(hookScript string) (string, error) {
	settings := map[string]interface{}{
		"hooks": map[string]interface{}{
			"Stop": []map[string]interface{}{
				{
					"matcher": "*",
					"hooks": []map[string]interface{}{
						{"type": "command", "command": hookScript + " stop"},
					},
				},
			},
		},
	}
	b, err := json.Marshal(settings)
	return string(b), err
}

// buildPTYInput sends the prompt to Ink. Newlines are replaced with spaces to
// avoid triggering Enter mid-prompt; a final \r submits.
func buildPTYInput(prompt string) []byte {
	// Replace embedded newlines so they don't submit the form early.
	safe := strings.ReplaceAll(prompt, "\n", " ")
	return append([]byte(safe), '\r')
}

// respondToDecQueries scans buf for the DEC/XTerm terminal queries that Ink
// (Claude Code's TUI runtime) issues at startup and returns the response bytes
// that must be written back to the PTY master. If none are found, returns nil.
func respondToDecQueries(buf []byte) []byte {
	var resp []byte
	i := 0
	for i < len(buf) {
		if buf[i] != '\x1b' {
			i++
			continue
		}
		if i+1 >= len(buf) {
			break
		}
		if buf[i+1] != '[' {
			i += 2
			continue
		}
		// CSI sequence: ESC [ <params> <final>
		// Parameter bytes: 0x30–0x3F  Final byte: 0x40–0x7E
		j := i + 2
		start := j
		for j < len(buf) && buf[j] >= 0x30 && buf[j] <= 0x3F {
			j++
		}
		if j >= len(buf) {
			break
		}
		final := buf[j]
		params := string(buf[start:j])
		j++

		switch {
		case final == 'c' && (params == "" || params == "0"):
			// DA1 primary device attributes
			resp = append(resp, "\x1b[?6c"...)
		case final == 'c' && params == ">", final == 'c' && params == ">0":
			// DA2 secondary device attributes
			resp = append(resp, "\x1b[>0;0;0c"...)
		case final == 'n' && params == "5":
			// DSR device status report — report ready
			resp = append(resp, "\x1b[0n"...)
		case final == 'n' && params == "6":
			// CPR cursor position report
			resp = append(resp, "\x1b[1;1R"...)
		case final == 't' && params == "18":
			// Window size query
			resp = append(resp, fmt.Sprintf("\x1b[8;%d;%dt", ptyRows, ptyCols)...)
		case final == 'q' && params == ">":
			// XTVERSION
			resp = append(resp, "\x1bP>|duckway 0\x1b\\"...)
		}
		i = j
	}
	return resp
}
