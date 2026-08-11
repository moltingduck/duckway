package services

import (
	"strings"
	"testing"
)

func TestUsageScanner_AnthropicSSE(t *testing.T) {
	// Realistic Anthropic streaming sequence: message_start carries
	// input + cache + initial output(1); message_delta carries the
	// final output_tokens.
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1200,"cache_read_input_tokens":800,"cache_creation_input_tokens":50,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":345}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	s := NewUsageScanner("text/event-stream; charset=utf-8")
	// Feed in two arbitrary chunks to exercise line carry-over.
	mid := len(stream) / 2
	s.Write([]byte(stream[:mid]))
	s.Write([]byte(stream[mid:]))

	u := s.Result()
	if u == nil {
		t.Fatal("no usage parsed")
		return
	}
	if u.Provider != "anthropic" {
		t.Errorf("provider = %q", u.Provider)
	}
	if u.Model != "claude-opus-4-7" {
		t.Errorf("model = %q", u.Model)
	}
	if u.InputTokens != 1200 {
		t.Errorf("input = %d, want 1200", u.InputTokens)
	}
	if u.OutputTokens != 345 {
		t.Errorf("output = %d, want 345 (from message_delta)", u.OutputTokens)
	}
	if u.CacheReadTokens != 800 {
		t.Errorf("cache_read = %d, want 800", u.CacheReadTokens)
	}
	if u.CacheCreationTokens != 50 {
		t.Errorf("cache_creation = %d, want 50", u.CacheCreationTokens)
	}
}

func TestUsageScanner_AnthropicSSE_ByteByByte(t *testing.T) {
	// Pathological chunking: one byte per Write. Line reassembly must
	// still work.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":20}}` + "\n"
	s := NewUsageScanner("text/event-stream")
	for i := 0; i < len(stream); i++ {
		s.Write([]byte{stream[i]})
	}
	u := s.Result()
	if u == nil || u.InputTokens != 10 || u.OutputTokens != 20 {
		t.Fatalf("got %+v", u)
	}
}

func TestUsageScanner_AnthropicJSON(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":42,"output_tokens":7,"cache_read_input_tokens":12}}`
	s := NewUsageScanner("application/json")
	s.Write([]byte(body))
	u := s.Result()
	if u == nil {
		t.Fatal("no usage")
		return
	}
	if u.Provider != "anthropic" || u.InputTokens != 42 || u.OutputTokens != 7 || u.CacheReadTokens != 12 {
		t.Errorf("got %+v", u)
	}
	if u.Model != "claude-opus-4-7" {
		t.Errorf("model = %q", u.Model)
	}
}

func TestUsageScanner_OpenAIJSON(t *testing.T) {
	body := `{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":15,"completion_tokens":3,"total_tokens":18}}`
	s := NewUsageScanner("application/json")
	s.Write([]byte(body))
	u := s.Result()
	if u == nil {
		t.Fatal("no usage")
		return
	}
	if u.Provider != "openai" || u.InputTokens != 15 || u.OutputTokens != 3 {
		t.Errorf("got %+v", u)
	}
	if u.Model != "gpt-4o" {
		t.Errorf("model = %q", u.Model)
	}
}

func TestUsageScanner_NoUsage(t *testing.T) {
	// A non-LLM JSON response (e.g. GitHub) must yield nil.
	s := NewUsageScanner("application/json")
	s.Write([]byte(`{"login":"octocat","id":1,"public_repos":8}`))
	if u := s.Result(); u != nil {
		t.Errorf("expected nil for non-LLM body, got %+v", u)
	}
}

func TestUsageScanner_EmptyAndGarbage(t *testing.T) {
	for _, body := range []string{"", "   ", "not json", "{"} {
		s := NewUsageScanner("application/json")
		s.Write([]byte(body))
		if u := s.Result(); u != nil {
			t.Errorf("body %q: expected nil, got %+v", body, u)
		}
	}
}

func TestTokenUsageEmpty(t *testing.T) {
	if !(TokenUsage{}).Empty() {
		t.Error("zero value should be Empty")
	}
	if (TokenUsage{InputTokens: 1}).Empty() {
		t.Error("non-zero input should not be Empty")
	}
	if (TokenUsage{CacheReadTokens: 5}).Empty() {
		t.Error("non-zero cache_read should not be Empty")
	}
}
