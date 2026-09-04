package handlers

import (
	"strings"
	"testing"
)

func TestSplitDiscordContentKeepsShortMessageSinglePart(t *testing.T) {
	parts := splitDiscordContent("short reply")
	if len(parts) != 1 || parts[0] != "short reply" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestSplitDiscordContentSplitsLongMessages(t *testing.T) {
	parts := splitDiscordContent(strings.Repeat("abcdefghi\n", 260))
	if len(parts) < 2 {
		t.Fatalf("expected long message to split, got %d part(s)", len(parts))
	}
	for i, part := range parts {
		if len([]rune(part)) > 2000 {
			t.Fatalf("part %d length = %d, want <= 2000", i, len([]rune(part)))
		}
		if !strings.HasPrefix(part, "(part ") {
			t.Fatalf("part %d missing prefix: %.32q", i, part)
		}
	}
}

func TestCanonicalDucklionSessionID(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"ABC123", "ABC123"},
		{"abc123", "ABC123"},
	} {
		got, err := canonicalDucklionSessionID(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("canonicalDucklionSessionID(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, invalid := range []string{"ABC12", "ABC12I", "ABC12O", "ABC12-"} {
		if _, err := canonicalDucklionSessionID(invalid); err == nil {
			t.Fatalf("canonicalDucklionSessionID(%q) unexpectedly succeeded", invalid)
		}
	}
}
