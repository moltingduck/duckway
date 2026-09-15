package supervisor

import (
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestPTYProbesConcurrentShutdown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func(*os.File) (int, error)
	}{
		{"foreground", foregroundProcessGroup},
		{"pending-output", pendingPTYBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for iteration := 0; iteration < 32; iteration++ {
				master, slave, err := pty.Open()
				if err != nil {
					t.Fatal(err)
				}
				// Mirror capture's pending Read: its deferred descriptor release
				// may perform the actual close concurrently with a probe.
				readDone := make(chan struct{})
				go func() {
					defer close(readDone)
					var buffer [1]byte
					_, _ = master.Read(buffer[:])
				}()
				started, probeDone := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(probeDone)
					_, _ = tc.probe(master)
					close(started)
					for attempt := 0; attempt < 256; attempt++ {
						_, _ = tc.probe(master)
					}
				}()
				<-started
				_ = master.Close()
				_ = slave.Close()
				for _, done := range []chan struct{}{readDone, probeDone} {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("PTY probe or capture did not finish after shutdown")
					}
				}
				if _, err := tc.probe(master); err == nil {
					t.Fatal("probe after shutdown unexpectedly succeeded")
				}
			}
		})
	}
}

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
		{"/home/user/.opencode/bin/opencode", []string{"opencode", "private prompt"}, "other_agent"},
		{"/usr/bin/opencode", []string{"/usr/bin/opencode"}, "other_agent"},
		{"/tmp/claude/versions/not-a-version", []string{"claude"}, ""},
		{"/usr/bin/node", []string{"opencode"}, ""},
		{"/usr/bin/printf", []string{"opencode"}, ""},
		{"/usr/bin/bash", []string{"bash", "-c", "opencode"}, ""},
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
