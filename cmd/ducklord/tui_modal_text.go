package main

import (
	"strings"
	"unicode"
	"unicode/utf8"

	textwidth "golang.org/x/text/width"
)

func modalDisplayText(value string) string {
	value = sanitizeTerminalText(value)
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(value, "�"))
}

func modalRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	switch textwidth.LookupRune(r).Kind() {
	case textwidth.EastAsianWide, textwidth.EastAsianFullwidth:
		return 2
	default:
		return 1
	}
}

// modalDisplayANSI preserves only SGR color sequences while sanitizing all
// other control bytes. It is used by the few views that color independent
// regions within one modal line.
func modalDisplayANSI(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == '[' {
			j := i + 2
			for j < len(value) && (value[j] >= '0' && value[j] <= '9' || value[j] == ';' || value[j] == ':') {
				j++
			}
			if j < len(value) && value[j] == 'm' {
				b.WriteString(value[i : j+1])
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		if r >= ' ' && r != '\x7f' && !unicode.IsControl(r) {
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func modalANSISequence(value string, i int) int {
	if i+1 >= len(value) || value[i] != '\x1b' || value[i+1] != '[' {
		return i
	}
	j := i + 2
	for j < len(value) && (value[j] >= '0' && value[j] <= '9' || value[j] == ';' || value[j] == ':') {
		j++
	}
	if j < len(value) && value[j] == 'm' {
		return j + 1
	}
	return i
}

func modalCellWidth(value string) int {
	width := 0
	for i := 0; i < len(value); {
		if next := modalANSISequence(value, i); next != i {
			i = next
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		width += modalRuneWidth(r)
		i += size
	}
	return width
}

func modalCellTruncate(value string, cells int) string {
	if cells <= 0 {
		return ""
	}
	if modalCellWidth(value) <= cells {
		return value
	}
	ellipsis := "…"
	limit := max(0, cells-modalCellWidth(ellipsis))
	var b strings.Builder
	used := 0
	for i := 0; i < len(value); {
		if next := modalANSISequence(value, i); next != i {
			b.WriteString(value[i:next])
			i = next
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		w := modalRuneWidth(r)
		if used+w > limit {
			break
		}
		b.WriteString(value[i : i+size])
		used += w
		i += size
	}
	b.WriteString(ellipsis)
	if strings.Contains(value, "\x1b[") {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func modalCellPad(value string, cells int) string {
	return value + strings.Repeat(" ", max(0, cells-modalCellWidth(value)))
}
