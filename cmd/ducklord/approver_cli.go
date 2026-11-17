package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

const approverUsage = `usage:
  ducklord approver login [--server <url>] [--insecure] [--config <path>]   (token from stdin or a hidden prompt)
  ducklord approver logout [--config <path>]`

// approverCredentialsFor returns the credentials file beside the given config
// file (or the default config). It is always a separate file from config.yaml.
func approverCredentialsFor(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return ducklord.ApproverCredentialsPath()
	}
	return filepath.Join(filepath.Dir(configPath), ducklord.ApproverCredentialsFile)
}

// runApproverCommand implements `ducklord approver login|logout`. stdin is read
// for the token when it is not a terminal; otherwise the token is prompted
// without echo.
func runApproverCommand(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(approverUsage)
	}
	sub, rest := args[0], args[1:]
	server, configPath, insecure := "", "", false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--server":
			if sub != "login" || i+1 >= len(rest) {
				return errors.New(approverUsage)
			}
			server = rest[i+1]
			i++
		case "--insecure":
			if sub != "login" {
				return errors.New(approverUsage)
			}
			insecure = true
		case "--config", "-c":
			if i+1 >= len(rest) {
				return fmt.Errorf("--config requires a value")
			}
			configPath = rest[i+1]
			i++
		default:
			return fmt.Errorf("unknown approver option %q\n%s", rest[i], approverUsage)
		}
	}
	path := approverCredentialsFor(configPath)
	switch sub {
	case "logout":
		if err := ducklord.DeleteApproverCredentials(path); err != nil {
			return err
		}
		fmt.Fprintln(out, "Approver login removed.")
		return nil
	case "login":
		return approverLogin(path, server, insecure, in, out)
	default:
		return fmt.Errorf("unknown approver command %q\n%s", sub, approverUsage)
	}
}

func approverLogin(path, server string, insecure bool, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	if server == "" {
		if existing, err := ducklord.LoadApproverCredentials(path); err == nil {
			server = existing.Server
		}
	}
	if server == "" {
		if f, ok := in.(*os.File); ok && f == os.Stdin {
			if stdinIsTerminal() { // a terminal: ask
				fmt.Fprint(out, "Duckway server URL: ")
				line, _ := reader.ReadString('\n')
				server = strings.TrimSpace(line)
			}
		}
	}
	if server == "" {
		return errors.New("--server <url> is required for the first login")
	}
	normalized, err := ducklord.NormalizeApproverServer(server)
	if err != nil {
		return err
	}
	if u, _ := url.Parse(normalized); u != nil && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		if !insecure {
			return fmt.Errorf("refusing plain http to %s: the approver token would cross the network unencrypted; use https or pass --insecure", normalized)
		}
		fmt.Fprintln(out, "warning: the server URL is plain http; the approver token will cross the network unencrypted.")
	}
	if w := localAgentWarning(); w != "" {
		fmt.Fprintln(out, w)
	}
	token, err := readApproverToken(in, reader, out)
	if err != nil {
		return err
	}
	client, err := ducklord.NewApproverClient(normalized, token, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pending, err := client.List(ctx, []string{"pending"}, 1000)
	if err != nil {
		return fmt.Errorf("could not validate the approver token against %s: %w", normalized, err)
	}
	if err := ducklord.SaveApproverCredentials(path, ducklord.ApproverCredentials{Server: normalized, Token: token}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in to %s as an approver (%d pending). Press the Approvals key in the TUI to review.\n", normalized, len(pending))
	return nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// readApproverToken reads the token from a non-terminal stdin, or prompts
// without echo when stdin is the terminal.
func readApproverToken(in io.Reader, reader *bufio.Reader, out io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok && f == os.Stdin {
		if old, err := makeRaw(); err == nil {
			defer restore(old)
			fmt.Fprint(out, "Approver token (input hidden): ")
			var token []byte
			for {
				b, err := reader.ReadByte()
				if err != nil {
					return "", err
				}
				switch {
				case b == '\r' || b == '\n':
					fmt.Fprint(out, "\r\n")
					return strings.TrimSpace(string(token)), nil
				case b == 3:
					fmt.Fprint(out, "\r\n")
					return "", errors.New("cancelled")
				case b == 127 || b == 8:
					if len(token) > 0 {
						token = token[:len(token)-1]
					}
				case b >= 32:
					token = append(token, b)
				}
			}
		}
	}
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no approver token on stdin")
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", errors.New("no approver token on stdin")
	}
	return token, nil
}

// stdinIsTerminal reports whether stdin is a terminal (raw mode can be set
// and is immediately undone).
func stdinIsTerminal() bool {
	old, err := makeRaw()
	if err != nil {
		return false
	}
	restore(old)
	return true
}
