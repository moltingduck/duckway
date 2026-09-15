package ducklord

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestFilterDetailedSessionsRanksQualityAcrossFields(t *testing.T) {
	items := []DetailedSessionItem{
		{Identity: detailIdentity("AAA111"), Name: "a-l-p-h-a"},
		{Identity: detailIdentity("CCC333"), Name: "unrelated", Host: "alpha"},
		{Identity: detailIdentity("BBB222"), Name: "other", Projects: []string{"alpha", "alpha-work"}},
	}
	got := FilterDetailedSessions(items, "alpha", DetailAll)
	want := []SessionIdentity{items[1].Identity, items[2].Identity, items[0].Identity}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %d Sessions", got, len(want))
	}
	for i, identity := range want {
		if got[i].Identity != identity {
			t.Fatalf("result %d = %+v, want %+v", i, got[i].Identity, identity)
		}
	}
}

func TestFilterDetailedSessionsEqualQualityKeepsBaseOrderAcrossFields(t *testing.T) {
	for _, value := range []string{"alpha", "alpha-work", "work-alpha-end", "a-l-p-h-a"} {
		t.Run(value, func(t *testing.T) {
			items := []DetailedSessionItem{
				{Identity: detailIdentity("CCC333"), Projects: []string{value}},
				{Identity: detailIdentity("BBB222"), Host: value},
				{Identity: detailIdentity("AAA111"), Name: value},
			}
			got := FilterDetailedSessions(items, "alpha", DetailAll)
			if len(got) != len(items) {
				t.Fatalf("results = %+v, want %d Sessions", got, len(items))
			}
			for i, item := range items {
				if got[i].Identity != item.Identity {
					t.Fatalf("equal-quality result %d = %+v, want base order %+v", i, got[i].Identity, item.Identity)
				}
			}
		})
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
	for _, width := range []int{55, 100} {
		for _, height := range []int{3, 4, 5, 11} {
			var output bytes.Buffer
			geometry := CalculateDetailGeometry(width, height, 1)
			renderDetailList(&output, geometry.List, items, len(items)-1, "", DetailAll)
			if !strings.Contains(output.String(), "› FFF666") || strings.Contains(output.String(), "AAA111") {
				t.Fatalf("width %d height %d selected card was not scrolled into list viewport: %q", width, height, output.String())
			}
		}
	}
}

func TestRenderDetailedOfflineUnselectedRowIsMuted(t *testing.T) {
	items := []DetailedSessionItem{
		{Identity: detailIdentity("AAA111"), Name: "first", Host: "host", State: "disconnected", Disconnected: true},
		{Identity: detailIdentity("BBB222"), Name: "second", Host: "host", State: "disconnected", Disconnected: true},
	}
	var output bytes.Buffer
	RenderDetailedSessionBody(&output, CalculateDetailGeometry(110, 14, 1), items, items[0].Identity, "", DetailAll, false, nil)
	if !strings.Contains(output.String(), "\x1b[2;37m") || !strings.Contains(output.String(), "second @host") {
		t.Fatalf("offline unselected row was not muted: %q", output.String())
	}
}

func TestRenderDetailedOfflineSelectedRowStaysMuted(t *testing.T) {
	item := DetailedSessionItem{
		Identity: detailIdentity("AAA111"), Name: "offline", Host: "host",
		State: "disconnected", Disconnected: true,
	}
	var output bytes.Buffer
	renderDetailList(&output, WorkspaceRect{X: 1, Y: 1, Width: 40, Height: 5}, []DetailedSessionItem{item}, 0, "", DetailAll)
	if !strings.Contains(output.String(), "\x1b[2;37m› offline @host") {
		t.Fatalf("selected disconnected Session lost gray styling or selection arrow: %q", output.String())
	}
	if !strings.Contains(output.String(), "\x1b[2;37m  disconnected") {
		t.Fatalf("selected disconnected Session state lost gray styling: %q", output.String())
	}
}

func TestDetailedRowsReserveMetadataForLongNames(t *testing.T) {
	for _, width := range []int{22, 39, 40, 52} {
		for _, name := range []string{strings.Repeat("long-name", 12), strings.Repeat("中文e\u0301", 20)} {
			item := DetailedSessionItem{
				Identity: detailIdentity("AAA111"), Name: name, Host: name,
				Projects: []string{name, name}, Type: "other agent?",
				Writer: name, State: "disconnected", Disconnected: true, Unread: true,
				LastNotification: time.Date(2026, 9, 14, 9, 30, 0, 0, time.UTC),
			}
			rows := 3
			if width < 40 {
				rows = 4
			}
			var lines []string
			for row := 0; row < rows; row++ {
				line := detailRowLine(item, row, width, "› ")
				if !utf8.ValidString(line) || workspaceCellWidth(line) > width {
					t.Fatalf("width %d row %d exceeds cell budget or breaks Unicode: %q", width, row, line)
				}
				lines = append(lines, line)
			}
			text := strings.Join(lines, "\n")
			for _, want := range []string{" @", " •", "other agent?", "disconnected", "09-14 09:30", "…"} {
				if !strings.Contains(text, want) {
					t.Fatalf("width %d missing metadata %q: %q", width, want, text)
				}
			}
			r, _ := utf8.DecodeRuneInString(name)
			if !strings.Contains(lines[2], " · "+string(r)) {
				t.Fatalf("width %d lost writer: %q", width, lines[2])
			}
			var output bytes.Buffer
			renderDetailList(&output, WorkspaceRect{X: 1, Y: 1, Width: width, Height: rows + 2}, []DetailedSessionItem{item}, 0, "", DetailAll)
			if !strings.Contains(output.String(), "09-14 09:30") || !strings.Contains(output.String(), " •") {
				t.Fatalf("width %d renderer clipped reserved metadata: %q", width, output.String())
			}
		}
	}
}

func TestDetailedNarrowRowsShowMissingWriterAndNotification(t *testing.T) {
	item := DetailedSessionItem{State: "disconnected"}
	if got := detailRowLine(item, 2, 22, "  "); !strings.Contains(got, "none") {
		t.Fatalf("missing writer was not shown: %q", got)
	}
	if got := detailRowLine(item, 3, 22, "  "); !strings.Contains(got, "never") {
		t.Fatalf("missing notification was not shown: %q", got)
	}
}
