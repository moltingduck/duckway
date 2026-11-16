package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"syscall"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func main() {
	runner := ducklord.NewRunner()
	err := run(os.Args[1:], os.Stdout, runner)
	runner.Close()
	if errors.Is(err, errRestartTUI) {
		executable, execErr := os.Executable()
		if execErr == nil {
			execErr = syscall.Exec(executable, os.Args, os.Environ())
		}
		log.Fatal("restart ducklord TUI: ", execErr)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, `ducklord - remote agent session TUI

Usage:
  ducklord [--name <owner>] tui [--config <path>] [--refresh 2s]
  ducklord approver login [--server <url>] | logout   (approvals panel in the TUI)
  ducklord clients [--config <path>]
  ducklord ssh-hosts [--config-file <ssh_config>]
  ducklord import-ssh-hosts [--ssh-config <ssh_config>] [--config <path>]
  ducklord sessions <client> [--config <path>]
  ducklord bookmarks <client> [--config <path>] (alias: projects)
  ducklord hook-status <client> <codex|claude> [--config <path>]
  ducklord agents <client> <directory-path> [--config <path>]
  ducklord probe <client> [--config <path>]
  ducklord install-ducklion <client> [--source <local-path>] [--dest <remote-path>] [--config <path>]
  ducklord tui [--config <path>] [--name <owner>] [--refresh 2s]
  ducklord attach-host <client> [--config <path>]
  ducklord attach <client> <session> [--config <path>]
  ducklord read <client> <session> [--lines N] [--config <path>]
  ducklord retained <client> [--config <path>]
  ducklord read-retained <client> <session-id> <generation> [--lines N] [--config <path>]
  ducklord send <client> <session> <text> [--config <path>]
  ducklord start <client> --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklord stop <client> <session> [--config <path>]
  ducklord end <client> <session> [-w|--wait|-f|--force] [--config <path>]
  ducklord restart <client> <session> [-f|--force] [--config <path>]
  ducklord destroy <client> <session> [-w|--wait|-f|--force] [--config <path>]
  ducklord yield <client> <session> [-w|--wait] [--config <path>]
  ducklord update [--check] [--server <https://duckway-server>]
  ducklord version`)
}
