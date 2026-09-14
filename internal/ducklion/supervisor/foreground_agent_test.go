package supervisor

import (
	"testing"
)

func TestParseForegroundProcStatHandlesParenthesesAndFields(t *testing.T) {
	data := []byte("123 (name ) with spaces) S 12 34 56 78 0 0 0 0 0 0 0 0 0 0 0 0 0 0 999 0\n")
	got, ok := parseForegroundProcStat(data)
	if !ok || got.parent != 12 || got.group != 34 || got.tty != 78 || got.start != 999 {
		t.Fatalf("valid stat was not parsed: %+v ok=%t", got, ok)
	}
	for _, value := range [][]byte{nil, []byte("123 (unterminated"), []byte("123 (x) S 1 2"),
		[]byte("123 (x) S 1 0 3 4 0 0 0 0 0 0 0 0 0 0 0 0 0 0 999"),
		[]byte("123 (x) S 1 2 3 4 0 0 0 0 0 0 0 0 0 0 0 0 0 0 invalid")} {
		if _, ok := parseForegroundProcStat(value); ok {
			t.Fatalf("malformed stat accepted: %q", value)
		}
	}
}

func TestClassifyForegroundAgentUsesOnlyExecutableAndArgvZero(t *testing.T) {
	for _, tc := range []struct {
		exe  string
		args []string
		want string
	}{
		{"/usr/bin/codex", []string{"/usr/bin/codex", "private prompt"}, "codex"},
		{"/usr/bin/node", []string{"/usr/local/bin/claude", "private prompt"}, "claude"},
		{"/usr/bin/node", []string{"/usr/local/bin/codex", "private prompt"}, "codex"},
		{"/home/user/.local/share/claude/versions/2.1.183", []string{"claude", "private prompt"}, "claude"},
		{"/tmp/claude/versions/not-a-version", []string{"claude"}, ""},
		{"/usr/bin/bash", []string{"bash", "-c", "codex"}, ""},
		{"/usr/bin/printf", []string{"codex", "private prompt"}, ""},
		{"/usr/bin/node", []string{"node", "/usr/local/bin/claude"}, ""},
		{"/usr/bin/other", []string{"claude"}, ""},
		{"/usr/bin/claude", nil, ""},
	} {
		args := make([][]byte, 0, len(tc.args))
		for _, arg := range tc.args {
			args = append(args, []byte(arg))
		}
		if got := classifyForegroundAgent(tc.exe, args); got != tc.want {
			t.Fatalf("classify %q / argv0 %q: got %q, want %q", tc.exe, firstArg(tc.args), got, tc.want)
		}
	}
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
