package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestUndoIsUsedAndKeyboardReachable(t *testing.T) {
	for _, f := range []struct{ file, want string }{
		{"templates/key_pools.html", "var target = findPool(poolKind, poolId)"},
		{"templates/notifications.html", "undo: function () { return setActive(id, prev, true)"},
		{"templates/supplychain.html", "undo: function() { return setMitigation("},
	} {
		if !strings.Contains(readWeb(t, f.file), f.want) {
			t.Errorf("%s: Undo toast not wired (%q)", f.file, f.want)
		}
	}
	dw := readWeb(t, "static/dw.js")
	for _, want := range []string{"(e.ctrlKey || e.metaKey)", "#toast .toast-undo", "Undo (", "o.dismiss", "if (e.repeat) return", "data-undo-ok", "stack[stack.length - 1]", "toast('Undo failed'"} {
		if !strings.Contains(dw, want) {
			t.Errorf("dw.js missing %q", want)
		}
	}
	if !strings.Contains(readWeb(t, "static/live.js"), "dismiss: true") {
		t.Error("the live retry toast needs a Dismiss button")
	}
	layout := readWeb(t, "templates/layout.html")
	if !strings.Contains(layout, `data-action="skipToToast" hidden`) || !strings.Contains(layout, `id="toast" tabindex="-1"`) {
		t.Error("layout needs a skip button (hidden until a toast exists) for the focusable toast region")
	}
	if !strings.Contains(dw, "syncSkipToast") || strings.Contains(layout, `href="#toast"`) {
		t.Error("the notifications skip control must not change the URL hash")
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
	dw := readWeb(t, "static/dw.js")
	// Announced once: one sr-only status region, no aria-describedby on the button.
	if !strings.Contains(dw, "getElementById('dw-term-status')") || strings.Contains(dw, "setAttribute('aria-describedby'") {
		t.Error("a hint is announced once, via the status region only")
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
	for _, sel := range []string{".lg-body {", ".cd-pre {", ".cr-cmdhelp {", ".dc-pre {", ".usage-snap {"} {
		for _, line := range strings.Split(css, "\n") {
			if strings.HasPrefix(line, sel) && !strings.Contains(line, "var(--track)") {
				t.Errorf("borderless panel %s must be tinted with --track: %.90s", sel, line)
			}
		}
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
	for _, want := range []string{`id="s-log-retention"`, "request_log_retention_days", "7 to 3650", "Keep forever (not set)", "next hourly cleanup"} {
		if !strings.Contains(html, want) {
			t.Errorf("settings missing %q", want)
		}
	}
}

func TestRound2FrontendContracts(t *testing.T) {
	dw := readWeb(t, "static/dw.js")
	if !strings.Contains(dw, "n.querySelector('details.more-menu')") || strings.Contains(dw, "if (m.addedNodes.length) { initMenus(); break; }") {
		t.Error("the More-menu observer must rescan only when a menu was added")
	}
	settings := readWeb(t, "templates/settings.html")
	for _, want := range []string{"function show(sec, fromHash)", "if (fromHash) return;", "show(location.hash.slice(1), true)", "applies at the next hourly cleanup", "doneRetention"} {
		if !strings.Contains(strings.ToLower(settings), strings.ToLower(want)) {
			t.Errorf("settings missing %q", want)
		}
	}
	if strings.Contains(settings, "retention saved. Clients") {
		t.Error("the retention copy must not claim clients pick it up")
	}
	docs := readWeb(t, "templates/docs.html")
	if !strings.Contains(docs, "Keep forever (not set)") {
		t.Error("docs Upgrade notes must explain the unset retention")
	}
}

// Round 3: a notification Undo that cannot run rejects (so "Undo failed" shows); the skip button
// carries the button reset; the term status region lives in the layout.
func TestRound3FrontendContracts(t *testing.T) {
	nt := readWeb(t, "templates/notifications.html")
	if !strings.Contains(nt, "Promise.reject(new Error(") {
		t.Error("notifications Undo must reject when the channel is busy or gone")
	}
	css := readWeb(t, "static/style.css")
	for _, line := range strings.Split(css, "\n") {
		if strings.HasPrefix(line, ".skip-link {") {
			for _, want := range []string{"font: inherit", "border: 0", "cursor: pointer"} {
				if !strings.Contains(line, want) {
					t.Errorf(".skip-link lacks %q", want)
				}
			}
		}
	}
	if !strings.Contains(readWeb(t, "templates/layout.html"), `id="dw-term-status" class="sr-only" role="status"`) {
		t.Error("layout needs the single term status region")
	}
}

// The Overview "Security posture" card header links to the first Security page.
func TestOverviewPostureCardLinksToSecurity(t *testing.T) {
	d := readWeb(t, "templates/dashboard.html")
	i := strings.Index(d, `id="ov-pos-h"`)
	if i < 0 {
		t.Fatal("posture heading missing")
	}
	head := d[i : i+strings.Index(d[i:], "</div>")]
	if !strings.Contains(head, `<a class="ov-link" href="/admin/canary">Review security settings</a>`) {
		t.Errorf("posture card header lacks the Review security settings link: %s", head)
	}
	if !strings.Contains(readWeb(t, "static/style.css"), ".ov-link, .ov-pos-label a { display: inline-flex; align-items: center; min-height: 44px; }") {
		t.Error("ov-link must keep a 44px target on narrow screens")
	}
}
