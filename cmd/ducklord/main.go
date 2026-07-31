package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
	"github.com/hackerduck/duckway/internal/version"
	"golang.org/x/sys/unix"
)

type remoteRunner interface {
	Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error)
	Read(context.Context, ducklord.Client, string, int) (string, error)
	Send(context.Context, ducklord.Client, string, string) error
	Start(context.Context, ducklord.Client, []string) error
	Stop(context.Context, ducklord.Client, string) error
	Attach(ducklord.Client, string) error
}

func main() {
	if err := run(os.Args[1:], os.Stdout, ducklord.Runner{}); err != nil {
		log.Fatal(err)
	}
}

func run(args []string, out io.Writer, runner remoteRunner) error {
	if len(args) == 0 {
		printUsage(out)
		return fmt.Errorf("command is required")
	}
	switch args[0] {
	case "clients":
		cfg, _, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		return printClients(out, cfg)
	case "sessions":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord sessions <client> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		sessions, err := runner.Sessions(context.Background(), c, 8)
		if err != nil {
			return err
		}
		return printSessions(out, sessions)
	case "read":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		clientName, sessionName, lines, err := parseReadArgs(rest)
		if err != nil {
			return err
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		text, err := runner.Read(context.Background(), c, sessionName, lines)
		if err != nil {
			return err
		}
		fmt.Fprint(out, text)
		return nil
	case "send":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 3 {
			return fmt.Errorf("usage: ducklord send <client> <session> <text>")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Send(context.Background(), c, rest[1], strings.Join(rest[2:], " "))
	case "start":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 2 {
			return fmt.Errorf("usage: ducklord start <client> --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Start(context.Background(), c, rest[1:])
	case "stop":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord stop <client> <session>")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Stop(context.Background(), c, rest[1])
	case "attach":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord attach <client> <session>")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Attach(c, rest[1])
	case "tui":
		cfgPath, refresh, err := parseTUIFlags(args[1:])
		if err != nil {
			return err
		}
		cfg, err := ducklord.LoadConfig(cfgPath)
		if err != nil {
			return err
		}
		return runTUI(cfg, runner, refresh)
	case "version", "--version", "-v":
		fmt.Fprintln(out, "ducklord", version.Get())
		return nil
	case "help", "--help", "-h":
		printUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown ducklord command: %s", args[0])
	}
}

func loadWithFlags(args []string) (*ducklord.Config, []string, error) {
	path := ""
	rest := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--config requires a value")
			}
			path = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	cfg, err := ducklord.LoadConfig(path)
	return cfg, rest, err
}

func parseTUIFlags(args []string) (string, time.Duration, error) {
	path := ""
	refresh := 2 * time.Second
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return "", 0, fmt.Errorf("--config requires a value")
			}
			path = args[i+1]
			i++
		case "--refresh":
			if i+1 >= len(args) {
				return "", 0, fmt.Errorf("--refresh requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < time.Second {
				return "", 0, fmt.Errorf("invalid --refresh value")
			}
			refresh = d
			i++
		default:
			return "", 0, fmt.Errorf("unknown tui option: %s", args[i])
		}
	}
	return path, refresh, nil
}

func mustClient(cfg *ducklord.Config, name string) (ducklord.Client, error) {
	c, ok := cfg.Client(name)
	if !ok {
		return ducklord.Client{}, fmt.Errorf("unknown client %q", name)
	}
	return c, nil
}

func printClients(out io.Writer, cfg *ducklord.Config) error {
	fmt.Fprintf(out, "%-18s %-12s %-24s %s\n", "NAME", "GROUP", "TARGET", "DUCKLION")
	for _, c := range cfg.Clients {
		fmt.Fprintf(out, "%-18s %-12s %-24s %s\n", c.Name, c.Group, c.Target(), c.Ducklion)
	}
	return nil
}

func printSessions(out io.Writer, sessions []ducklord.RemoteSession) error {
	if len(sessions) == 0 {
		fmt.Fprintln(out, "No remote sessions.")
		return nil
	}
	fmt.Fprintf(out, "%-12s %-18s %-10s %-12s %s\n", "CLIENT", "SESSION", "STATUS", "AGENT", "LAST")
	for _, s := range sessions {
		fmt.Fprintf(out, "%-12s %-18s %-10s %-12s %s\n", s.Client, s.Name, s.Status, s.AgentType, truncate(s.LastLine, 80))
	}
	return nil
}

func parseReadArgs(args []string) (clientName, sessionName string, lines int, err error) {
	lines = 120
	pos := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lines", "-n":
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("--lines requires a value")
			}
			lines, err = strconv.Atoi(args[i+1])
			if err != nil || lines <= 0 {
				return "", "", 0, fmt.Errorf("invalid --lines value")
			}
			i++
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 2 {
		return "", "", 0, fmt.Errorf("usage: ducklord read <client> <session> [--lines N]")
	}
	return pos[0], pos[1], lines, nil
}

type tuiState struct {
	cfg      *ducklord.Config
	runner   remoteRunner
	refresh  time.Duration
	sessions []ducklord.RemoteSession
	selected int
	hashes   map[string]string
}

