package services

import (
	"testing"
	"time"
)

func TestApprovals_Resolve(t *testing.T) {
	r := NewCCApprovalRegistry()
	ch := r.Register("M1", map[string]string{"✅": "yes", "❌": "no"}, nil)

	if ok := r.Resolve("M1", "✅", "U1"); !ok {
		t.Error("Resolve returned false on valid emoji")
	}
	select {
	case res := <-ch:
		if res.Chosen != "yes" || res.Emoji != "✅" || res.ReactorUserID != "U1" {
			t.Errorf("got %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("never got result")
	}
	if r.Pending() != 0 {
		t.Errorf("pending should drop on resolve, got %d", r.Pending())
	}
}

func TestApprovals_UntrackedEmoji(t *testing.T) {
	r := NewCCApprovalRegistry()
	r.Register("M1", map[string]string{"✅": "yes"}, nil)
	if ok := r.Resolve("M1", "🍕", "U1"); ok {
		t.Error("untracked emoji should not resolve")
	}
	if r.Pending() != 1 {
		t.Errorf("pending should remain on bad emoji, got %d", r.Pending())
	}
}

func TestApprovals_RestrictedReactor(t *testing.T) {
	r := NewCCApprovalRegistry()
	ch := r.Register("M1", map[string]string{"✅": "yes"}, []string{"admin1"})

	if ok := r.Resolve("M1", "✅", "intruder"); ok {
		t.Error("non-allowed reactor should not resolve")
	}
	select {
	case res := <-ch:
		t.Fatalf("unexpected resolve from intruder: %+v", res)
	case <-time.After(50 * time.Millisecond):
	}
	if ok := r.Resolve("M1", "✅", "admin1"); !ok {
		t.Error("allowed reactor should resolve")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("admin's resolve didn't deliver")
	}
}

func TestApprovals_Cancel(t *testing.T) {
	r := NewCCApprovalRegistry()
	r.Register("M1", map[string]string{"✅": "yes"}, nil)
	r.Cancel("M1")
	if r.Pending() != 0 {
		t.Errorf("Cancel should drop, got %d", r.Pending())
	}
	if ok := r.Resolve("M1", "✅", "U1"); ok {
		t.Error("Resolve should return false after Cancel")
	}
}

func TestApprovals_FirstReactionWins(t *testing.T) {
	r := NewCCApprovalRegistry()
	ch := r.Register("M1", map[string]string{"✅": "yes", "❌": "no"}, nil)

	if !r.Resolve("M1", "✅", "U1") {
		t.Fatal("first resolve failed")
	}
	if r.Resolve("M1", "❌", "U2") {
		t.Error("second resolve should not succeed (already resolved)")
	}
	res := <-ch
	if res.Chosen != "yes" {
		t.Errorf("first reactor's choice should win: %+v", res)
	}
}

func TestDefaultEmojiForOption(t *testing.T) {
	cases := []struct {
		i, total int
		want     string
	}{
		{0, 2, "✅"},
		{1, 2, "❌"},
		{0, 3, "1️⃣"},
		{1, 3, "2️⃣"},
		{9, 10, "🔟"},
		{10, 12, "❓"},
	}
	for _, c := range cases {
		if got := DefaultEmojiForOption(c.i, c.total); got != c.want {
			t.Errorf("DefaultEmojiForOption(%d,%d) = %q, want %q", c.i, c.total, got, c.want)
		}
	}
}
