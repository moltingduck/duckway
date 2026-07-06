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
