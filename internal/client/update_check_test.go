package client

import (
	"testing"
	"time"
)

func TestDeterministicJitter(t *testing.T) {
	window := 30 * time.Minute
	a := deterministicJitter("client-a", window)
	b := deterministicJitter("client-a", window)
	if a != b {
		t.Fatalf("jitter is not deterministic: %s != %s", a, b)
	}
	if a < 0 || a >= window {
		t.Fatalf("jitter out of range: %s", a)
	}
	if got := deterministicJitter("client-a", 0); got != 0 {
		t.Fatalf("zero window jitter = %s", got)
	}
}

func TestJitterSeedSeparatesComponents(t *testing.T) {
	cfg := &Config{ServerURL: "https://duckway.example", ClientName: "agent-1", Token: "tok"}
	proxy := deterministicJitter(jitterSeed(cfg, "proxy", "bucket"), 30*time.Minute)
	watch := deterministicJitter(jitterSeed(cfg, "cc-watch", "bucket"), 30*time.Minute)
	if proxy == watch {
		t.Fatalf("expected different component jitter offsets, both were %s", proxy)
	}
}
