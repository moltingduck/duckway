package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion"
)

func runViaClaudePTY(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	out, err := runCommandInDucklionPTY(ctx, "claude", bin, cwd, claudePrintCommandArgs(prompt, sid), extraEnv, claudePTYOutputComplete)
	if err != nil {
		return "", "", false, err
	}
	out = normalizePTYCommandOutput(out)
	var pr printResult
	if jsonErr := json.Unmarshal(out, &pr); jsonErr != nil {
		return "", "", false, fmt.Errorf("parse pty claude output: %w (raw: %.200s)", jsonErr, out)
	}
	return pr.SessionID, pr.Result, pr.IsError, nil
}

func runViaCodexPTY(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	out, err := runCommandInDucklionPTY(ctx, "codex", bin, cwd, codexCommandArgs(cwd, prompt, sid, extraEnv), extraEnv, func(out []byte) bool {
		parsed := parseCodexJSONLResult(normalizePTYCommandOutput(out), sid)
		return parsed.Complete && parsed.Result != ""
	})
	out = normalizePTYCommandOutput(out)
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
				return sessionID, result, isError, fmt.Errorf("codex pty: transport failed before completion: %s", summary)
			}
			return sessionID, result, isError, fmt.Errorf("codex reported an error: %s", result)
		}
		return "", "", false, codexExecutionError("codex pty", err, out)
	}
	if !parsed.Complete {
		if result != "" && isError {
			if summary, ok := codexTransportFailureSummary([]byte(result), nil); ok {
				return sessionID, result, isError, fmt.Errorf("codex pty ended before completion: transport failed before completion: %s", summary)
			}
			return sessionID, result, isError, fmt.Errorf("codex reported an error: %s", result)
		}
		return sessionID, result, isError, codexExecutionError("codex pty ended before completion", nil, out)
	}
	if result == "" {
		return sessionID, result, isError, codexExecutionError("codex pty completed without result", nil, out)
	}
	return sessionID, result, isError, nil
}

func runCommandInDucklionPTY(ctx context.Context, agentType, bin, cwd string, args, extraEnv []string, complete func([]byte) bool) ([]byte, error) {
	configDir := envValue(extraEnv, "DUCKWAY_CONFIG_DIR")
	if configDir == "" {
		configDir = DefaultConfigDir()
	}
	exe, err := exec.LookPath("ducklion")
	if err != nil {
		return nil, fmt.Errorf("ducklion not found in PATH; install standalone ducklion or run cc watch with --tmux/DUCKWAY_CC_USE_TMUX=1")
	}
	handle := envValue(extraEnv, "DUCKWAY_CC_CHANNEL_HANDLE")
	if handle == "" {
		handle = "cc"
	}
	name := safeCCPTYSessionName(handle, time.Now().UnixNano())
	root := filepath.Join(configDir, "ccpty")
	manager := ducklion.NewManager(root, exe)
	command := append([]string{bin}, args...)
	rec, err := manager.Start(ducklion.StartOptions{Name: name, AgentType: agentType, Cwd: cwd, Env: extraEnv, Command: wrapPTYCommand(command)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = manager.Stop(name) }()
	deadline := time.Now().Add(ptyDefaultTimeout)
	for {
		select {
		case <-ctx.Done():
			_ = manager.Stop(name)
			return nil, ctx.Err()
		default:
		}
		out := []byte(mustReadDucklion(manager, name))
		if complete != nil && complete(out) {
			return out, nil
		}
		records, _ := manager.List()
		running := false
		for _, item := range records {
			if item.Name == rec.Name && item.Status == ducklion.StatusRunning {
				running = true
				break
			}
		}
		if !running {
			time.Sleep(100 * time.Millisecond)
			out = []byte(mustReadDucklion(manager, name))
			return out, nil
		}
		if time.Now().After(deadline) {
			return out, fmt.Errorf("%s pty runner timed out after %v", agentType, ptyDefaultTimeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func claudePTYOutputComplete(out []byte) bool {
	var pr printResult
	return json.Unmarshal(normalizePTYCommandOutput(out), &pr) == nil && pr.SessionID != ""
}

func wrapPTYCommand(command []string) []string {
	script := "set +e\n" +
		"\"$@\"\n" +
		"rc=$?\n" +
		"sleep 0.2\n" +
		"exit \"$rc\"\n"
	return append([]string{"sh", "-lc", script, "duckway-pty"}, command...)
}

func mustReadDucklion(manager *ducklion.Manager, name string) string {
	out, err := manager.Read(name, 4000)
	if err != nil {
		return ""
	}
	return out
}

func normalizePTYCommandOutput(out []byte) []byte {
	out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\r"), nil)
	return bytes.TrimSpace(out)
}

func safeCCPTYSessionName(handle string, ts int64) string {
	var readable strings.Builder
	for _, r := range strings.ToLower(handle) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			readable.WriteRune(r)
		} else {
			readable.WriteByte('-')
		}
	}
	base := strings.Trim(readable.String(), "-")
	if base == "" {
		base = "cc"
	}
	if len(base) > 14 {
		base = base[:14]
	}
	sum := sha256.Sum256([]byte(handle))
	return fmt.Sprintf("cc-%s-%s-%s", base, hex.EncodeToString(sum[:4]), strconv.FormatInt(ts, 36))
}
