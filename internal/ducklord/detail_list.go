package ducklord

import (
	"sort"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type DetailFilter string

const (
	DetailAll          DetailFilter = "all"
	DetailUnread       DetailFilter = "unread"
	DetailNeedsAction  DetailFilter = "needs_action"
	DetailDisconnected DetailFilter = "disconnected"
)

// DetailedSessionItem is already-known local metadata. Searching this slice
// never queries a Host or changes notification seen state.
type DetailedSessionItem struct {
	Identity         SessionIdentity
	Name             string
	Host             string
	Projects         []string
	Type             string
	Writer           string
	State            string
	LastNotification time.Time
	Unread           bool
	NeedsAction      bool
	Disconnected     bool
}

// FilterDetailedSessions keeps the caller's base order for equal-quality
// matches. One Session appears once even if several Project names match.
func FilterDetailedSessions(items []DetailedSessionItem, query string, filter DetailFilter) []DetailedSessionItem {
	query = detailFold(strings.TrimSpace(query))
	type ranked struct {
		item  DetailedSessionItem
		score int
		order int
	}
	var matches []ranked
	seen := make(map[SessionIdentity]bool, len(items))
	for index, item := range items {
		if item.Identity.Key() == "" || seen[item.Identity] {
			continue
		}
		seen[item.Identity] = true
		if !detailFilterMatches(item, filter) {
			continue
		}
		score, ok := detailMatchScore(item, query)
		if ok {
			matches = append(matches, ranked{item: item, score: score, order: index})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		if matches[i].order != matches[j].order {
			return matches[i].order < matches[j].order
		}
		return matches[i].item.Identity.Key() < matches[j].item.Identity.Key()
	})
	result := make([]DetailedSessionItem, len(matches))
	for i := range matches {
		result[i] = matches[i].item
	}
	return result
}

func detailFilterMatches(item DetailedSessionItem, filter DetailFilter) bool {
	switch filter {
	case DetailAll:
		return true
	case DetailUnread:
		return item.Unread
	case DetailNeedsAction:
		return item.NeedsAction
	case DetailDisconnected:
		return item.Disconnected
	default:
		return false
	}
}

func detailFold(value string) string {
	return cases.Fold().String(norm.NFKC.String(value))
}

// Lower scores are better. Name matches win over Host matches, which win
// over Project matches; exact/prefix/substring beat fuzzy subsequences.
func detailMatchScore(item DetailedSessionItem, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	best := 1 << 30
	fields := append([]string{item.Name, item.Host}, item.Projects...)
	for index, field := range fields {
		folded := detailFold(field)
		base := 200
		switch index {
		case 0:
			base = 0
		case 1:
			base = 100
		}
		if folded == query {
			best = min(best, base)
		} else if strings.HasPrefix(folded, query) {
			best = min(best, base+10)
		} else if strings.Contains(folded, query) {
			best = min(best, base+20)
		} else if detailSubsequence(folded, query) {
			best = min(best, base+30)
		}
	}
	return best, best != 1<<30
}

func detailSubsequence(value, query string) bool {
	remaining := []rune(query)
	for _, r := range value {
		if len(remaining) != 0 && r == remaining[0] {
			remaining = remaining[1:]
		}
	}
	return len(remaining) == 0
}
