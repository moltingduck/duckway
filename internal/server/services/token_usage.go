package services

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
)

// TokenUsage is the per-request token breakdown parsed from an LLM
// response. Zero value means "no usage found".
type TokenUsage struct {
	Provider            string // "anthropic" | "openai"
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	ReasoningTokens     int64
}

// Empty reports whether no token counts were captured.
func (u TokenUsage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheCreationTokens == 0 &&
		u.ReasoningTokens == 0
}

// CategoryRate is an explicit price in USD micros per million tokens. Priced
// distinguishes a configured zero price from a missing price.
type CategoryRate struct {
	USDMicrosPerMillion int64
	Priced              bool
}

// UsageRates contains prices for mutually exclusive token categories.
type UsageRates struct {
	Input         CategoryRate
	Output        CategoryRate
	CacheRead     CategoryRate
	CacheCreation CategoryRate
	Reasoning     CategoryRate
}

// UsageCost is an integer-micro-USD result. Priced is false when a required
// category has no rate, usage is invalid, or the result exceeds int64.
type UsageCost struct {
	USDMicros int64
	Priced    bool
}

// CalculateUsageCost prices provider-reported usage without charging nested
// categories twice. OpenAI cached tokens are included in InputTokens and
// reasoning tokens are included in OutputTokens; Anthropic cache tokens are
// reported separately from InputTokens.
func CalculateUsageCost(usage TokenUsage, rates UsageRates) UsageCost {
	regularInput := usage.InputTokens
	regularOutput := usage.OutputTokens
	if strings.EqualFold(usage.Provider, "openai") {
		regularInput -= usage.CacheReadTokens
		regularOutput -= usage.ReasoningTokens
	}
	categories := []struct {
		tokens int64
		rate   CategoryRate
	}{
		{regularInput, rates.Input},
		{regularOutput, rates.Output},
		{usage.CacheReadTokens, rates.CacheRead},
		{usage.CacheCreationTokens, rates.CacheCreation},
		{usage.ReasoningTokens, rates.Reasoning},
	}

	total := new(big.Int)
	for _, category := range categories {
		if category.tokens < 0 || category.rate.USDMicrosPerMillion < 0 {
			return UsageCost{}
		}
		if category.tokens == 0 {
			continue
		}
		if !category.rate.Priced {
			return UsageCost{}
		}
		product := new(big.Int).Mul(big.NewInt(category.tokens), big.NewInt(category.rate.USDMicrosPerMillion))
		total.Add(total, product)
	}

	// Round the aggregate price to the nearest micro-dollar, half up. Rounding
	// once after summing avoids losing fractions independently per category.
	total.Add(total, big.NewInt(500_000))
	total.Quo(total, big.NewInt(1_000_000))
	if !total.IsInt64() {
		return UsageCost{}
	}
	return UsageCost{USDMicros: total.Int64(), Priced: true}
}

// usageScannerMaxJSON caps how much of a non-streaming JSON body we
// buffer to find the trailing `usage` object. Anthropic puts usage at
// the end of the message; 4 MB comfortably covers even very long
// single-shot completions while bounding memory.
const usageScannerMaxJSON = 4 << 20

// UsageScanner is an io.Writer the proxy tees an LLM response through.
// It extracts token usage as the body streams by without buffering the
// whole thing for the common (streaming SSE) case:
//
//   - text/event-stream → parse each `data:` line as it arrives,
//     merging usage from Anthropic message_start / message_delta (and
//     OpenAI's final chunk when present). O(1) memory.
//   - anything else (application/json) → accumulate up to
//     usageScannerMaxJSON bytes and parse once at Result().
//
// Writes never error and always report the full length so the tee
// doesn't disturb the real response copy.
type UsageScanner struct {
	isSSE   bool
	lineBuf []byte       // partial SSE line carry-over
	jsonBuf bytes.Buffer // non-SSE accumulation (capped)
	jsonCap bool         // true once we stopped accumulating
	usage   TokenUsage
}

// NewUsageScanner returns a scanner specialized for the response's
// Content-Type.
func NewUsageScanner(contentType string) *UsageScanner {
	ct := strings.ToLower(contentType)
	return &UsageScanner{isSSE: strings.Contains(ct, "text/event-stream")}
}

func (s *UsageScanner) Write(p []byte) (int, error) {
	if s.isSSE {
		s.feedSSE(p)
	} else if !s.jsonCap {
		remaining := usageScannerMaxJSON - s.jsonBuf.Len()
		if remaining > 0 {
			if len(p) <= remaining {
				s.jsonBuf.Write(p)
			} else {
				s.jsonBuf.Write(p[:remaining])
				s.jsonCap = true
			}
		} else {
			s.jsonCap = true
		}
	}
	return len(p), nil
}

// Result returns the accumulated usage (parsing the JSON buffer for the
// non-streaming case). Returns nil when nothing usable was found.
func (s *UsageScanner) Result() *TokenUsage {
	if !s.isSSE {
		s.parseJSONBody(s.jsonBuf.Bytes())
	}
	if s.usage.Empty() && s.usage.Model == "" {
		return nil
	}
	return &s.usage
}

