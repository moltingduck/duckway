package main

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func appendOutputText(current, chunk string, maxLines int) string {
	const maxOutputBytes = 1 << 20
	if chunk == "" {
		return current
	}
	text := applyInteractiveText(current, chunk)
	if len(text) > maxOutputBytes {
		text = strings.ToValidUTF8(text[len(text)-maxOutputBytes:], " ")
	}
	endsWithNewline := strings.HasSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	text = strings.Join(tailLines(lines, maxLines), "\n")
	if endsWithNewline {
		text += "\n"
	}
	return text
}

func applyInteractiveText(current, chunk string) string {
	current = strings.ToValidUTF8(current, " ")
	chunk = strings.ToValidUTF8(chunk, " ")
	out := []rune(current)
	for _, r := range chunk {
		switch {
		case r == '\b' || r == 0x7f:
			if len(out) > 0 && out[len(out)-1] != '\n' {
				out = out[:len(out)-1]
			}
		case r == '\n' || r == '\t':
			out = append(out, r)
		case r == '\r':
			if len(out) == 0 || out[len(out)-1] != '\n' {
				out = append(out, '\n')
			}
		case r < 0x20 || r >= 0x80 && r <= 0x9f:
			out = append(out, ' ')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

func sanitizeTerminalText(s string) string {
	s = strings.ToValidUTF8(s, " ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			b.WriteRune('\n')
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || unicode.Is(unicode.Cf, r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func displayField(s string) string {
	return sanitizeTerminalText(s)
}

func sessionTypeLabel(session ducklord.RemoteSession) string {
	if session.Kind == string(model.KindShell) {
		if session.DetectedForeground == "codex" || session.DetectedForeground == "claude" {
			return session.DetectedForeground + "?" // process detection is advisory, not an exact task state
		}
		if session.DetectedForeground == "other_agent" {
			return "other agent?" // conservative executable detection, not an exact task state
		}
		return "shell"
	}
	if session.AgentType == "" {
		return "agent"
	}
	return "agent:" + session.AgentType
}

func sessionNeedsAttention(session ducklord.RemoteSession) bool {
	return session.AdapterState == string(model.AdapterUnhealthy) ||
		session.Status == string(model.StatusStopped) && (session.ExitSuccess == nil || !*session.ExitSuccess)
}

func formatByteCount(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KiB", float64(bytes)/1024)
}

func formatRetainedUntil(timestampMS int64) string {
	if timestampMS <= 0 {
		return "unknown"
	}
	return time.UnixMilli(timestampMS).Local().Format("Jan 02 15:04")
}

func tailLines(lines []string, max int) []string {
	if max <= 0 {
		return nil
	}
	if len(lines) <= max {
		return lines
	}
	return lines[len(lines)-max:]
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if modalCellWidth(s) <= n {
		return s
	}
	return modalCellTruncate(s, n)
}
