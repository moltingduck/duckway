package services

import (
	"math"
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
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":42,"output_tokens":7,"cache_read_input_tokens":12,"cache_creation_input_tokens":4}}`
	s := NewUsageScanner("application/json")
	s.Write([]byte(body))
	u := s.Result()
	if u == nil {
		t.Fatal("no usage")
		return
	}
	if u.Provider != "anthropic" || u.InputTokens != 42 || u.OutputTokens != 7 || u.CacheReadTokens != 12 || u.CacheCreationTokens != 4 {
		t.Errorf("got %+v", u)
	}
	if u.Model != "claude-opus-4-7" {
		t.Errorf("model = %q", u.Model)
	}
}

func TestUsageScanner_OpenAIJSON(t *testing.T) {
	body := `{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":15,"completion_tokens":8,"total_tokens":23,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":3}}}`
	s := NewUsageScanner("application/json")
	s.Write([]byte(body))
	u := s.Result()
	if u == nil {
		t.Fatal("no usage")
		return
	}
	if u.Provider != "openai" || u.InputTokens != 15 || u.OutputTokens != 8 || u.CacheReadTokens != 5 || u.ReasoningTokens != 3 {
		t.Errorf("got %+v", u)
	}
	if u.Model != "gpt-4o" {
		t.Errorf("model = %q", u.Model)
	}
}

func TestUsageScanner_OpenAIResponsesJSON(t *testing.T) {
	body := `{"id":"resp_1","model":"gpt-5","usage":{"input_tokens":100,"output_tokens":40,"input_tokens_details":{"cached_tokens":60},"output_tokens_details":{"reasoning_tokens":25}}}`
	s := NewUsageScanner("application/json")
	_, _ = s.Write([]byte(body))
	u := s.Result()
	if u == nil || u.Provider != "openai" || u.Model != "gpt-5" ||
		u.InputTokens != 100 || u.OutputTokens != 40 || u.CacheReadTokens != 60 || u.ReasoningTokens != 25 {
		t.Fatalf("got %+v", u)
	}
}

func TestUsageScanner_OpenAISSEFinalChunk(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl_1\",\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":12},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	s := NewUsageScanner("text/event-stream")
	for i := 0; i < len(stream); i += 3 {
		end := i + 3
		if end > len(stream) {
			end = len(stream)
		}
		_, _ = s.Write([]byte(stream[i:end]))
	}
	u := s.Result()
	if u == nil || u.InputTokens != 20 || u.OutputTokens != 7 || u.CacheReadTokens != 12 || u.ReasoningTokens != 2 {
		t.Fatalf("got %+v", u)
	}
}

func TestUsageScanner_OpenAIResponsesSSE(t *testing.T) {
	stream := `data: {"type":"response.completed","response":{"model":"gpt-5","usage":{"input_tokens":80,"output_tokens":30,"input_tokens_details":{"cached_tokens":50},"output_tokens_details":{"reasoning_tokens":20}}}}` + "\n\n"
	s := NewUsageScanner("text/event-stream")
	_, _ = s.Write([]byte(stream))
	u := s.Result()
	if u == nil || u.Model != "gpt-5" || u.InputTokens != 80 || u.OutputTokens != 30 ||
		u.CacheReadTokens != 50 || u.ReasoningTokens != 20 {
		t.Fatalf("got %+v", u)
	}
}

