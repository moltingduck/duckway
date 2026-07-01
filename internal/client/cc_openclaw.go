package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type openClawResult struct {
	Output  string `json:"output"`
	Result  string `json:"result"`
	Reply   string `json:"reply"`
	Message string `json:"message"`
	Error   string `json:"error"`
	Status  string `json:"status"`
}

func runViaOpenClaw(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	handle := envValue(extraEnv, "DUCKWAY_CC_CHANNEL_HANDLE")
	if handle == "" {
		handle = filepath.Base(strings.TrimRight(cwd, string(os.PathSeparator)))
	}
	sessionKey := sid
	if sessionKey == "" || !strings.HasPrefix(sessionKey, "duckway:") {
		sessionKey = "duckway:" + handle
	}
	agentID := strings.TrimSpace(os.Getenv("DUCKWAY_CC_OPENCLAW_AGENT"))
	if agentID == "" {
		agentID = "default"
	}
	promptFile, err := os.CreateTemp("", "duckway-openclaw-*.md")
	if err != nil {
		return sessionKey, "", false, fmt.Errorf("openclaw prompt temp file: %w", err)
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if _, err := promptFile.WriteString(prompt); err != nil {
		_ = promptFile.Close()
		return sessionKey, "", false, fmt.Errorf("write openclaw prompt: %w", err)
	}
	if err := promptFile.Close(); err != nil {
		return sessionKey, "", false, fmt.Errorf("close openclaw prompt: %w", err)
	}

	args := []string{
		"agent",
		"--agent", agentID,
		"--session-key", sessionKey,
		"--message-file", promptPath,
		"--json",
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), extraEnv...)

	out, err := cmd.CombinedOutput()
	result, isError = parseOpenClawOutput(out)
	if err != nil {
		if result != "" {
			return sessionKey, result, true, fmt.Errorf("openclaw reported an error: %s", result)
		}
		return sessionKey, "", false, fmt.Errorf("openclaw: %w (output: %.400s)", err, out)
	}
	return sessionKey, result, isError, nil
}

func parseOpenClawOutput(out []byte) (string, bool) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "", false
	}
	var payload openClawResult
	if err := json.Unmarshal(out, &payload); err == nil {
		text := firstOpenClawText(payload.Output, payload.Result, payload.Reply, payload.Message, payload.Error)
		isError := payload.Error != "" || strings.EqualFold(payload.Status, "error") || strings.EqualFold(payload.Status, "failed")
		if text != "" {
			return text, isError
		}
	}
	return trimmed, false
}

func firstOpenClawText(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
