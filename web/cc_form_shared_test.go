package web

import (
	"os"
	"strings"
	"testing"
)

// The wizard (step 3) and the Control channels drawer must both load the
// shared form module and must not carry their own copy of its code.
func TestControlChannelFormIsShared(t *testing.T) {
	shared, err := os.ReadFile("static/cc-form.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DW.ccForm", "agentField", "credentialField", "serverField", "categoryField", "checkRow", "kvFieldset", "kvApply", "kvFromConfig", "SECRET_RE", "MASK"} {
		if !strings.Contains(string(shared), want) {
			t.Errorf("cc-form.js missing %s", want)
		}
	}
	forbidden := []string{
		"var SECRET_RE = /", "var MASK = '", "var SANDBOXES = [", "var AGENTS",
		"function kvFromConfig", "function stepList", "function resultCard",
		"'Claude Code'", "'danger-full-access', 'none'", "Stored on server, leave blank to keep",
		"Check again</button>", "<label for=\"wz-agent\">", "<label for=\"cf-agent\">",
		"<option value=\"__new__\"",
	}
	for _, page := range []string{"templates/onboarding.html", "templates/cc.html"} {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, `src="/static/cc-form.js"`) {
			t.Errorf("%s does not load /static/cc-form.js", page)
		}
		if !strings.Contains(src, "DW.ccForm") && !strings.Contains(src, "CF.") {
			t.Errorf("%s does not use DW.ccForm", page)
		}
		for _, bad := range forbidden {
			if strings.Contains(src, bad) {
				t.Errorf("%s still duplicates shared form code: %s", page, bad)
			}
		}
		if strings.Contains(src, " style=") || strings.Contains(src, "alert(") || strings.Contains(src, "confirm(") {
			t.Errorf("%s has inline style or blocking dialog", page)
		}
	}
	if strings.Contains(string(shared), "innerHTML") || strings.Contains(string(shared), "style=") {
		t.Error("cc-form.js must use DW.html only and no inline styles")
	}
	css, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "Shared control-channel form (ccf-*") {
		t.Error("style.css missing the ccf- section")
	}
}
