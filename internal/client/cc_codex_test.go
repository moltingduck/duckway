package client

import "testing"

func TestParseCodexJSONL(t *testing.T) {
	out := []byte(`Reading additional input from stdin...
{"type":"thread.started","thread_id":"019f02a8-0abe-71c1-bbf6-54b1c4a41dc7"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"final answer"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}
`)
	sid, result, isError := parseCodexJSONL(out, "")
	if sid != "019f02a8-0abe-71c1-bbf6-54b1c4a41dc7" {
		t.Fatalf("sid = %q", sid)
	}
	if result != "final answer" {
		t.Fatalf("result = %q", result)
	}
	if isError {
		t.Fatal("isError = true")
	}
}

func TestParseCodexJSONL_KeepsFallbackSessionID(t *testing.T) {
	out := []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`)
	sid, result, _ := parseCodexJSONL(out, "existing-thread")
	if sid != "existing-thread" {
		t.Fatalf("sid = %q", sid)
	}
	if result != "ok" {
		t.Fatalf("result = %q", result)
	}
}
