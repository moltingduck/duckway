package ducklioncli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion"
	"github.com/hackerduck/duckway/internal/duckwayconfig"
	"github.com/hackerduck/duckway/internal/projectregistry"
	"github.com/hackerduck/duckway/internal/version"
	"golang.org/x/sys/unix"
)

type SessionManager interface {
	List() ([]ducklion.Record, error)
	Start(ducklion.StartOptions) (*ducklion.Record, error)
	Send(name, text string) error
	Read(name string, lines int) (string, error)
	Attach(name string, in io.Reader, out io.Writer) error
	Stop(name string) error
}

type SessionOutput struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	AgentType string `json:"agent_type"`
	Cwd       string `json:"cwd"`
	Backend   string `json:"backend"`
	PID       int    `json:"pid,omitempty"`
	LastLine  string `json:"last_line,omitempty"`
	TailHash  string `json:"tail_hash,omitempty"`
	Error     string `json:"error,omitempty"`
}

func Main(args []string, stdout io.Writer) {
	if len(args) > 0 && args[0] == "__supervise" {
		opts, err := ducklion.ParseSupervisorArgs(args[1:])
		if err != nil {
			log.Fatal(err)
		}
		if err := ducklion.RunSupervisor(opts); err != nil {
			log.Fatal(err)
		}
		return
	}
	manager := ducklion.NewManager(ducklion.DefaultRoot(), "")
	if err := Run(manager, args, stdout); err != nil {
		log.Fatal(err)
	}
}

func Run(manager SessionManager, args []string, out io.Writer) error {
	if len(args) == 0 {
		PrintUsage(out)
		return fmt.Errorf("command is required")
	}
	switch args[0] {
	case "list":
		return runList(manager, args[1:], out)
	case "projects":
		return runProjects(args[1:], out)
	case "start":
		opts, err := ParseStart(args[1:])
		if err != nil {
			return err
		}
		rec, err := manager.Start(opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Started %s (%s, pid %d)\n", rec.Name, rec.AgentType, rec.PID)
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
		restore, err := makeRawIfTTY(os.Stdin)
		if err != nil {
			return err
		}
		defer restore()
		return manager.Attach(args[1], os.Stdin, out)
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
		PrintUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown ducklion command: %s", args[0])
	}
}

type ProjectOutput struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

func runProjects(args []string, out io.Writer) error {
	jsonOut := false
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOut = true
		default:
			return fmt.Errorf("unknown projects option: %s", arg)
		}
	}
	projects, err := projectregistry.NewStore(duckwayconfig.DefaultConfigDir()).List()
	if err != nil {
		return err
	}
	result := make([]ProjectOutput, 0, len(projects))
	for _, p := range projects {
		result = append(result, ProjectOutput{Name: p.Name, Path: p.Path, Source: "duckway-client"})
	}
	if jsonOut {
		return json.NewEncoder(out).Encode(result)
	}
	if len(result) == 0 {
		fmt.Fprintln(out, "No saved Duckway projects.")
		return nil
	}
	fmt.Fprintf(out, "%-18s %s\n", "NAME", "PATH")
	for _, p := range result {
		fmt.Fprintf(out, "%-18s %s\n", p.Name, p.Path)
	}
	return nil
}

func runList(manager SessionManager, args []string, out io.Writer) error {
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
	result := make([]SessionOutput, 0, len(records))
	for _, rec := range records {
		item := SessionOutput{Name: rec.Name, Status: rec.Status, AgentType: rec.AgentType, Cwd: rec.Cwd, Backend: "pty", PID: rec.PID}
		if tailLines > 0 && rec.Status == ducklion.StatusRunning {
			text, err := manager.Read(rec.Name, tailLines)
			if err != nil {
				item.Error = err.Error()
			} else {
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
	fmt.Fprintf(out, "%-18s %-10s %-8s %-12s %s\n", "NAME", "STATUS", "BACKEND", "AGENT", "CWD")
	for _, item := range result {
		fmt.Fprintf(out, "%-18s %-10s %-8s %-12s %s\n", item.Name, item.Status, item.Backend, item.AgentType, item.Cwd)
	}
	return nil
}

func ParseStart(args []string) (ducklion.StartOptions, error) {
	opts := ducklion.StartOptions{AgentType: "shell"}
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

func makeRawIfTTY(f *os.File) (func(), error) {
	fd := int(f.Fd())
	oldState, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}, nil
	}
	raw := *oldState
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, oldState) }, nil
}

func PrintUsage(out io.Writer) {
	fmt.Fprintln(out, `ducklion — remote PTY session supervisor

Usage:
  ducklion list [--json] [--tail-lines N]
  ducklion projects [--json]
  ducklion start --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklion read <name> [--lines N] [--json]
  ducklion send <name> <text>
  ducklion attach <name>
  ducklion stop <name>
  ducklion version`)
}
