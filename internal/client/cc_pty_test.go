package client

import (
	"testing"
)

func TestRespondToDecQueries(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  []byte
	}{
		// DA1 — device attributes primary
		{"DA1 bare", []byte("\x1b[c"), []byte("\x1b[?6c")},
		{"DA1 explicit 0", []byte("\x1b[0c"), []byte("\x1b[?6c")},

		// DA2 — device attributes secondary
		{"DA2 bare", []byte("\x1b[>c"), []byte("\x1b[>0;0;0c")},
		{"DA2 explicit 0", []byte("\x1b[>0c"), []byte("\x1b[>0;0;0c")},

		// DSR — device status report
		{"DSR ready", []byte("\x1b[5n"), []byte("\x1b[0n")},

		// CPR — cursor position request
		{"CPR", []byte("\x1b[6n"), []byte("\x1b[1;1R")},

		// Window size query
		{"window size", []byte("\x1b[18t"), []byte("\x1b[8;40;120t")},

		// XTVERSION
		{"XTVERSION", []byte("\x1b[>q"), []byte("\x1bP>|duckway 0\x1b\\")},

		// Multiple queries in one buffer
		{
			"DA1 then DSR",
			[]byte("\x1b[c\x1b[5n"),
			[]byte("\x1b[?6c\x1b[0n"),
		},
		{
			"DA2 then XTVERSION",
			[]byte("\x1b[>c\x1b[>q"),
			[]byte("\x1b[>0;0;0c\x1bP>|duckway 0\x1b\\"),
		},

		// Unrecognised sequences produce no output
		{"unrecognised CSI", []byte("\x1b[42m"), nil},
		{"no ESC", []byte("hello"), nil},
		{"ESC without bracket", []byte("\x1b="), nil},

		// Queries embedded in surrounding output (Ink mixes TUI content with queries)
		{
			"noise then DA1 then noise",
			[]byte("some output\x1b[cmore output"),
			[]byte("\x1b[?6c"),
		},

		// Truncated sequence at end of buffer — must not panic, no response
		{"truncated ESC", []byte("\x1b"), nil},
		{"truncated CSI", []byte("\x1b["), nil},
		{"truncated params", []byte("\x1b[5"), nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := respondToDecQueries(tc.input)
			if string(got) != string(tc.want) {
				t.Errorf("respondToDecQueries(%q)\n  got  %q\n  want %q", tc.input, got, tc.want)
			}
		})
	}
}