func runTUI(cfg *ducklord.Config, runner remoteRunner, refresh time.Duration) error {
	oldState, err := makeRaw()
	if err != nil {
		return err
	}
	defer restore(oldState)
	fmt.Print("\033[?1049h\033[?25l\033[?1000h\033[?1006h")
	defer fmt.Print("\033[?1006l\033[?1000l\033[?25h\033[?1049l")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	state := &tuiState{cfg: cfg, runner: runner, refresh: refresh, hashes: map[string]string{}}
	state.refreshSessions(ctx)
	state.render()
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	input := make(chan []byte, 8)
	go readInput(ctx, input)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			state.refreshSessions(ctx)
			state.render()
		case b := <-input:
			action := state.handleInput(b)
			state.render()
			switch action {
			case "quit":
				return nil
			case "refresh":
				state.refreshSessions(ctx)
				state.render()
			case "attach":
				if len(state.sessions) == 0 {
					continue
				}
				restore(oldState)
				fmt.Print("\033[?1006l\033[?1000l\033[?25h\033[?1049l")
				s := state.sessions[state.selected]
				c, err := mustClient(cfg, s.Client)
				if err != nil {
					return err
				}
				return runner.Attach(c, s.Name)
			}
		}
	}
}

func (s *tuiState) refreshSessions(ctx context.Context) {
	var all []ducklord.RemoteSession
	for _, c := range s.cfg.Clients {
		sessions, err := s.runner.Sessions(ctx, c, 8)
		if err != nil {
			all = append(all, ducklord.RemoteSession{Client: c.Name, Group: c.Group, Name: "(offline)", Status: "error", Error: err.Error()})
			continue
		}
		all = append(all, sessions...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Group != all[j].Group {
			return all[i].Group < all[j].Group
		}
		if all[i].Client != all[j].Client {
			return all[i].Client < all[j].Client
		}
		return all[i].Name < all[j].Name
	})
	for i := range all {
		key := all[i].Client + "/" + all[i].Name
		if all[i].TailHash != "" && s.hashes[key] != "" && s.hashes[key] != all[i].TailHash {
			all[i].Updated = true
			fmt.Print("\a")
		}
		if all[i].TailHash != "" {
			s.hashes[key] = all[i].TailHash
		}
	}
	s.sessions = all
	if s.selected >= len(s.sessions) {
		s.selected = len(s.sessions) - 1
	}
	if s.selected < 0 {
		s.selected = 0
	}
}

func (s *tuiState) render() {
	fmt.Print("\033[H\033[2J")
	fmt.Println("ducklord remote agents")
	fmt.Println("j/k or arrows move  enter attach  r refresh  q quit")
	fmt.Println(strings.Repeat("-", 96))
	if len(s.sessions) == 0 {
		fmt.Println("No sessions.")
		return
	}
	currentGroup := "\000"
	row := 4
	for i, sess := range s.sessions {
		group := sess.Group
		if group == "" {
			group = "default"
		}
		if group != currentGroup {
			fmt.Printf("\033[%d;1H[%s]\n", row, group)
			row++
			currentGroup = group
		}
		prefix := " "
		if i == s.selected {
			prefix = ">"
		}
		mark := " "
		if sess.Updated {
			mark = "*"
		}
		line := fmt.Sprintf("%s%s %-12s %-18s %-9s %-10s %s", prefix, mark, sess.Client, sess.Name, sess.Status, sess.AgentType, sess.LastLine)
		if sess.Error != "" {
			line = fmt.Sprintf("%s! %-12s %-18s %-9s %s", prefix, sess.Client, sess.Name, sess.Status, sess.Error)
		}
		fmt.Printf("\033[%d;1H%s\033[K\n", row, truncate(line, 120))
		row++
	}
}

func (s *tuiState) handleInput(b []byte) string {
	text := string(b)
	switch {
	case text == "q" || text == "\x03":
		return "quit"
	case text == "r":
		return "refresh"
	case text == "\r" || text == "\n":
		return "attach"
	case text == "j" || text == "\x1b[B":
		if s.selected < len(s.sessions)-1 {
			s.selected++
		}
	case text == "k" || text == "\x1b[A":
		if s.selected > 0 {
			s.selected--
		}
	case strings.HasPrefix(text, "\x1b[<0;"):
		if idx, ok := s.sessionIndexForMouse(text); ok {
			s.selected = idx
		}
	}
	return ""
}

func (s *tuiState) sessionIndexForMouse(seq string) (int, bool) {
	seq = strings.TrimPrefix(seq, "\x1b[<0;")
	parts := strings.FieldsFunc(seq, func(r rune) bool { return r == ';' || r == 'M' || r == 'm' })
	if len(parts) < 2 {
		return 0, false
	}
	y, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	row := 4
	currentGroup := "\000"
	for i, sess := range s.sessions {
		group := sess.Group
		if group == "" {
			group = "default"
		}
		if group != currentGroup {
			row++
			currentGroup = group
		}
		if y == row {
			return i, true
		}
		row++
	}
	return 0, false
}

func makeRaw() (*unix.Termios, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	newState := *oldState
	newState.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG
	newState.Iflag &^= unix.ICRNL | unix.IXON
	newState.Cc[unix.VMIN] = 1
	newState.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &newState); err != nil {
		return nil, err
	}
	return oldState, nil
}

func restore(state *unix.Termios) {
	if state != nil {
		_ = unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TCSETS, state)
	}
}

func readInput(ctx context.Context, ch chan<- []byte) {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		select {
		case ch <- append([]byte(nil), buf[:n]...):
		case <-ctx.Done():
			return
		}
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, `ducklord - remote agent session TUI

Usage:
  ducklord clients [--config <path>]
  ducklord sessions <client> [--config <path>]
  ducklord tui [--config <path>] [--refresh 2s]
  ducklord attach <client> <session> [--config <path>]
  ducklord read <client> <session> [--lines N] [--config <path>]
  ducklord send <client> <session> <text> [--config <path>]
  ducklord start <client> --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklord stop <client> <session> [--config <path>]
  ducklord version`)
}
