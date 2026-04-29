package services

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UsageSnapshot is the persisted JSON shape stored on api_keys.usage_snapshot.
// It captures whatever rate-limit info the upstream LLM provider gave us on
// the most recent successful request — Anthropic and OpenAI use different
// header names, so the inner shape is provider-agnostic.
type UsageSnapshot struct {
	UpdatedAt string                  `json:"updated_at"`
	Provider  string                  `json:"provider"` // "anthropic", "openai", ""
	Metrics   map[string]UsageMetric  `json:"metrics"`  // keyed by metric name (requests, input_tokens, ...)
	Subscription map[string]string    `json:"subscription,omitempty"` // claude max unified-5h-status etc
}

// UsageMetric describes one rate-limited dimension. Reset is RFC 3339 if the
// upstream sent a timestamp; if it sent seconds we add to the parse time.
type UsageMetric struct {
	Limit     int64  `json:"limit"`
	Remaining int64  `json:"remaining"`
	Reset     string `json:"reset,omitempty"` // RFC 3339
}

// ParseRateLimits inspects upstream response headers and returns a snapshot
// if any known LLM rate-limit headers are present. Returns nil when the
// response carries no recognised headers (e.g. internal services, GitHub).
func ParseRateLimits(h http.Header) *UsageSnapshot {
	snap := &UsageSnapshot{
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Metrics:   map[string]UsageMetric{},
	}

	parseAnthropic(h, snap)
	parseOpenAI(h, snap)

	if len(snap.Metrics) == 0 && len(snap.Subscription) == 0 {
		return nil
	}
	if snap.Provider == "" {
		snap.Provider = "unknown"
	}
	return snap
}

// MarshalJSON helper: returns the JSON string for storage, or "" if nil.
func (s *UsageSnapshot) String() string {
	if s == nil {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseAnthropic(h http.Header, snap *UsageSnapshot) {
	prefix := "Anthropic-Ratelimit-"
	dims := []struct{ key, name string }{
		{"Requests", "requests"},
		{"Tokens", "tokens"},
		{"Input-Tokens", "input_tokens"},
		{"Output-Tokens", "output_tokens"},
	}
	saw := false
	for _, d := range dims {
		limit := h.Get(prefix + d.key + "-Limit")
		rem := h.Get(prefix + d.key + "-Remaining")
		reset := h.Get(prefix + d.key + "-Reset")
		if limit == "" && rem == "" {
			continue
		}
		saw = true
		snap.Metrics[d.name] = UsageMetric{
			Limit:     atoi64(limit),
			Remaining: atoi64(rem),
			Reset:     reset,
		}
	}

	// Claude Max OAuth subscription windows: unified-5h-* and unified-7d-*.
	// Anthropic exposes status (allowed/allowed_warning/exceeded) and remaining
	// percentage for the subscription window, not raw token counts.
	subKeys := []string{
		"Anthropic-Ratelimit-Unified-Status",
		"Anthropic-Ratelimit-Unified-5h-Status",
		"Anthropic-Ratelimit-Unified-5h-Remaining",
		"Anthropic-Ratelimit-Unified-5h-Reset",
		"Anthropic-Ratelimit-Unified-7d-Status",
		"Anthropic-Ratelimit-Unified-7d-Remaining",
		"Anthropic-Ratelimit-Unified-7d-Reset",
	}
	for _, k := range subKeys {
		if v := h.Get(k); v != "" {
			if snap.Subscription == nil {
				snap.Subscription = map[string]string{}
			}
			// strip prefix for nicer display: "5h_status", "5h_remaining", etc.
			label := strings.ToLower(strings.TrimPrefix(k, "Anthropic-Ratelimit-Unified-"))
			label = strings.ReplaceAll(label, "-", "_")
			snap.Subscription[label] = v
			saw = true
		}
	}

	if saw {
		snap.Provider = "anthropic"
	}
}

func parseOpenAI(h http.Header, snap *UsageSnapshot) {
	dims := []struct{ key, name string }{
		{"Requests", "requests"},
		{"Tokens", "tokens"},
	}
	saw := false
	for _, d := range dims {
		limit := h.Get("X-Ratelimit-Limit-" + d.key)
		rem := h.Get("X-Ratelimit-Remaining-" + d.key)
		reset := h.Get("X-Ratelimit-Reset-" + d.key)
		if limit == "" && rem == "" {
			continue
		}
		saw = true
		// OpenAI's reset is a duration like "1.234s" or "1m23s" — leave as-is;
		// the UI can show it raw. Limit/remaining are integers.
		snap.Metrics[d.name] = UsageMetric{
			Limit:     atoi64(limit),
			Remaining: atoi64(rem),
			Reset:     reset,
		}
	}
	if saw && snap.Provider == "" {
		snap.Provider = "openai"
	}
}

func atoi64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
