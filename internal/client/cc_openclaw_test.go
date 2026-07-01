package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunViaOpenClawUsesMessageFileAndSessionKey(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "openclaw")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
printf '%s\n' "$@" > args.txt
prompt_file=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--message-file" ]; then
    prompt_file="$2"
    break
  fi
  shift
done
cat "$prompt_file" > prompt.txt
printf '{"result":"openclaw ok"}\n'
`), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUCKWAY_CC_OPENCLAW_AGENT", "ops")
	sid, result, isErr, err := runViaOpenClaw(context.Background(), stub, dir, "hello openclaw", "", []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=dwch_oc",
	})
	if err != nil {
		t.Fatalf("runViaOpenClaw: %v", err)
	}
	if sid != "duckway:dwch_oc" {
		t.Fatalf("session id = %q", sid)
	}
	if result != "openclaw ok" || isErr {
		t.Fatalf("result=%q isErr=%v", result, isErr)
	}
	argsRaw, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	args := string(argsRaw)
	for _, want := range []string{"agent", "--agent\nops", "--session-key\nduckway:dwch_oc", "--message-file", "--json"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}
	promptRaw, _ := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if string(promptRaw) != "hello openclaw" {
		t.Fatalf("prompt = %q", promptRaw)
	}
}

func TestRunViaOpenClawExitErrorIncludesOutput(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "openclaw")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
printf '{"error":"bad claws","status":"failed"}\n'
exit 2
`), 0700); err != nil {
		t.Fatal(err)
	}
	sid, result, isErr, err := runViaOpenClaw(context.Background(), stub, dir, "hello", "duckway:existing", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if sid != "duckway:existing" {
		t.Fatalf("session id = %q", sid)
	}
	if result != "bad claws" || !isErr {
		t.Fatalf("result=%q isErr=%v", result, isErr)
	}
}
