package web

import (
	"regexp"
	"strings"
	"testing"
)

var fleetCSPTemplates = []string{
	"clients.html",
	"client_updates.html",
	"onboarding.html",
	"cc.html",
	"approvals.html",
	"notifications.html",
}

// fleetInlineHandlerRE matches inline event-handler attributes such as
// ` onclick=` or ` onchange =` but not data-on-change, hx-on or words like "button".
var fleetInlineHandlerRE = regexp.MustCompile(`(?i)[\s"'<]on[a-z]+\s*=`)

func TestFleetTemplatesAreCSPClean(t *testing.T) {
	scriptTag := regexp.MustCompile(`(?i)<script\b[^>]*>`)
	for _, name := range fleetCSPTemplates {
		b, err := Content.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if m := fleetInlineHandlerRE.FindString(src); m != "" {
			t.Errorf("%s contains inline handler %q", name, m)
		}
		if strings.Contains(src, ".innerHTML") {
			t.Errorf("%s uses .innerHTML (use DW.setHTML / textContent / replaceChildren)", name)
		}
		if regexp.MustCompile(`(?i)\bhx-on`).MatchString(src) {
			t.Errorf("%s contains hx-on attributes", name)
		}
		if regexp.MustCompile(`(?i)javascript:|\beval\(|new Function\(`).MatchString(src) {
			t.Errorf("%s contains javascript: URL, eval or new Function", name)
		}
		tags := scriptTag.FindAllString(src, -1)
		for _, tag := range tags {
			if !strings.Contains(tag, `nonce="{{.CSPNonce}}"`) {
				t.Errorf("%s script tag lacks nonce: %s", name, tag)
			}
		}
		if len(tags) == 0 {
			t.Errorf("%s has no script tag; test list is stale", name)
		}
	}
}
