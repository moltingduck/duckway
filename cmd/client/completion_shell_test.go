package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionEndpointHelper(t *testing.T) {
	if os.Getenv("DUCKWAY_COMPLETION_TEST_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "__complete" {
			for _, item := range completionCandidates("", os.Args[i+1:]) {
				fmt.Println(item)
			}
			os.Exit(0)
		}
	}
	os.Exit(1)
}

// Exercise the generated adapters in real shells, using the test executable as
// an offline completion endpoint. Each shell owns only t.TempDir fixtures.
func TestCompletionRealShells(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			shellPath, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(shell + " is not installed")
			}
			dir := t.TempDir()
			shim := "#!/bin/sh\nexec " + shellQuote(binary) + " -test.run=^TestCompletionEndpointHelper$ -- \"$@\"\n"
			if err := os.WriteFile(filepath.Join(dir, "duckway"), []byte(shim), 0700); err != nil {
				t.Fatal(err)
			}
			var script strings.Builder
			if err := writeCompletion(&script, shell); err != nil {
				t.Fatal(err)
			}
			switch shell {
			case "bash":
				script.WriteString("\nCOMP_WORDS=(duckway git cl); COMP_CWORD=2; _duckway_complete; printf '%s\\n' \"${COMPREPLY[@]}\"\n")
			case "zsh":
				script.Reset()
				script.WriteString("autoload -Uz compinit; compinit -D\n")
				if err := writeCompletion(&script, shell); err != nil {
					t.Fatal(err)
				}
				script.WriteString("\ncompadd() { shift; printf '%s\\n' \"$@\"; }; words=(duckway git cl); CURRENT=3; _duckway_complete\n")
			case "fish":
				script.WriteString("\ncomplete -C 'duckway git cl'\n")
			}
			cmd := exec.Command(shellPath, "-c", script.String())
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DUCKWAY_COMPLETION_TEST_HELPER=1", "HOME="+dir, "ZDOTDIR="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if strings.TrimSpace(string(out)) != "clone" {
				t.Fatalf("unexpected completions %q", out)
			}
		})
	}
}
