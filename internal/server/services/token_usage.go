package services

import (
	"bytes"
	"encoding/json"
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
}

// Empty reports whether no token counts were captured.
func (u TokenUsage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheCreationTokens == 0
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
	lineBuf []byte        // partial SSE line carry-over
	jsonBuf bytes.Buffer  // non-SSE accumulation (capped)
	jsonCap bool          // true once we stopped accumulating
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
		Model string         `json:"model"`
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

func (s *UsageScanner) parseSSEEvent(payload []byte) {
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
		}
	}
}

// parseJSONBody handles non-streaming responses for both providers.
func (s *UsageScanner) parseJSONBody(body []byte) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return
	}
	// Anthropic shape: {"model":"...","usage":{"input_tokens":...}}
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

	// OpenAI shape: {"model":"...","usage":{"prompt_tokens":...,"completion_tokens":...}}
	var oai struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &oai); err == nil && oai.Usage != nil &&
		(oai.Usage.PromptTokens > 0 || oai.Usage.CompletionTokens > 0) {
		s.usage.Provider = "openai"
		s.usage.Model = oai.Model
		s.usage.InputTokens = oai.Usage.PromptTokens
		s.usage.OutputTokens = oai.Usage.CompletionTokens
	}
}
