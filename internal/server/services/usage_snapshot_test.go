package services

import (
	"net/http"
	"testing"
)

func TestParseRateLimits_Anthropic(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Requests-Limit", "50")
	h.Set("Anthropic-Ratelimit-Requests-Remaining", "47")
	h.Set("Anthropic-Ratelimit-Requests-Reset", "2026-04-29T12:00:00Z")
	h.Set("Anthropic-Ratelimit-Input-Tokens-Limit", "200000")
	h.Set("Anthropic-Ratelimit-Input-Tokens-Remaining", "180000")
	h.Set("Anthropic-Ratelimit-Output-Tokens-Limit", "80000")
	h.Set("Anthropic-Ratelimit-Output-Tokens-Remaining", "79500")

	snap := ParseRateLimits(h)
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if snap.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", snap.Provider)
	}
	if m, ok := snap.Metrics["requests"]; !ok || m.Limit != 50 || m.Remaining != 47 {
		t.Errorf("requests metric = %+v, want limit=50 remaining=47", m)
	}
	if m, ok := snap.Metrics["input_tokens"]; !ok || m.Limit != 200000 || m.Remaining != 180000 {
		t.Errorf("input_tokens = %+v", m)
	}
	if m, ok := snap.Metrics["output_tokens"]; !ok || m.Limit != 80000 {
		t.Errorf("output_tokens = %+v", m)
	}
}

func TestParseRateLimits_AnthropicSubscription(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed_warning")
	h.Set("Anthropic-Ratelimit-Unified-5h-Remaining", "12%")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "2026-04-29T17:00:00Z")

	snap := ParseRateLimits(h)
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if snap.Provider != "anthropic" {
		t.Errorf("provider = %q", snap.Provider)
	}
	if v := snap.Subscription["status"]; v != "allowed" {
		t.Errorf("status = %q, want allowed", v)
	}
	if v := snap.Subscription["5h_remaining"]; v != "12%" {
		t.Errorf("5h_remaining = %q", v)
	}
}

func TestParseRateLimits_OpenAI(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ratelimit-Limit-Requests", "10000")
	h.Set("X-Ratelimit-Remaining-Requests", "9876")
	h.Set("X-Ratelimit-Reset-Requests", "5.4s")
	h.Set("X-Ratelimit-Limit-Tokens", "1000000")
	h.Set("X-Ratelimit-Remaining-Tokens", "987654")

	snap := ParseRateLimits(h)
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if snap.Provider != "openai" {
		t.Errorf("provider = %q", snap.Provider)
	}
	if m := snap.Metrics["requests"]; m.Limit != 10000 || m.Remaining != 9876 || m.Reset != "5.4s" {
		t.Errorf("requests = %+v", m)
	}
	if m := snap.Metrics["tokens"]; m.Limit != 1000000 || m.Remaining != 987654 {
		t.Errorf("tokens = %+v", m)
	}
}

func TestParseRateLimits_NoRelevantHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Github-Request-Id", "abc")

	if snap := ParseRateLimits(h); snap != nil {
		t.Errorf("expected nil for non-LLM response, got %+v", snap)
	}
}

func TestUsageSnapshotString(t *testing.T) {
	var nilSnap *UsageSnapshot
	if s := nilSnap.String(); s != "" {
		t.Errorf("nil snapshot should marshal to empty string, got %q", s)
	}

	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Tokens-Limit", "100")
	h.Set("Anthropic-Ratelimit-Tokens-Remaining", "75")
	snap := ParseRateLimits(h)
	s := snap.String()
	if s == "" || s[0] != '{' {
		t.Errorf("expected JSON object, got %q", s)
	}
}
