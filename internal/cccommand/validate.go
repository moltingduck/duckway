package cccommand

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnknownCommand lets dispatchers preserve their own user-facing unknown
// command response while keeping validation fail-closed for new handlers.
var ErrUnknownCommand = errors.New("unknown CC command")

var usages = map[string]string{
	"!help":            "!help",
	"!new":             "!new <slug> [--cwd <path>|--project <name|number>] [--topic <text>]",
	"!new-confirm":     "!new-confirm <token>",
	"!end":             "!end",
	"!destroy":         "!destroy",
	"!yield":           "!yield [-w|--wait]",
	"!list":            "!list",
	"!status":          "!status",
	"!sessions":        "!sessions [<cwd-filter>]",
	"!bind":            "!bind <session_id> [<session_id> ...]",
	"!projects":        "!projects [<filter>]",
	"!duckway-version": "!duckway-version",
	"!duckway-doctor":  "!duckway-doctor",
	"!duckway-restart": "!duckway-restart",
	"!duckway-update":  "!duckway-update [--restart]",
	"!log":             "!log [N]",
}

// Usage returns the canonical command syntax displayed after validation errors.
func Usage(command string) string {
	return usages[command]
}

// Validate rejects unsupported options and malformed arguments before a
// command handler can read local state or perform any mutation. Unknown
// commands are reported with ErrUnknownCommand so dispatchers can use their
// own suggestion/error response.
func Validate(command string, args []string) error {
	switch command {
	case "!help", "!end", "!destroy", "!list", "!status", "!duckway-version", "!duckway-doctor", "!duckway-restart":
		return validateNoArgs(args)
	case "!yield":
		if len(args) == 0 || len(args) == 1 && (args[0] == "-w" || args[0] == "--wait") {
			return nil
		}
		return unsupportedArgs(args)
	case "!new":
		_, err := ParseNewArgs(args)
		return err
	case "!new-confirm":
		return validatePositionals(args, 1, 1)
	case "!sessions", "!projects":
		return validatePositionals(args, 0, 1)
	case "!bind":
		return validatePositionals(args, 1, -1)
	case "!duckway-update":
		if len(args) == 0 || (len(args) == 1 && args[0] == "--restart") {
			return nil
		}
		return unsupportedArgs(args)
	case "!log":
		return validateLogArgs(args)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownCommand, command)
	}
}

func validateNoArgs(args []string) error {
	if len(args) == 0 {
		return nil
	}
	return unsupportedArgs(args)
}

func validatePositionals(args []string, minArgs, maxArgs int) error {
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("argument must not be empty")
		}
		if looksLikeOption(arg) {
			return fmt.Errorf("unsupported option %q", arg)
		}
	}
	if len(args) < minArgs {
		return fmt.Errorf("missing required argument")
	}
	if maxArgs >= 0 && len(args) > maxArgs {
		return fmt.Errorf("too many arguments")
	}
	return nil
}

func validateLogArgs(args []string) error {
	if len(args) == 0 {
		return nil
	}
	for _, arg := range args {
		if looksLikeOption(arg) {
			return fmt.Errorf("unsupported option %q", arg)
		}
	}
	parts := args
	if len(parts) == 2 && strings.EqualFold(parts[0], "last") {
		parts = parts[1:]
	}
	if len(parts) != 1 {
		return fmt.Errorf("expected one log count")
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 1 || n > 20 {
		return fmt.Errorf("log count must be between 1 and 20")
	}
	return nil
}

func unsupportedArgs(args []string) error {
	if len(args) == 1 && looksLikeOption(args[0]) {
		return fmt.Errorf("unsupported option %q", args[0])
	}
	return fmt.Errorf("unsupported arguments: %q", strings.Join(args, " "))
}

func looksLikeOption(arg string) bool {
	return strings.HasPrefix(arg, "-")
}

// NewArgs is the parsed, validated argument set for !new.
type NewArgs struct {
	Slug    string
	Cwd     string
	Project string
	Topic   string
}

// ParseNewArgs accepts one slug and the documented value flags only.
func ParseNewArgs(args []string) (NewArgs, error) {
	var parsed NewArgs
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--") {
			key := strings.TrimPrefix(arg, "--")
			if key != "cwd" && key != "project" && key != "topic" {
				return NewArgs{}, fmt.Errorf("unsupported option %q", arg)
			}
			if seen[key] {
				return NewArgs{}, fmt.Errorf("option %q may only be specified once", arg)
			}
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "--") {
				return NewArgs{}, fmt.Errorf("option %q requires a value", arg)
			}
			seen[key] = true
			i++
			switch key {
			case "cwd":
				parsed.Cwd = args[i]
			case "project":
				parsed.Project = args[i]
			case "topic":
				parsed.Topic = args[i]
			}
			continue
		}
		if looksLikeOption(arg) {
			return NewArgs{}, fmt.Errorf("unsupported option %q", arg)
		}
		if parsed.Slug != "" {
			return NewArgs{}, fmt.Errorf("unexpected positional argument %q", arg)
		}
		parsed.Slug = arg
	}
	if parsed.Slug == "" {
		return NewArgs{}, fmt.Errorf("missing <slug>")
	}
	if parsed.Cwd != "" && parsed.Project != "" {
		return NewArgs{}, fmt.Errorf("choose either --cwd or --project, not both")
	}
	return parsed, nil
}