// feedSSE splits incoming bytes into lines and parses `data:` payloads.
func (s *UsageScanner) feedSSE(p []byte) {
	s.lineBuf = append(s.lineBuf, p...)
	for {
		i := bytes.IndexByte(s.lineBuf, '\n')
		if i < 0 {
			// Guard against an unbounded line (malformed stream).
			if len(s.lineBuf) > usageScannerMaxJSON {
				s.lineBuf = s.lineBuf[:0]
			}
			return
		}
		line := bytes.TrimRight(s.lineBuf[:i], "\r")
		s.lineBuf = s.lineBuf[i+1:]
		const prefix = "data:"
		if !bytes.HasPrefix(line, []byte(prefix)) {
			continue
		}
		payload := bytes.TrimSpace(line[len(prefix):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		s.parseSSEEvent(payload)
	}
}

// sseEvent is the union of Anthropic SSE event shapes we care about.
type sseEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *anthropicUsage `json:"usage"` // message_delta carries top-level usage
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	PromptDetails    struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	InputDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func openAIUsageDistinct(u *openAIUsage) bool {
	return u.PromptTokens != 0 || u.CompletionTokens != 0 ||
		u.PromptDetails.CachedTokens != 0 || u.InputDetails.CachedTokens != 0 ||
		u.CompletionDetails.ReasoningTokens != 0 || u.OutputDetails.ReasoningTokens != 0
}

func (s *UsageScanner) parseSSEEvent(payload []byte) {
	var openAI struct {
		Type     string       `json:"type"`
		Model    string       `json:"model"`
		Usage    *openAIUsage `json:"usage"`
		Response *struct {
			Model string       `json:"model"`
			Usage *openAIUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &openAI) == nil {
		usage := openAI.Usage
		model := openAI.Model
		if openAI.Response != nil {
			if openAI.Response.Usage != nil {
				usage = openAI.Response.Usage
			}
			if openAI.Response.Model != "" {
				model = openAI.Response.Model
			}
		}
		isResponseEvent := strings.HasPrefix(openAI.Type, "response.") || openAI.Response != nil
		if usage != nil && openAIUsagePresent(usage) && (isResponseEvent || openAIUsageDistinct(usage)) {
			s.applyOpenAIUsage(model, usage)
			return
		}
	}

	var ev sseEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			if ev.Message.Model != "" {
				s.usage.Model = ev.Message.Model
			}
			if ev.Message.Usage != nil {
				s.usage.Provider = "anthropic"
				// message_start carries the input + cache figures and an
				// initial output_tokens (~1). Take input/cache here.
				s.usage.InputTokens = ev.Message.Usage.InputTokens
				s.usage.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
				s.usage.CacheCreationTokens = ev.Message.Usage.CacheCreationInputTokens
				if ev.Message.Usage.OutputTokens > s.usage.OutputTokens {
					s.usage.OutputTokens = ev.Message.Usage.OutputTokens
				}
			}
		}
	case "message_delta":
		if ev.Usage != nil {
			s.usage.Provider = "anthropic"
			// Final output token count lands in the last message_delta.
			if ev.Usage.OutputTokens > 0 {
				s.usage.OutputTokens = ev.Usage.OutputTokens
			}
			// Some streams also restate input/cache here.
			if ev.Usage.InputTokens > 0 {
				s.usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				s.usage.CacheReadTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				s.usage.CacheCreationTokens = ev.Usage.CacheCreationInputTokens
			}
		}
	}
}

func openAIUsagePresent(u *openAIUsage) bool {
	return u.PromptTokens != 0 || u.CompletionTokens != 0 || u.InputTokens != 0 || u.OutputTokens != 0 ||
		u.PromptDetails.CachedTokens != 0 || u.InputDetails.CachedTokens != 0 ||
		u.CompletionDetails.ReasoningTokens != 0 || u.OutputDetails.ReasoningTokens != 0
}

func (s *UsageScanner) applyOpenAIUsage(model string, u *openAIUsage) {
	s.usage.Provider = "openai"
	if model != "" {
		s.usage.Model = model
	}
	s.usage.InputTokens = u.InputTokens
	if s.usage.InputTokens == 0 {
		s.usage.InputTokens = u.PromptTokens
	}
	s.usage.OutputTokens = u.OutputTokens
	if s.usage.OutputTokens == 0 {
		s.usage.OutputTokens = u.CompletionTokens
	}
	s.usage.CacheReadTokens = u.InputDetails.CachedTokens
	if s.usage.CacheReadTokens == 0 {
		s.usage.CacheReadTokens = u.PromptDetails.CachedTokens
	}
	s.usage.ReasoningTokens = u.OutputDetails.ReasoningTokens
	if s.usage.ReasoningTokens == 0 {
		s.usage.ReasoningTokens = u.CompletionDetails.ReasoningTokens
	}
}

// parseJSONBody handles non-streaming responses for both providers.
func (s *UsageScanner) parseJSONBody(body []byte) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return
	}
	var envelope struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Object string `json:"object"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return
	}

	// OpenAI chat completions and Responses use distinct envelope identifiers.
	var oai struct {
		Model string       `json:"model"`
		Usage *openAIUsage `json:"usage"`
	}
	isOpenAI := strings.HasPrefix(envelope.ID, "chatcmpl-") || strings.HasPrefix(envelope.ID, "resp_") ||
		strings.Contains(strings.ToLower(envelope.Object), "completion") || strings.EqualFold(envelope.Object, "response")
	if err := json.Unmarshal(body, &oai); err == nil && oai.Usage != nil && openAIUsagePresent(oai.Usage) &&
		(isOpenAI || openAIUsageDistinct(oai.Usage)) {
		s.applyOpenAIUsage(oai.Model, oai.Usage)
		return
	}

	// Anthropic shape: {"type":"message","usage":{"input_tokens":...}}
	var anth struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &anth); err == nil && anth.Usage != nil &&
		(anth.Usage.InputTokens > 0 || anth.Usage.OutputTokens > 0) {
		s.usage.Provider = "anthropic"
		s.usage.Model = anth.Model
		s.usage.InputTokens = anth.Usage.InputTokens
		s.usage.OutputTokens = anth.Usage.OutputTokens
		s.usage.CacheReadTokens = anth.Usage.CacheReadInputTokens
		s.usage.CacheCreationTokens = anth.Usage.CacheCreationInputTokens
		return
	}

}
