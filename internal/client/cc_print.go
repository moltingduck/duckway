package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type printResult struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

// runViaPrint runs claude with --print --output-format json, which is the
// reliable path for getting structured output with session_id. Supports
// multi-turn sessions via the sid parameter (passed as --resume).
func runViaPrint(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	args := []string{
		"--print",
		"--output-format", "json",
		"--dangerously-skip-permissions",
	}
	if sid != "" {
		args = append(args, "--resume", sid)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), extraEnv...)

	out, err := cmd.Output()
	if err != nil {
		return "", "", false, fmt.Errorf("claude: %w", err)
	}

	var pr printResult
	if jsonErr := json.Unmarshal(out, &pr); jsonErr != nil {
		return "", "", false, fmt.Errorf("parse output: %w (raw: %.200s)", jsonErr, out)
	}
	return pr.SessionID, pr.Result, pr.IsError, nil
}
