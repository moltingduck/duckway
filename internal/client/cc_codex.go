package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type codexJSONEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Error string `json:"error"`
}

// runViaCodexExec runs Codex in non-interactive JSONL mode. The first turn
// starts a new Codex thread and captures thread.started.thread_id; follow-up
// turns use `codex exec resume <thread_id>`.
func runViaCodexExec(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	args := []string{"exec"}
	if sid != "" {
		args = append(args, "resume", "--json", sid, prompt)
	} else {
		args = append(args, "--json", "--sandbox", "workspace-write", "-C", cwd, prompt)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), extraEnv...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", false, fmt.Errorf("codex: %w (output: %.400s)", err, out)
	}
	sessionID, result, isError = parseCodexJSONL(out, sid)
	if sessionID == "" {
		sessionID = sid
	}
	return sessionID, result, isError, nil
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
			if ev.Error != "" {
				result = ev.Error
			}
		}
	}
	return sessionID, result, isError
}
