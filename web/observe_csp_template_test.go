package web

import (
	"regexp"
	"strings"
	"testing"
)

var observeOnAttrRE = regexp.MustCompile(`(?i)[\s"'(]on[a-z]+\s*=`)
var observeScriptTagRE = regexp.MustCompile(`(?i)<script\b[^>]*>`)

func TestObserveTemplatesCSPClean(t *testing.T) {
	files := []string{
		"templates/logs.html", "templates/canary.html", "templates/supplychain.html",
		"templates/settings.html", "templates/docs.html", "static/search.js",
	}
	for _, name := range files {
		b, err := Content.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if m := observeOnAttrRE.FindString(src); m != "" {
			t.Errorf("%s contains inline handler %q", name, m)
		}
		if strings.Contains(src, "hx-on") {
			t.Errorf("%s contains hx-on attribute", name)
		}
		if strings.Contains(src, ".innerHTML") {
			t.Errorf("%s uses .innerHTML", name)
		}
		if strings.Contains(strings.ToLower(src), "javascript:") {
			t.Errorf("%s contains javascript: URL", name)
		}
		if strings.HasSuffix(name, ".html") {
			for _, tag := range observeScriptTagRE.FindAllString(src, -1) {
				if !strings.Contains(tag, `nonce="{{.CSPNonce}}"`) {
					t.Errorf("%s script tag lacks nonce: %s", name, tag)
				}
			}
		}
	}
}
