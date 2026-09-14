package ducklord

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func detailIdentity(id string) SessionIdentity {
	return SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: id}
}

func TestFilterDetailedSessionsUnicodeFuzzyAndStableOrder(t *testing.T) {
	a, b, c := detailIdentity("AAA111"), detailIdentity("BBB222"), detailIdentity("CCC333")
	items := []DetailedSessionItem{
		{Identity: a, Name: "Alpha", Host: "remote", Projects: []string{"中文專案", "Ops"}},
		{Identity: b, Name: "Ågent", Host: "host-b", Projects: []string{"Research"}},
		{Identity: c, Name: "Alpine", Host: "host-c", Projects: []string{"Ops"}},
		{Identity: a, Name: "Alpha", Host: "remote", Projects: []string{"中文專案"}},
	}
	for _, tc := range []struct {
		query string
		want  []SessionIdentity
	}{
		{query: "ALP", want: []SessionIdentity{a, c}},
		{query: "中專", want: []SessionIdentity{a}},
		{query: "ops", want: []SessionIdentity{a, c}},
		{query: "htb", want: []SessionIdentity{b}},
	} {
		got := FilterDetailedSessions(items, tc.query, DetailAll)
		if len(got) != len(tc.want) {
			t.Fatalf("query %q returned %d items, want %d: %+v", tc.query, len(got), len(tc.want), got)
		}
		for i, want := range tc.want {
			if got[i].Identity != want {
				t.Fatalf("query %q item %d = %+v, want %+v", tc.query, i, got[i].Identity, want)
			}
		}
	}
	if got := FilterDetailedSessions(items, "ÅG", DetailAll); len(got) != 1 || got[0].Identity != b {
		t.Fatalf("Unicode case-insensitive search failed: %+v", got)
	}
	if got := FilterDetailedSessions(items, "中文", DetailAll); len(got) != 1 || got[0].Identity != a {
		t.Fatalf("multi-Project match duplicated Session: %+v", got)
	}
}

func TestFilterDetailedSessionsAppliesStateFilterBeforeRanking(t *testing.T) {
	a, b, c := detailIdentity("AAA111"), detailIdentity("BBB222"), detailIdentity("CCC333")
	items := []DetailedSessionItem{
		{Identity: a, Name: "alpha", Host: "remote", Unread: true, NeedsAction: true},
		{Identity: b, Name: "alpine", Host: "alpha-host", Unread: true, Disconnected: true},
		{Identity: c, Name: "beta", Host: "alpha", NeedsAction: true, Disconnected: true},
	}
	for _, tc := range []struct {
		filter DetailFilter
		want   []SessionIdentity
	}{
		{DetailAll, []SessionIdentity{a, c, b}},
		{DetailUnread, []SessionIdentity{a, b}},
		{DetailNeedsAction, []SessionIdentity{a, c}},
		{DetailDisconnected, []SessionIdentity{c, b}},
	} {
		got := FilterDetailedSessions(items, "alpha", tc.filter)
		if len(got) != len(tc.want) {
			t.Fatalf("filter %s = %+v", tc.filter, got)
		}
		for i := range got {
			if got[i].Identity != tc.want[i] {
				t.Fatalf("filter %s item %d = %+v, want %+v", tc.filter, i, got[i].Identity, tc.want[i])
			}
		}
	}
	if got := FilterDetailedSessions(items, "", DetailFilter("invalid")); len(got) != 0 {
		t.Fatalf("invalid filter should fail closed: %+v", got)
	}
}

func TestRenderDetailedSessionBodySinglePreviewAndSafeVT(t *testing.T) {
	a, b := detailIdentity("AAA111"), detailIdentity("BBB222")
	items := []DetailedSessionItem{{Identity: a, Name: "Alpha", Host: "one", Projects: []string{"Work"}, Type: "Codex", Writer: "laptop", State: "running",
		LastNotification: time.Date(2026, 9, 14, 9, 30, 0, 0, time.UTC), Unread: true},
		{Identity: b, Name: "Beta", Host: "two", Projects: []string{"Docs"}, Type: "Shell", State: "disconnected", Disconnected: true}}
	var output bytes.Buffer
	called := 0
	RenderDetailedSessionBody(&output, CalculateDetailGeometry(100, 12, 1), items, b, "", DetailAll, false,
		func(id SessionIdentity, _, _ int) WorkspacePaneView {
			called++
			if id != b {
				t.Fatalf("rendered non-selected pane %+v", id)
			}
			return WorkspacePaneView{Title: "Beta", Lines: []string{"\x1b[31mRED\x1b[0m\x1b[1;1H\x1b]0;evil\aSAFE"}}
		})
	if called != 1 {
		t.Fatalf("pane callback called %d times", called)
	}
	text := output.String()
	for _, want := range []string{"Alpha", "Beta", "Work", "Docs", "Codex", "Shell", "running", "disconnected", "laptop", "09-14 09:30", "RED", "SAFE", "[disconnected/stale]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("render missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "\x1b]0;evil") || strings.Contains(text, "\x1b[1;1H\x1b]0") {
		t.Fatalf("unsafe VT escape survived renderer: %q", text)
	}
	output.Reset()
	RenderDetailedSessionBody(&output, CalculateDetailGeometry(100, 12, 1), nil, SessionIdentity{}, "missing", DetailUnread, false, nil)
	if !strings.Contains(output.String(), "No matching Sessions") {
		t.Fatalf("empty search retained an old preview: %q", output.String())
	}
}

func TestCalculateDetailGeometryKeepsOnePreviewOnNarrowTerminal(t *testing.T) {
	for _, width := range []int{1, 20, 43, 44, 100} {
		geometry := CalculateDetailGeometry(width, 24, 4)
		if geometry.Pane.Width < 1 || geometry.Pane.X+geometry.Pane.Width-1 > width {
			t.Fatalf("width %d: invalid preview geometry %+v", width, geometry)
		}
		if geometry.List.Width > 0 && geometry.List.X+geometry.List.Width >= geometry.Pane.X {
			t.Fatalf("width %d: overlapping columns %+v", width, geometry)
		}
	}
}

func TestRenderDetailedSessionListScrollsToSelectedCard(t *testing.T) {
	ids := []string{"AAA111", "BBB222", "CCC333", "DDD444", "EEE555", "FFF666"}
	items := make([]DetailedSessionItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, DetailedSessionItem{Identity: detailIdentity(id), Name: id, Host: "host", State: "running"})
	}
	var output bytes.Buffer
	RenderDetailedSessionBody(&output, CalculateDetailGeometry(100, 11, 1), items, detailIdentity("FFF666"), "", DetailAll, false, nil)
	if !strings.Contains(output.String(), "FFF666") || strings.Contains(output.String(), "AAA111") {
		t.Fatalf("selected card was not scrolled into list viewport: %q", output.String())
	}
}
