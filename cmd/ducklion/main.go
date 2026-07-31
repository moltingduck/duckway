package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/hackerduck/duckway/internal/client"
	"github.com/hackerduck/duckway/internal/version"
)

var attachExec = func(args []string) error {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux is not installed or not on PATH")
	}
	return syscall.Exec(tmuxPath, append([]string{"tmux"}, args...), os.Environ())
}

type sessionManager interface {
	List() ([]client.SessionRecord, error)
	Start(client.SessionStartOptions) (*client.SessionRecord, error)
	Send(name, text string) error
	Read(name string, lines int) (string, error)
	Stop(name string) error
	AttachArgs(name string) ([]string, error)
}

type sessionOutput struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	AgentType   string `json:"agent_type"`
	Cwd         string `json:"cwd"`
	TmuxSession string `json:"tmux_session"`
	LastLine    string `json:"last_line,omitempty"`
	TailHash    string `json:"tail_hash,omitempty"`
}

func main() {
	manager := client.NewSessionManager(client.DefaultConfigDir(), nil)
	if err := run(manager, os.Args[1:], os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(manager sessionManager, args []string, out io.Writer) error {
	if len(args) == 0 {
		printUsage(out)
		return fmt.Errorf("command is required")
	}
	switch args[0] {
	case "list":
		return runList(manager, args[1:], out)
	case "start":
		opts, err := parseStart(args[1:])
		if err != nil {
			return err
		}
		rec, err := manager.Start(opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Started %s (%s)\n", rec.Name, rec.AgentType)
		return nil
	case "read":
		name, lines, jsonOut, err := parseRead(args[1:])
		if err != nil {
			return err
		}
		text, err := manager.Read(name, lines)
		if err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(out).Encode(map[string]string{"name": name, "output": text})
		}
		fmt.Fprint(out, text)
		return nil
	case "send":
		if len(args) < 3 {
			return fmt.Errorf("usage: ducklion send <name> <text>")
		}
		return manager.Send(args[1], strings.Join(args[2:], " "))
	case "attach":
		if len(args) != 2 {
			return fmt.Errorf("usage: ducklion attach <name>")
		}
		attachArgs, err := manager.AttachArgs(args[1])
		if err != nil {
			return err
		}
		return attachExec(attachArgs)
	case "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: ducklion stop <name>")
		}
		if err := manager.Stop(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "Stopped %s\n", args[1])
		return nil
	case "version", "--version", "-v":
		fmt.Fprintln(out, "ducklion", version.Get())
		return nil
	case "help", "--help", "-h":
		printUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown ducklion command: %s", args[0])
	}
}

func runList(manager sessionManager, args []string, out io.Writer) error {
	jsonOut := false
	tailLines := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--tail-lines":
			if i+1 >= len(args) {
				return fmt.Errorf("--tail-lines requires a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 || n > 200 {
				return fmt.Errorf("invalid --tail-lines value")
			}
			tailLines = n
			i++
		default:
			return fmt.Errorf("unknown list option: %s", args[i])
		}
	}
	records, err := manager.List()
	if err != nil {
		return err
	}
	result := make([]sessionOutput, 0, len(records))
	for _, rec := range records {
		item := sessionOutput{Name: rec.Name, Status: rec.Status, AgentType: rec.AgentType, Cwd: rec.Cwd, TmuxSession: rec.TmuxSession}
		if tailLines > 0 && rec.Status == client.SessionStatusRunning {
			if text, err := manager.Read(rec.Name, tailLines); err == nil {
				item.LastLine = lastNonEmptyLine(text)
				sum := sha256.Sum256([]byte(text))
				item.TailHash = hex.EncodeToString(sum[:])
			}
		}
		result = append(result, item)
	}
	if jsonOut {
		return json.NewEncoder(out).Encode(result)
	}
	if len(result) == 0 {
		fmt.Fprintln(out, "No ducklion sessions.")
		return nil
	}
	fmt.Fprintf(out, "%-18s %-10s %-12s %s\n", "NAME", "STATUS", "AGENT", "CWD")
	for _, item := range result {
		fmt.Fprintf(out, "%-18s %-10s %-12s %s\n", item.Name, item.Status, item.AgentType, item.Cwd)
	}
	return nil
}

func parseStart(args []string) (client.SessionStartOptions, error) {
	opts := client.SessionStartOptions{Kind: "terminal", AgentType: "shell"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			opts.Command = append([]string(nil), args[i+1:]...)
			i = len(args)
		case "--name", "-n":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--name requires a value")
			}
			opts.Name = args[i+1]
			i++
		case "--agent":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--agent requires a value")
			}
			opts.AgentType = args[i+1]
			i++
		case "--cwd", "-C":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--cwd requires a value")
			}
			opts.Cwd = args[i+1]
			i++
		default:
			return opts, fmt.Errorf("unknown start option: %s", args[i])
		}
	}
	if opts.Name == "" {
		return opts, fmt.Errorf("--name is required")
	}
	if len(opts.Command) == 0 {
		return opts, fmt.Errorf("command is required after --")
	}
	return opts, nil
}

func parseRead(args []string) (name string, lines int, jsonOut bool, err error) {
	lines = 120
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lines", "-n":
			if i+1 >= len(args) {
				return "", 0, false, fmt.Errorf("--lines requires a value")
			}
			lines, err = strconv.Atoi(args[i+1])
			if err != nil || lines <= 0 || lines > 5000 {
				return "", 0, false, fmt.Errorf("invalid --lines value")
			}
			i++
		case "--json":
			jsonOut = true
		default:
			if name != "" {
				return "", 0, false, fmt.Errorf("usage: ducklion read <name> [--lines N] [--json]")
			}
			name = args[i]
		}
	}
	if name == "" {
		return "", 0, false, fmt.Errorf("usage: ducklion read <name> [--lines N] [--json]")
	}
	return name, lines, jsonOut, nil
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, `ducklion - remote agent session endpoint

Usage:
  ducklion list [--json] [--tail-lines N]
  ducklion start --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklion read <name> [--lines N] [--json]
  ducklion send <name> <text>
  ducklion attach <name>
  ducklion stop <name>
  ducklion version`)
}
