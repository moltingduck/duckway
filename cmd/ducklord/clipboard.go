package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const clipboardCommandTimeout = 750 * time.Millisecond

// clipboardGOOS and clipboardRun are variables so command selection and
// failure handling can be tested without depending on the host desktop.
var clipboardGOOS = runtime.GOOS

var clipboardRun = runClipboardCommand

type clipboardCommand struct {
	name string
	args []string
}

func clipboardCommands(goos string) []clipboardCommand {
	switch goos {
	case "darwin":
		return []clipboardCommand{{name: "pbcopy"}}
	case "linux":
		return []clipboardCommand{
			{name: "wl-copy"},
			{name: "xclip", args: []string{"-selection", "clipboard"}},
			{name: "xsel", args: []string{"--clipboard", "--input"}},
		}
	case "windows":
		return []clipboardCommand{
			{name: "clip.exe"},
			{name: "powershell.exe", args: []string{"-NoProfile", "-NonInteractive", "-Command", "$input = [Console]::In.ReadToEnd(); Set-Clipboard -Value $input"}},
		}
	}
	return nil
}

func runClipboardCommand(ctx context.Context, name string, args []string, body string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = strings.NewReader(body)
	return command.Run()
}

// writeNativeClipboard tries the native clipboard utility for the host OS.
// Each candidate is bounded so a missing or unhealthy desktop helper cannot
// stall the TUI. The caller may still provide a terminal escape fallback.
func writeNativeClipboard(body string) (string, error) {
	commands := clipboardCommands(clipboardGOOS)
	if len(commands) == 0 {
		return "", fmt.Errorf("no native clipboard command for %s", clipboardGOOS)
	}
	var failures []string
	ctx, cancel := context.WithTimeout(context.Background(), clipboardCommandTimeout)
	defer cancel()
	for _, candidate := range commands {
		err := clipboardRun(ctx, candidate.name, candidate.args, body)
		if err == nil {
			return candidate.name, nil
		}
		failures = append(failures, candidate.name+": "+err.Error())
	}
	return "", errors.New(strings.Join(failures, "; "))
}

// emitNotesClipboard writes Notes content to the host clipboard and also emits
// OSC 52 so a user viewing a remote Ducklord gets terminal copy. The native
// command is still required on macOS, where Terminal.app may not support OSC52.
func emitNotesClipboard(out io.Writer, body string) string {
	native, nativeErr := writeNativeClipboard(body)
	_, _ = fmt.Fprint(out, "\x1b]52;c;", encodeClipboardBody(body), "\x07")
	if nativeErr == nil {
		return fmt.Sprintf("Notes copied to system clipboard (%s + OSC 52 terminal copy)", native)
	}
	return "Notes copied via terminal clipboard (OSC 52; native clipboard unavailable: " + nativeErr.Error() + ")"
}

func encodeClipboardBody(body string) string {
	return base64.StdEncoding.EncodeToString([]byte(body))
}
