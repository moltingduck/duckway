package ducklord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

type RemoteProject struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

type Runner struct{}

type AttachSession struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Done   <-chan error
	cmd    *exec.Cmd
}

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

func (Runner) Projects(ctx context.Context, c Client) ([]RemoteProject, error) {
	out, err := sshOutput(ctx, c, "projects", "--json")
	if err != nil {
		return nil, err
	}
	var projects []RemoteProject
	if err := json.Unmarshal(out, &projects); err != nil {
		return nil, fmt.Errorf("parse ducklion projects from %s: %w", c.Name, err)
	}
	return projects, nil
}

func (Runner) Attach(c Client, name string) error {
	if !SafeIdentifier(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	args := SSHArgs(c, true, c.DucklionArgs("attach", name)...)
	sshParts := c.SSHCommandParts()
	cmd := exec.Command(sshParts[0], append(sshParts[1:], args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh attach to %s: %w", c.Name, err)
	}
	return nil
}

func (Runner) AttachStream(ctx context.Context, c Client, name string) (*AttachSession, error) {
	if !SafeIdentifier(name) {
		return nil, fmt.Errorf("invalid session name %q", name)
	}
	args := SSHArgs(c, false, c.DucklionArgs("attach", name)...)
	sshParts := c.SSHCommandParts()
	cmd := exec.CommandContext(ctx, sshParts[0], append(sshParts[1:], args...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh attach to %s: %w", c.Name, err)
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		done <- err
	}()
	return &AttachSession{Stdin: stdin, Stdout: stdout, Done: done, cmd: cmd}, nil
}

func sshOutput(ctx context.Context, c Client, ducklionArgs ...string) ([]byte, error) {
	return sshOutputRaw(ctx, c, c.DucklionArgs(ducklionArgs...)...)
}

func sshOutputRaw(ctx context.Context, c Client, remoteArgs ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := SSHArgs(c, false, remoteArgs...)
	sshParts := c.SSHCommandParts()
	cmd := exec.CommandContext(ctx, sshParts[0], append(sshParts[1:], args...)...)
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
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
	}
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
