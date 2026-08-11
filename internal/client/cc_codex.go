package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type codexJSONEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Text     string `json:"text"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

type codexParseResult struct {
	SessionID string
	Result    string
	IsError   bool
	Complete  bool
}

func codexExecSandboxArgs(extraEnv []string) []string {
	sandbox := codexSandboxValue(extraEnv)
	switch sandbox {
	case "read-only", "workspace-write", "danger-full-access":
		return []string{"--sandbox", sandbox}
	case "none":
		return nil
	default:
		return []string{"--sandbox", "workspace-write"}
	}
}

func codexResumeSandboxArgs(extraEnv []string) []string {
	sandbox := codexSandboxValue(extraEnv)
	switch sandbox {
	case "read-only", "workspace-write", "danger-full-access":
		return []string{"-c", "sandbox_mode=\"" + sandbox + "\""}
	case "none":
		return nil
	default:
		return []string{"-c", "sandbox_mode=\"workspace-write\""}
	}
}

func codexSandboxValue(extraEnv []string) string {
	sandbox := envValue(extraEnv, "DUCKWAY_CC_CODEX_SANDBOX")
	if sandbox == "" {
		sandbox = os.Getenv("DUCKWAY_CC_CODEX_SANDBOX")
	}
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	return sandbox
}

func isAllowedCodexSandbox(sandbox string) bool {
	switch sandbox {
	case "read-only", "workspace-write", "danger-full-access", "none":
		return true
	default:
		return false
	}
}

func codexCommandArgs(cwd, prompt, sid string, extraEnv []string) []string {
	args := []string{"exec"}
	if sid != "" {
		args = append(args, "resume", "--json", "--skip-git-repo-check")
		args = append(args, codexResumeSandboxArgs(extraEnv)...)
		args = append(args, sid, prompt)
		return args
	}
	args = append(args, "--json", "--skip-git-repo-check")
	args = append(args, codexExecSandboxArgs(extraEnv)...)
	args = append(args, "-C", cwd, prompt)
	return args
}

// runViaCodexExec runs Codex in non-interactive JSONL mode. The first turn
// starts a new Codex thread and captures thread.started.thread_id; follow-up
// turns use `codex exec resume <thread_id>`.
func runViaCodexExec(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	args := codexCommandArgs(cwd, prompt, sid, extraEnv)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out, err := runCodexCommandUntilComplete(ctx, cmd)
	parsed := parseCodexJSONLResult(out, sid)
	sessionID, result, isError = parsed.SessionID, parsed.Result, parsed.IsError
	if sessionID == "" {
		sessionID = sid
	}
	if err != nil {
		if codexResultWinsOverExit(parsed) {
			return sessionID, result, false, nil
		}
		if result != "" {
			if summary, ok := codexTransportFailureSummary([]byte(result), nil); ok {
				return sessionID, result, isError, fmt.Errorf("codex: transport failed before completion: %s", summary)
			}
			return sessionID, result, isError, fmt.Errorf("codex reported an error: %s", result)
		}
		return "", "", false, codexExecutionError("codex", err, out)
	}
	return sessionID, result, isError, nil
}

func runCodexCommandUntilComplete(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	var outMu syncBuffer
	complete := make(chan struct{}, 1)
	read := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			outMu.writeLine(line)
			parsed := parseCodexJSONLResult(outMu.bytes(), "")
			if parsed.Complete && parsed.Result != "" {
				select {
				case complete <- struct{}{}:
				default:
				}
			}
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waitDone := make(chan error, 1)
	var readers sync.WaitGroup
	readAndDone := func(r io.Reader) {
		defer readers.Done()
		read(r)
	}
	readers.Add(2)
	go readAndDone(stdout)
	go readAndDone(stderr)
	go func() {
		readers.Wait()
		waitDone <- cmd.Wait()
	}()
	select {
	case err := <-waitDone:
		return outMu.bytes(), err
	case <-complete:
		if cmd.Process != nil {
			killProcessGroup(cmd.Process)
		}
		<-waitDone
		return outMu.bytes(), nil
	case <-ctx.Done():
		if cmd.Process != nil {
			killProcessGroup(cmd.Process)
		}
		<-waitDone
		return outMu.bytes(), ctx.Err()
	}
}

func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		_ = p.Kill()
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) writeLine(line []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(line)
	b.buf.WriteByte('\n')
}

func (b *syncBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

type codexTmuxEvent struct {
	OutputPath        string `json:"output_path"`
	ExitCode          int    `json:"exit_code"`
	FallbackSessionID string `json:"fallback_session_id"`
}

func runViaCodexTmux(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	handle := envValue(extraEnv, "DUCKWAY_CC_CHANNEL_HANDLE")
	if handle == "" {
		return "", "", false, fmt.Errorf("codex tmux runner: missing DUCKWAY_CC_CHANNEL_HANDLE in extraEnv")
	}
	ensureTmuxSessionNaming(handle)
	sess := tmuxSessionName(handle)
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		return "", "", false, err
	}
	eventsDir := filepath.Join(chDir, "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		return "", "", false, fmt.Errorf("mkdir events: %w", err)
	}

	turnTS := time.Now().UnixNano()
	promptPath := filepath.Join(chDir, "codex-prompt.txt")
	outputPath := filepath.Join(chDir, fmt.Sprintf("codex-%d.jsonl", turnTS))
	launchPath := filepath.Join(chDir, "codex-launch.sh")
	inFlightPath := filepath.Join(chDir, "in-flight.json")

	if err := os.WriteFile(promptPath, []byte(prompt), 0600); err != nil {
		return "", "", false, fmt.Errorf("write codex prompt: %w", err)
	}
	if err := writeInFlight(inFlightPath, handle, envValue(extraEnv, "DUCKWAY_CC_MESSAGE_ID"), turnTS); err != nil {
		return "", "", false, err
	}
	if err := writeCodexTmuxLaunchScript(launchPath, bin, cwd, promptPath, outputPath, eventsDir, sid, extraEnv); err != nil {
		return "", "", false, err
	}

	if !tmuxHasSession(sess) {
		if err := tmuxNewSession(sess, cwd, launchPath, extraEnv); err != nil {
			return "", "", false, err
		}
	} else if err := tmuxRespawnPane(sess, cwd, launchPath, extraEnv); err != nil {
		return "", "", false, err
	}

	evt, err := pollForCodexTmuxEvent(ctx, eventsDir, turnTS, outputPath, sid, 0)
	if err != nil {
		return "", "", false, err
	}
	out, err := os.ReadFile(evt.OutputPath)
	if err != nil {
		return "", "", false, fmt.Errorf("read codex output: %w", err)
	}
	_ = os.Remove(inFlightPath)
	return resolveCodexTmuxExit(out, evt.FallbackSessionID, sid, evt.ExitCode)
}

func codexResultWinsOverExit(parsed codexParseResult) bool {
	return parsed.Result != "" && !parsed.IsError
}

func resolveCodexTmuxExit(out []byte, fallbackSessionID, sid string, exitCode int) (sessionID, result string, isError bool, err error) {
	parsed := parseCodexJSONLResult(out, fallbackSessionID)
	sessionID, result, isError = parsed.SessionID, parsed.Result, parsed.IsError
	if sessionID == "" {
		sessionID = sid
	}
	if exitCode == 0 {
		return sessionID, result, isError, nil
	}
	if codexResultWinsOverExit(parsed) {
		return sessionID, result, false, nil
	}
	if result != "" {
		if summary, ok := codexTransportFailureSummary([]byte(result), nil); ok {
			return sessionID, result, isError, fmt.Errorf("codex exited with status %d: transport failed before completion: %s", exitCode, summary)
		}
		return sessionID, result, isError, fmt.Errorf("codex reported an error: %s", result)
	}
	return sessionID, result, isError, codexExecutionError(fmt.Sprintf("codex exited with status %d", exitCode), nil, out)
}

func codexExecutionError(prefix string, err error, out []byte) error {
	if summary, ok := codexTransportFailureSummary(out, err); ok {
		return fmt.Errorf("%s: transport failed before completion: %s", prefix, summary)
	}
	if err != nil {
		return fmt.Errorf("%s: %w (output: %.400s)", prefix, err, out)
	}
	return fmt.Errorf("%s (output: %.400s)", prefix, out)
}

func codexTransportFailureSummary(out []byte, err error) (string, bool) {
	var lines []string
	if err != nil {
		lines = append(lines, err.Error())
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "{") {
			continue
		}
		lines = append(lines, line)
	}

	var matched []string
	for _, line := range lines {
		if looksLikeCodexTransportFailure(line) {
			matched = append(matched, line)
		}
	}
	if len(matched) == 0 {
		return "", false
	}
	const maxLines = 3
	if len(matched) > maxLines {
		matched = matched[:maxLines]
	}
	return truncateForDiscordLog(strings.Join(matched, "; "), 700), true
}

func looksLikeCodexTransportFailure(s string) bool {
	s = strings.ToLower(s)
	patterns := []string{
		"attack attempt detected",
		"websocket",
		"stream disconnected",
		"transport",
		"proxyconnect",
		"connection reset",
		"connection refused",
		"connection timed out",
		"tls handshake",
		"deadline exceeded",
		"unexpected eof",
		"network connection interrupted",
		"rate limit reached",
		"tokens per min",
	}
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func writeCodexTmuxLaunchScript(path, bin, cwd, promptPath, outputPath, eventsDir, sid string, extraEnv []string) error {
	outputJSON, _ := json.Marshal(outputPath)
	sidJSON, _ := json.Marshal(sid)
	args := append([]string{bin}, codexCommandArgs(cwd, "-", sid, extraEnv)...)

	var sb strings.Builder
	q := shellSingleQuote
	sb.WriteString("#!/bin/sh\n")
	sb.WriteString("set +e\n")
	sb.WriteString("printf '%s\\n' '[duckway] starting Codex turn in tmux...'\n")
	sb.WriteString("cd " + q(cwd) + " || exit 1\n")
	sb.WriteString("out=" + q(outputPath) + "\n")
	sb.WriteString("prompt=" + q(promptPath) + "\n")
	sb.WriteString("events=" + q(eventsDir) + "\n")
	sb.WriteString("mkdir -p \"$events\"\n")
	sb.WriteString("rm -f \"$out\"\n")
	sb.WriteString("set --")
	for _, a := range args {
		sb.WriteByte(' ')
		sb.WriteString(q(a))
	}
	sb.WriteString("\n")
	sb.WriteString("\"$@\" < \"$prompt\" > \"$out\" 2>&1\n")
	sb.WriteString("rc=$?\n")
	sb.WriteString("cat \"$out\"\n")
	sb.WriteString("ts=$(date +%s%N)\n")
	sb.WriteString("final=\"$events/${ts}.stop.json\"\n")
	sb.WriteString("tmp=\"${final}.tmp\"\n")
	sb.WriteString("printf '{\"output_path\":%s,\"fallback_session_id\":%s,\"exit_code\":%s}' " +
		shellSingleQuote(string(outputJSON)) + " " + shellSingleQuote(string(sidJSON)) + " \"$rc\" > \"$tmp\"\n")
	sb.WriteString("mv \"$tmp\" \"$final\"\n")
	sb.WriteString("printf '%s\\n' '[duckway] Codex turn finished. Attach session remains open for inspection.'\n")
	sb.WriteString("exec ${SHELL:-/bin/sh} -i\n")
	return os.WriteFile(path, []byte(sb.String()), 0700)
}

func pollForCodexTmuxEvent(ctx context.Context, eventsDir string, afterTS int64, outputPath, fallbackSessionID string, timeout time.Duration) (*codexTmuxEvent, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	tick := time.NewTicker(eventPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("codex tmux runner: timed out waiting for codex after %v", timeout)
		case <-tick.C:
			evt, found, err := findStopEvent(eventsDir, afterTS)
			if err != nil {
				return nil, fmt.Errorf("scan events: %w", err)
			}
			if !found {
				if outputPath != "" {
					complete, err := codexOutputComplete(outputPath, fallbackSessionID)
					if err != nil {
						return nil, err
					}
					if complete {
						return &codexTmuxEvent{OutputPath: outputPath, FallbackSessionID: fallbackSessionID, ExitCode: 0}, nil
					}
				}
				continue
			}
			parsed, ok := parseCodexTmuxEventPayload(evt.payload)
			if !ok {
				_ = os.Remove(evt.path)
				continue
			}
			_ = os.Remove(evt.path)
			return parsed, nil
		}
	}
}

func parseCodexTmuxEventPayload(payload string) (*codexTmuxEvent, bool) {
	var ev codexTmuxEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil, false
	}
	if ev.OutputPath == "" {
		return nil, false
	}
	return &ev, true
}

func parseCodexJSONL(out []byte, fallbackSessionID string) (sessionID, result string, isError bool) {
	parsed := parseCodexJSONLResult(out, fallbackSessionID)
	return parsed.SessionID, parsed.Result, parsed.IsError
}

func parseCodexJSONLResult(out []byte, fallbackSessionID string) codexParseResult {
	parsed := codexParseResult{SessionID: fallbackSessionID}
	haveAgentMessage := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev codexJSONEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			if ev.ThreadID != "" {
				parsed.SessionID = ev.ThreadID
			}
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				parsed.Result = ev.Item.Text
				haveAgentMessage = true
				parsed.IsError = false
			}
		case "final":
			text := ev.Text
			if text == "" {
				text = ev.Message
			}
			if text != "" {
				parsed.Result = text
				haveAgentMessage = true
				parsed.IsError = false
			}
		case "task_complete", "turn.completed":
			parsed.Complete = true
		case "error":
			if !haveAgentMessage {
				parsed.IsError = true
				switch {
				case ev.Message != "":
					parsed.Result = ev.Message
				case ev.Error != "":
					parsed.Result = ev.Error
				}
			}
		case "turn.failed":
			errText := ev.Message
			if errText == "" {
				errText = ev.Error
			}
			if errText != "" && !haveAgentMessage {
				parsed.IsError = true
				parsed.Result = errText
			} else if !haveAgentMessage && parsed.Result == "" {
				parsed.IsError = true
				parsed.Result = "codex turn failed"
			}
		}
	}
	return parsed
}

func codexOutputComplete(outputPath, fallbackSessionID string) (bool, error) {
	out, err := os.ReadFile(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read codex output: %w", err)
	}
	parsed := parseCodexJSONLResult(out, fallbackSessionID)
	return parsed.Complete && parsed.Result != "", nil
}
