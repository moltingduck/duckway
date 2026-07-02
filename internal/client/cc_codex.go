package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type codexJSONEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Error   string `json:"error"`
	Message string `json:"message"`
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

	out, err := cmd.CombinedOutput()
	sessionID, result, isError = parseCodexJSONL(out, sid)
	if sessionID == "" {
		sessionID = sid
	}
	if err != nil {
		if result != "" {
			return sessionID, result, isError, fmt.Errorf("codex reported an error: %s", result)
		}
		return "", "", false, fmt.Errorf("codex: %w (output: %.400s)", err, out)
	}
	return sessionID, result, isError, nil
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

	evt, err := pollForCodexTmuxEvent(ctx, eventsDir, turnTS, tmuxRunTimeout)
	if err != nil {
		return "", "", false, err
	}
	out, err := os.ReadFile(evt.OutputPath)
	if err != nil {
		return "", "", false, fmt.Errorf("read codex output: %w", err)
	}
	sessionID, result, isError = parseCodexJSONL(out, evt.FallbackSessionID)
	if sessionID == "" {
		sessionID = sid
	}
	_ = os.Remove(inFlightPath)
	if evt.ExitCode != 0 {
		if result != "" {
			return sessionID, result, isError, fmt.Errorf("codex reported an error: %s", result)
		}
		return sessionID, result, isError, fmt.Errorf("codex exited with status %d (output: %.400s)", evt.ExitCode, out)
	}
	return sessionID, result, isError, nil
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

func pollForCodexTmuxEvent(ctx context.Context, eventsDir string, afterTS int64, timeout time.Duration) (*codexTmuxEvent, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(eventPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("codex tmux runner: timed out waiting for codex after %v", timeout)
		case <-tick.C:
			evt, found, err := findStopEvent(eventsDir, afterTS)
			if err != nil {
				return nil, fmt.Errorf("scan events: %w", err)
			}
			if !found {
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
	sessionID = fallbackSessionID
	sc := bufio.NewScanner(bytes.NewReader(out))
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
				sessionID = ev.ThreadID
			}
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				result = ev.Item.Text
			}
		case "turn.failed", "error":
			isError = true
			switch {
			case ev.Message != "":
				result = ev.Message
			case ev.Error != "":
				result = ev.Error
			}
		}
	}
	return sessionID, result, isError
}
