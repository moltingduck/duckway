package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestUndoIsUsedAndKeyboardReachable(t *testing.T) {
	for _, f := range []struct{ file, want string }{
		{"templates/key_pools.html", "undo: function () { var cp = curPool()"},
		{"templates/notifications.html", "undo: function () { return setActive(id, prev, true)"},
		{"templates/supplychain.html", "undo: function() { return setMitigation("},
	} {
		if !strings.Contains(readWeb(t, f.file), f.want) {
			t.Errorf("%s: Undo toast not wired (%q)", f.file, f.want)
		}
	}
	dw := readWeb(t, "static/dw.js")
	for _, want := range []string{"(e.ctrlKey || e.metaKey)", "#toast .toast-undo", "Undo (", "o.dismiss"} {
		if !strings.Contains(dw, want) {
			t.Errorf("dw.js missing %q", want)
		}
	}
	if !strings.Contains(readWeb(t, "static/live.js"), "dismiss: true") {
		t.Error("the live retry toast needs a Dismiss button")
	}
	layout := readWeb(t, "templates/layout.html")
	if !strings.Contains(layout, `href="#toast"`) || !strings.Contains(layout, `id="toast" tabindex="-1"`) {
		t.Error("layout needs a skip link to the focusable toast region")
	}
}

func TestMoreMenuAriaAndNoSingleItemMenus(t *testing.T) {
	dw := readWeb(t, "static/dw.js")
	if !strings.Contains(dw, "function initMenus") || !strings.Contains(dw, "if (sum) sum.focus(); closeMenu(d, false)") {
		t.Error("More menus must get aria-haspopup/aria-expanded at load and Tab must move focus to the summary first")
	}
	// A menu with a single item is not allowed: pool member rows use a plain Remove button.
	pools := readWeb(t, "templates/key_pools.html")
	if strings.Contains(pools, `data-action="askRemove"`) && regexp.MustCompile(`more-menu[^\n]*askRemove`).MatchString(pools) {
		t.Error("key pool member rows must show Remove directly, not a one-item menu")
	}
}

func TestTermHintsAreOutsideHeadingNames(t *testing.T) {
	entries, _ := fs.ReadDir(Content, "templates")
	for _, e := range entries {
		body := readWeb(t, "templates/"+e.Name())
		for _, tag := range []string{"h1", "h2", "h3", "h4", "th"} {
			rest := body
			for {
				i := strings.Index(rest, "<"+tag)
				if i < 0 {
					break
				}
				rest = rest[i+1:]
				if len(rest) <= len(tag) || (rest[len(tag)] != ' ' && rest[len(tag)] != '>') {
					continue
				}
				end := strings.Index(rest, "</"+tag+">")
				if end < 0 {
					break
				}
				seg := rest[:end]
				open := seg[:strings.Index(seg, ">")+1]
				if strings.Contains(seg, `class="term-hint"`) && !strings.Contains(open, "aria-labelledby") {
					t.Errorf("%s: a term hint sits inside <%s>: %.100s", e.Name(), tag, seg)
				}
			}
		}
	}
	if !strings.Contains(readWeb(t, "static/dw.js"), "dw-term-status") {
		t.Error("opening a hint must announce the definition through a status region")
	}
}

// In the light theme --raised equals --surface, so fills must use --track.
func TestTrackTokenReplacesRaisedFills(t *testing.T) {
	css := readWeb(t, "static/style.css")
	if strings.Count(css, "--track:") < 3 {
		t.Error("--track must be defined for dark, light and auto-light")
	}
	for _, line := range strings.Split(css, "\n") {
		if !strings.Contains(line, "var(--raised)") {
			continue
		}
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "--bg-input") || strings.HasPrefix(s, ".more-menu-list") || strings.HasPrefix(s, ".term-pop") {
			continue // inputs have borders; menus and popovers float with a shadow
		}
		t.Errorf("var(--raised) used as a fill on a surface (use --track): %.100s", s)
	}
	if !strings.Contains(css, `.crumbs li + li::before { content: "/";`) {
		t.Error("breadcrumb separator must be plain content")
	}
}

func TestOverviewLiveWordingFollowsTransport(t *testing.T) {
	html := readWeb(t, "templates/dashboard.html")
	if !strings.Contains(html, "DW.live.state() === 'live'") || !strings.Contains(html, "'Updated '") {
		t.Error("Overview says Live only while SSE is connected, otherwise Updated")
	}
	if !strings.Contains(html, "excludes held/denied") {
		t.Error("Overview must say held and denied requests are excluded")
	}
}

func TestSettingsHasRequestLogRetention(t *testing.T) {
	html := readWeb(t, "templates/settings.html")
	for _, want := range []string{`id="s-log-retention"`, "request_log_retention_days", "1 to 3650"} {
		if !strings.Contains(html, want) {
			t.Errorf("settings missing %q", want)
		}
	}
}
