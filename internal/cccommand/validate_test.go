package cccommand

import (
	"errors"
	"testing"
)

func TestValidateCommandArguments(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		wantErr bool
	}{
		{name: "help", command: "!help"},
		{name: "help extra", command: "!help", args: []string{"--verbose"}, wantErr: true},
		{name: "new documented flags", command: "!new", args: []string{"task", "--cwd", "/tmp/task", "--topic", "work"}},
		{name: "new unknown flag", command: "!new", args: []string{"task", "--bogus", "value"}, wantErr: true},
		{name: "new short flag", command: "!new", args: []string{"task", "-x"}, wantErr: true},
		{name: "new missing flag value", command: "!new", args: []string{"task", "--cwd", "--topic", "work"}, wantErr: true},
		{name: "new empty flag value", command: "!new", args: []string{"task", "--topic", ""}, wantErr: true},
		{name: "new duplicate flag", command: "!new", args: []string{"task", "--topic", "one", "--topic", "two"}, wantErr: true},
		{name: "new conflicting roots", command: "!new", args: []string{"task", "--cwd", "/tmp", "--project", "duckway"}, wantErr: true},
		{name: "confirm", command: "!new-confirm", args: []string{"abcd1234"}},
		{name: "confirm flag", command: "!new-confirm", args: []string{"--force"}, wantErr: true},
		{name: "sessions filter", command: "!sessions", args: []string{"duckway"}},
		{name: "sessions unknown flag", command: "!sessions", args: []string{"--all"}, wantErr: true},
		{name: "sessions too many filters", command: "!sessions", args: []string{"one", "two"}, wantErr: true},
		{name: "sessions empty filter", command: "!sessions", args: []string{""}, wantErr: true},
		{name: "bind ids", command: "!bind", args: []string{"one", "two"}},
		{name: "bind missing", command: "!bind", wantErr: true},
		{name: "bind flag", command: "!bind", args: []string{"--all"}, wantErr: true},
		{name: "projects filter", command: "!projects", args: []string{"duckway"}},
		{name: "projects flag", command: "!projects", args: []string{"--all"}, wantErr: true},
		{name: "update", command: "!duckway-update"},
		{name: "update restart", command: "!duckway-update", args: []string{"--restart"}},
		{name: "update unknown flag", command: "!duckway-update", args: []string{"--force"}, wantErr: true},
		{name: "log count", command: "!log", args: []string{"20"}},
		{name: "log legacy last", command: "!log", args: []string{"last", "3"}},
		{name: "log flag", command: "!log", args: []string{"--last", "3"}, wantErr: true},
		{name: "log too large", command: "!log", args: []string{"21"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.command, tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%q, %v) error = %v, wantErr %v", tt.command, tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestUsageCoversKnownCommands(t *testing.T) {
	for command := range usages {
		if Usage(command) == "" {
			t.Fatalf("missing usage for %s", command)
		}
	}
}

func TestValidateRejectsUnknownCommand(t *testing.T) {
	if err := Validate("!future-command", nil); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("error = %v, want ErrUnknownCommand", err)
	}
}