func TestUsageScanner_OpenAISSEUsesLatestCumulativeUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"model":"gpt-5","usage":{"input_tokens":10,"output_tokens":2}}`,
		`data: {"model":"gpt-5","usage":{"input_tokens":10,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4}}}`,
		"",
	}, "\n")
	s := NewUsageScanner("text/event-stream")
	_, _ = s.Write([]byte(stream))
	u := s.Result()
	if u == nil || u.InputTokens != 10 || u.OutputTokens != 9 || u.ReasoningTokens != 4 {
		t.Fatalf("cumulative usage was double-counted or lost: %+v", u)
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
	if (TokenUsage{ReasoningTokens: 5}).Empty() {
		t.Error("non-zero reasoning should not be Empty")
	}
}

func pricedRate(micros int64) CategoryRate {
	return CategoryRate{USDMicrosPerMillion: micros, Priced: true}
}

func TestCalculateUsageCostOpenAIDoesNotDoubleCountNestedCategories(t *testing.T) {
	usage := TokenUsage{
		Provider: "openai", InputTokens: 1_000_000, OutputTokens: 500_000,
		CacheReadTokens: 250_000, ReasoningTokens: 100_000,
	}
	rates := UsageRates{
		Input: pricedRate(2_000_000), Output: pricedRate(8_000_000),
		CacheRead: pricedRate(500_000), Reasoning: pricedRate(10_000_000),
	}
	// (750k * $2) + (400k * $8) + (250k * $0.5) + (100k * $10) = $5.825.
	got := CalculateUsageCost(usage, rates)
	if !got.Priced || got.USDMicros != 5_825_000 {
		t.Fatalf("got %+v", got)
	}
}

func TestCalculateUsageCostAnthropicCacheIsSeparateFromInput(t *testing.T) {
	usage := TokenUsage{Provider: "anthropic", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40, CacheCreationTokens: 10}
	rates := UsageRates{
		Input: pricedRate(1_000_000), Output: pricedRate(2_000_000),
		CacheRead: pricedRate(250_000), CacheCreation: pricedRate(1_250_000),
	}
	got := CalculateUsageCost(usage, rates)
	// 100 + 40 + 10 + 12.5 micro-USD rounds once to 163.
	if !got.Priced || got.USDMicros != 163 {
		t.Fatalf("got %+v", got)
	}
}

func TestCalculateUsageCostMissingAndZeroRates(t *testing.T) {
	if got := CalculateUsageCost(TokenUsage{Provider: "openai", InputTokens: 1}, UsageRates{}); got.Priced {
		t.Fatalf("missing input price should be unpriced: %+v", got)
	}
	got := CalculateUsageCost(TokenUsage{Provider: "openai", InputTokens: 1}, UsageRates{Input: pricedRate(0)})
	if !got.Priced || got.USDMicros != 0 {
		t.Fatalf("explicit free category should be priced at zero: %+v", got)
	}
	if got := CalculateUsageCost(TokenUsage{}, UsageRates{}); !got.Priced || got.USDMicros != 0 {
		t.Fatalf("empty usage should be priced without requiring rates: %+v", got)
	}
}

func TestCalculateUsageCostRejectsInvalidAndOverflow(t *testing.T) {
	cases := []struct {
		name  string
		usage TokenUsage
		rates UsageRates
	}{
		{"negative tokens", TokenUsage{InputTokens: -1}, UsageRates{Input: pricedRate(1)}},
		{"nested cache exceeds input", TokenUsage{Provider: "openai", InputTokens: 1, CacheReadTokens: 2}, UsageRates{CacheRead: pricedRate(1)}},
		{"negative rate", TokenUsage{InputTokens: 1}, UsageRates{Input: pricedRate(-1)}},
		{"overflow", TokenUsage{InputTokens: math.MaxInt64}, UsageRates{Input: pricedRate(math.MaxInt64)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CalculateUsageCost(tc.usage, tc.rates); got.Priced {
				t.Fatalf("invalid cost should be unpriced: %+v", got)
			}
		})
	}
}

func TestCalculateUsageCostRoundsAggregateOnce(t *testing.T) {
	usage := TokenUsage{Provider: "anthropic", InputTokens: 1, OutputTokens: 1}
	rates := UsageRates{Input: pricedRate(250_000), Output: pricedRate(250_000)}
	got := CalculateUsageCost(usage, rates)
	if !got.Priced || got.USDMicros != 1 {
		t.Fatalf("two quarter-micro categories should round together: %+v", got)
	}
}
