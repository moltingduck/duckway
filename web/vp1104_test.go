package web

import (
	"strings"
	"testing"
)

func mustContain(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s: missing %q", what, w)
		}
	}
}

// Side panels exist only while something is selected, so tables keep the full width.
func TestEmptySidePanelsAreHiddenUntilSelection(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		".cr-detail:not(.cr-open) { display: none; }",
		".lg-detail:not(.open) { display: none; }",
		".lg-split.lg-has-detail { grid-template-columns: minmax(0, 1fr) 22rem; }",
		".lg-req { min-width: 15rem; }",
		".lg-req .lg-path { overflow-wrap: anywhere; }")
	cr := readWeb(t, "templates/credentials.html")
	mustContain(t, "credentials.html", cr,
		"el('cr-detail').classList.add('cr-open');", // opens at every width, not only as a drawer
		"if (isDrawer()) { show(el('cr-scrim'), true); drawerA11y(true); }")
	if strings.Contains(cr, "if (!isDrawer()) { el('cr-detail').classList.remove('cr-open')") {
		t.Error("leaving drawer width must not close the side panel")
	}
	lg := readWeb(t, "templates/logs.html")
	mustContain(t, "logs.html", lg,
		"panel.parentNode.classList.add('lg-has-detail')",
		"$('log-detail').parentNode.classList.remove('lg-has-detail')",
		`<td class="lg-req">`, `class="lg-clip" title="${l.client_name}"`)
}

// One crumb where group and page share a name; no second in-page crumb on detail pages.
func TestBreadcrumbsHaveNoDuplicates(t *testing.T) {
	for _, name := range []string{"templates/client_detail.html", "templates/phantom_token_detail.html"} {
		body := readWeb(t, name)
		// Only the "Not found" branch keeps its own back link.
		if n := strings.Count(body, `class="cd-crumb"`); n != 1 || !strings.Contains(body, "Not found</span></div>") {
			t.Errorf("%s: want only the Not found crumb, got %d cd-crumb elements", name, n)
		}
	}
	layout := readWeb(t, "templates/layout.html")
	mustContain(t, "layout.html", layout, `class="crumb-back"`, `aria-label="Back to {{.Label}}"`)
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		".crumbs li:not(.crumb-current) { display: none; }",
		".live-status { flex: none; }",
		".live-status .live-text { overflow: visible; text-overflow: clip; }",
		"min-width: 44px; min-height: 44px; font-size: 1.5rem")
}

func TestClientDetailOverviewIsBalanced(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		".cd-stats { grid-template-columns: repeat(3, minmax(0, 1fr)); }",
		".cd-side { flex: 0 0 20rem; min-width: 20rem; }")
}

// The Updates-available nudge needs unseen changes: identical refetches and a parked pointer do not count.
func TestApprovalsNudgeNeedsUnseenChanges(t *testing.T) {
	a := readWeb(t, "templates/approvals.html")
	mustContain(t, "approvals.html", a,
		"if (!box.matches(':hover') || !lastPointer) return false;",
		"JSON.stringify(data) === JSON.stringify(allApprovals)")
}

func TestOverviewRecentClientNamesAreClipped(t *testing.T) {
	d := readWeb(t, "templates/dashboard.html")
	mustContain(t, "dashboard.html", d, "${l.client_name ? DW.html`<span class=\"lg-clip\"", `<span class="lg-clip" title="{{.ClientName}}">{{.ClientName}}</span>`)
}

// Every <details> gets a chevron (except the More menu and the systemd block, which draw their own).
func TestDetailsHaveADisclosureChevron(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		"details:not(.more-menu):not(.cl-systemd) > summary::before",
		"details:not(.more-menu):not(.cl-systemd)[open] > summary::before { transform: rotate(45deg); }",
		"details:not(.more-menu) > summary:focus-visible",
		"cursor: pointer; list-style: none;")
	cn := readWeb(t, "templates/canary.html")
	mustContain(t, "canary.html", cn, `id="cn-set-sum"`, "Alert email: not set", "charAt(0) + '***'")
	if strings.Contains(cn, "Alert email: ' + email") {
		t.Error("the full alert address must not appear in the summary")
	}
}

// The wizard has no in-page crumb; the top bar carries "Add client".
func TestOnboardingHasNoInPageCrumb(t *testing.T) {
	ob := readWeb(t, "templates/onboarding.html")
	if strings.Contains(ob, "wz-crumb\"") || strings.Contains(ob, `aria-label="Breadcrumb"`) {
		t.Error("onboarding must not render its own breadcrumb")
	}
	mustContain(t, "onboarding.html", ob, `id="wz-draftnote"`)
}

// Client names and source pills are clipped with a title everywhere they sit in a table or list;
// service host patterns break only at commas.
func TestClientNamesAndPillsAreClippedEverywhere(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css, ".nm-clip { white-space: nowrap; }", ".lg-clip { white-space: nowrap; }", ".sv-host { display: inline-block; white-space: nowrap; }")
	if strings.Contains(css, "max-width: 12rem; overflow: hidden") || strings.Contains(css, ".lg-clip { display: inline-block; max-width") {
		t.Error("names must show in full on desktop, not be ellipsised")
	}
	for name, want := range map[string][]string{
		"templates/phantom_tokens.html": {`class="nm-clip" title="{{.ClientName}}"`, `cl-pill cl-pill-info nm-clip`},
		"templates/approvals.html":      {`appr-client nm-clip`},
		"templates/usage.html":          {`<strong class="nm-clip"`, `<span class="nm-clip" title="${s.client_name`},
		"templates/canary.html":         {`<span class="nm-clip" title="${c.name}">`},
		"templates/client_detail.html":  {`cl-pill cl-pill-info nm-clip`},
		"templates/key_bundles.html":    {`<a class="nm-clip" title="${p.client_name`},
		"templates/cc.html":             {`<span class="nm-clip" title="${c.client_name`},
		"templates/client_updates.html": {`ro-job-client nm-clip`},
		"static/plugins.js":             {`'nm-clip'`},
		"templates/services.html":       {`function hostSpans(v)`, `<span class="sv-host">`, `<wbr>`, `${hostSpans(s.host_pattern)}`},
	} {
		mustContain(t, name, readWeb(t, name), want...)
	}
}

// Env-var names stay on one line, and mono text in a cell is a plain font (no whole-cell slab).
func TestEnvVarNamesAreNowrapAndMonoCellsHaveNoSlab(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		".mono-text { font-family: var(--mono); font-size: 0.8125rem; }",
		".env-clip, .pt-env, .cd-env { white-space: nowrap; }")
	pt := readWeb(t, "templates/phantom_tokens.html")
	mustContain(t, "phantom_tokens.html", pt, `class="pt-env" title="{{.EnvName}}"`, `<td class="pt-c-token mono-text"`)
	if strings.Contains(pt, `pt-c-token mono"`) {
		t.Error("the token cell must not use the .mono slab")
	}
	mustContain(t, "services.html", readWeb(t, "templates/services.html"), `<td class="sv-c-host mono-text text-sm"`)
	mustContain(t, "key_bundles.html", readWeb(t, "templates/key_bundles.html"), `class="mono-text env-clip" title="${p.env_name}"`)
	mustContain(t, "client_detail.html", readWeb(t, "templates/client_detail.html"), `class="cd-env" title="${p.envName}"`)
}
