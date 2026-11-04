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

// Credential cell: name on line one, service slug muted on line two; service display names stay whole.
func TestCredentialAndServiceNameCellsDoNotWrap(t *testing.T) {
	pt := readWeb(t, "templates/phantom_tokens.html")
	mustContain(t, "phantom_tokens.html", pt, `<span class="pt-cred-name">`, `<span class="pt-svc">{{.ServiceName}}</span>`)
	if strings.Contains(pt, "· {{.ServiceName}}") {
		t.Error("the middle-dot separator belongs to the card layout CSS only")
	}
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		".pt-cred-name, .pt-svc { display: block; white-space: nowrap; }",
		".sv-table td.cl-c-name .cl-name { white-space: nowrap; }",
		`.pt-svc::before { content: " · "; }`)
}

// Desktop table cells never break a word; wide tables scroll inside their card.
func TestDesktopTableCellsNeverBreakWords(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		"td, th, td .mono, td code { overflow-wrap: normal; word-break: normal; }",
		".pt-table td.pt-c-rules { min-width: 13rem; }",
		".cn-table td, .cn-table .badge, .sc-table td, .sc-table code, .sc-table .mono { white-space: nowrap; }",
		"td code, td .mono, td strong, td .nb { white-space: nowrap; }",
		".nm-clip, .lg-clip, .cl-table .cl-c-name .cl-name { display: inline-block; max-width: var(--nm-cap, 32ch);",
		".sc-table td.sc-notes { white-space: normal; min-width: 22rem; }")
	mustContain(t, "clients.html", readWeb(t, "templates/clients.html"), `class="cl-name" title="${c.name}"`)
}

// The canary fleet is a card list at phone width, with a labelled row per column.
func TestCanaryFleetIsCardsAtPhoneWidth(t *testing.T) {
	cn := readWeb(t, "templates/canary.html")
	mustContain(t, "canary.html", cn, `class="cn-table cn-fleet"`, `data-label="Canary"`, `data-label="Tokens"`, `data-label="Last generated"`, `data-label="Last triggered"`)
	mustContain(t, "style.css", readWeb(t, "static/style.css"),
		".cn-fleet thead { display: none; }",
		".cn-fleet tr:hover td, .cn-fleet tr:hover th { background: transparent; }",
		".cn-fleet td[data-label]::before { content: attr(data-label)")
}

// Overview recent requests, the Request log and Supply chain mitigations are stacked cards at phone width.
func TestRemainingTablesAreCardsAtPhoneWidth(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		".ov-recent thead, .lg-table thead, .sc-table thead { display: none; }",
		".ov-req, .lg-tr, .sc-row { display: grid;",
		".ov-c-client { grid-area: 1 / 1;", ".lg-c-client { grid-area: 1 / 1;", ".sc-c-writes { grid-area: 3 / 1 / auto / -1;",
		".lg-tr { cursor: pointer; min-height: 44px; }")
	d := readWeb(t, "templates/dashboard.html")
	if n := strings.Count(d, `class="ov-req"`); n != 2 {
		t.Errorf("dashboard: want server and live rows to share the ov-req card class, got %d", n)
	}
	mustContain(t, "dashboard.html", d, `ov-c-time`, `ov-c-client`, `ov-c-path`, `ov-c-status`, `ov-c-svc`)
	mustContain(t, "logs.html", readWeb(t, "templates/logs.html"), `lg-c-time`, `lg-c-client`, `lg-c-svc`, `lg-c-dec`, `lg-c-st`, `lg-c-ms`, `data-action="openRow"`)
	mustContain(t, "supplychain.html", readWeb(t, "templates/supplychain.html"), `class="sc-row"`, `sc-c-switch`, `sc-c-config`, `sc-c-writes`, `sc-notes`)
}

// Top-bar controls never wrap, the approvals item keeps its service, and client filters are capped.
func TestTopBarApprovalsItemAndFilterCaps(t *testing.T) {
	css := readWeb(t, "static/style.css")
	mustContain(t, "style.css", css,
		".topbar .menu-btn, .topbar .pal-trigger, .topbar .crumb-back, .live-status { flex: none; white-space: nowrap; }",
		".crumbs li.crumb-current { overflow: hidden; text-overflow: ellipsis; }",
		".appr-arrow { white-space: nowrap; overflow: visible; }",
		".appr-row-top { flex-wrap: wrap; row-gap: 4px; }",
		"#pt-f-client, #pt-f-service, #pt-f-source, #pt-f-approval, #f-client, #f-service, #f-credential, #f-status, #us-target { max-width: 14rem; text-overflow: ellipsis; }",
		"#pt-f-client, #f-client { max-width: 11rem; }")
}
