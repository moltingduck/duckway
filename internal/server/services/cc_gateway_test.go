package services

import (
	"testing"
	"time"
)

// TestCCReconnectDelay_GrowsOnUnstable verifies that repeated short-lived
// connections back off exponentially (capped), instead of re-identifying every
// fixed interval and tripping Discord's IDENTIFY rate limit.
func TestCCReconnectDelay_GrowsOnUnstable(t *testing.T) {
	min, max, stable := 1*time.Second, 60*time.Second, 60*time.Second
	unstableUptime := 3 * time.Second // like the 13:03 invalid-session storm

	base := min
	wantWaits := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second, // capped
		60 * time.Second, // stays capped
	}
	for i, want := range wantWaits {
		var wait time.Duration
		wait, base = ccReconnectDelay(base, unstableUptime, min, max, stable)
		if wait != want {
			t.Fatalf("attempt %d: wait = %v, want %v", i, wait, want)
		}
	}
}

// TestCCReconnectDelay_ResetsOnStable verifies that a connection that stayed up
// long enough (e.g. a routine op-7 reconnect after 45 min) is not penalized —
// the next wait drops back to the minimum.
func TestCCReconnectDelay_ResetsOnStable(t *testing.T) {
	min, max, stable := 1*time.Second, 60*time.Second, 60*time.Second

	// Drive the backoff up first.
	base := min
	for i := 0; i < 5; i++ {
		_, base = ccReconnectDelay(base, 2*time.Second, min, max, stable)
	}
	if base <= min {
		t.Fatalf("precondition: backoff should have grown, got %v", base)
	}

	// A healthy, long-lived connection now closes — wait must reset to min.
	wait, next := ccReconnectDelay(base, 45*time.Minute, min, max, stable)
	if wait != min {
		t.Fatalf("stable connection: wait = %v, want %v", wait, min)
	}
	if next != 2*time.Second {
		t.Fatalf("stable connection: nextBase = %v, want %v", next, 2*time.Second)
	}
}

// TestCCReconnectDelay_CapAtMax guards the upper bound directly.
func TestCCReconnectDelay_CapAtMax(t *testing.T) {
	min, max, stable := 1*time.Second, 60*time.Second, 60*time.Second
	_, next := ccReconnectDelay(40*time.Second, 1*time.Second, min, max, stable)
	if next != max {
		t.Fatalf("nextBase = %v, want cap %v", next, max)
	}
}
