package ducklord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type RemoteSession struct {
	Client      string `json:"client,omitempty"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	AgentType   string `json:"agent_type"`
	Cwd         string `json:"cwd"`
	TmuxSession string `json:"tmux_session"`
	LastLine    string `json:"last_line,omitempty"`
	TailHash    string `json:"tail_hash,omitempty"`
	Group       string `json:"group,omitempty"`
	Updated     bool   `json:"updated,omitempty"`
	Error       string `json:"error,omitempty"`
}

type Runner struct{}

func (Runner) Sessions(ctx context.Context, c Client, tailLines int) ([]RemoteSession, error) {
	if tailLines <= 0 {
		tailLines = 8
	}
	out, err := sshOutput(ctx, c, "list", "--json", "--tail-lines", strconv.Itoa(tailLines))
	if err != nil {
		return nil, err
	}
	var sessions []RemoteSession
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, fmt.Errorf("parse ducklion sessions from %s: %w", c.Name, err)
	}
	for i := range sessions {
		sessions[i].Client = c.Name
		sessions[i].Group = c.Group
	}
	return sessions, nil
}

func (Runner) Read(ctx context.Context, c Client, name string, lines int) (string, error) {
	if !SafeIdentifier(name) {
		return "", fmt.Errorf("invalid session name %q", name)
	}
	if lines <= 0 {
		lines = 120
	}
	out, err := sshOutput(ctx, c, "read", name, "--lines", strconv.Itoa(lines))
	return string(out), err
}

func (Runner) Send(ctx context.Context, c Client, name, text string) error {
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "send", name, text)
	return err
}

func (Runner) Start(ctx context.Context, c Client, args []string) error {
	_, err := sshOutput(ctx, c, append([]string{"start"}, args...)...)
	return err
}

func (Runner) Stop(ctx context.Context, c Client, name string) error {
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	_, err := sshOutput(ctx, c, "stop", name)
	return err
}

func (Runner) Attach(c Client, name string) error {
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	sshPath, err := exec.LookPath(c.SSH)
	if err != nil {
		return fmt.Errorf("ssh command %q not found: %w", c.SSH, err)
	}
	args := SSHArgs(c, true, c.Ducklion, "attach", name)
	return syscall.Exec(sshPath, append([]string{c.SSH}, args...), os.Environ())
}

func sshOutput(ctx context.Context, c Client, ducklionArgs ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := SSHArgs(c, false, append([]string{c.Ducklion}, ducklionArgs...)...)
	cmd := exec.CommandContext(ctx, c.SSH, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("ssh to %s timed out", c.Name)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ssh to %s: %s", c.Name, msg)
	}
	return out, nil
}

func SSHArgs(c Client, tty bool, remoteArgs ...string) []string {
	args := []string{"-o", "BatchMode=yes"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, c.Target(), remoteCommand(remoteArgs))
	return args
}

func remoteCommand(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&|;<>(){}[]*?!#~=") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
