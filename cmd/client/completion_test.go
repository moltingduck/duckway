package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestCompletionContext(t *testing.T) {
	for _, tt := range []struct {
		words []string
		want  []string
	}{
		{[]string{"git", "cl"}, []string{"clone"}},
		{[]string{"completion", ""}, []string{"bash", "fish", "zsh"}},
		{[]string{"session", "start", "--agent", "c"}, []string{"claude", "codex"}},
		{[]string{"session", "start", "--name", ""}, nil},
		{[]string{"session", "start", "--", ""}, nil},
		{[]string{"config", "get", "pty_log_retention_days", ""}, nil},
		{[]string{"update", "--server", ""}, nil},
		{[]string{"git", "list", ""}, nil},
		{[]string{"unknown", ""}, nil},
	} {
		if got := completionCandidates(t.TempDir(), tt.words); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %q want %q", tt.words, got, tt.want)
		}
	}
	for _, candidate := range completionCandidates(t.TempDir(), []string{""}) {
		if strings.HasPrefix(candidate, "__") {
			t.Errorf("private candidate %q", candidate)
		}
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var out bytes.Buffer
		if err := writeCompletion(&out, shell); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "duckway __complete") {
			t.Fatal(shell)
		}
	}
	var out bytes.Buffer
	if writeCompletion(&out, "$(touch /should-not-run)") == nil || out.Len() != 0 {
		t.Fatal("unsupported shell accepted")
	}
}
