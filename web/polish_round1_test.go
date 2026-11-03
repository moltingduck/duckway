package web

import (
	"strings"
	"testing"
)

func TestPaletteMacDetectionAndNavSearchLabel(t *testing.T) {
	js := readWeb(t, "static/palette.js")
	if !strings.Contains(js, "/mac|iphone|ipad/i") {
		t.Error("platform test must be case-insensitive (Chromium reports \"macOS\")")
	}
	if !strings.Contains(js, "getElementById('nav-search')") || !strings.Contains(js, "setAttribute('aria-label', hintLabel)") {
		t.Error("#nav-search must get the platform-correct aria-label like the top-bar trigger")
	}
}

func TestTourSemantics(t *testing.T) {
	js := readWeb(t, "static/tour.js")
	if strings.Contains(js, "'aria-label'") {
		t.Error("the tour popover is named by aria-labelledby only")
	}
	for _, want := range []string{"aria-labelledby", "aria-modal", "e.ctrlKey || e.metaKey"} {
		if !strings.Contains(js, want) {
			t.Errorf("tour.js missing %q", want)
		}
	}
}

func TestUsageRangeNoteStaysMountedAndEscapeClosesCustom(t *testing.T) {
	html := readWeb(t, "templates/usage.html")
	if strings.Contains(html, "us-range-note').hidden") {
		t.Error("the role=status range note must stay mounted: toggle content only")
	}
	if !strings.Contains(html, "e.key !== 'Escape' || !customOpen") {
		t.Error("Escape must close the custom range panel")
	}
}

func TestApprovalsCopyIsAGrantNotOneShot(t *testing.T) {
	html := readWeb(t, "templates/approvals.html")
	if strings.Contains(html, "Next request asks again") || strings.Contains(html, "this one request only") || strings.Contains(html, "for 24 hours") {
		t.Error("an approval is a grant until it expires, not a single request")
	}
	for _, want := range []string{"that exact request (method, path, query and body)", "this token", "fmtTTL(a.approval_ttl_minutes)", "approval_ttl_minutes; 24 hours by default"} {
		if !strings.Contains(html, want) {
			t.Errorf("approvals copy missing %q", want)
		}
	}
}
