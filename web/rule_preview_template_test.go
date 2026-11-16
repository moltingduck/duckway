package web

import (
	"strings"
	"testing"
)

// The three editors load rule-preview.js and carry a host for its result; the
// module renders through DW.html and never touches innerHTML.
func TestRulePreviewWiredIntoEditors(t *testing.T) {
	for _, c := range []struct{ tpl, host string }{
		{"templates/phantom_token_detail.html", `id="pt-impact"`},
		{"templates/credentials.html", `id="acl-impact"`},
		{"templates/services.html", `id="sv-impact"`},
	} {
		src := readContent(t, c.tpl)
		if !strings.Contains(src, `src="/static/rule-preview.js"`) {
			t.Errorf("%s does not load rule-preview.js", c.tpl)
		}
		if !strings.Contains(src, c.host) {
			t.Errorf("%s lacks the preview host %s", c.tpl, c.host)
		}
		if !strings.Contains(src, "checkBeforeSave") && !strings.Contains(readContent(t, "static/phantom-token-detail.js"), "checkBeforeSave") {
			t.Errorf("%s does not ask before saving", c.tpl)
		}
	}
	js := readContent(t, "static/rule-preview.js")
	for _, want := range []string{`aria-live="polite"`, `role="tablist"`, `role="tab"`, "preview-impact", "DW.confirm"} {
		if !strings.Contains(js, want) {
			t.Errorf("rule-preview.js lacks %s", want)
		}
	}
	if strings.Contains(js, "innerHTML") {
		t.Error("rule-preview.js must not use innerHTML")
	}
	css := readContent(t, "static/style.css")
	if !strings.Contains(css, ".rp-cards") || !strings.Contains(css, "max-width: 900px") {
		t.Error("style.css lacks the preview card layout")
	}
}
