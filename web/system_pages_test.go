package web

import (
	"regexp"
	"strings"
	"testing"
)

// The system pages (Notifications, Settings, Docs) carry scripts, no inline
// styles or handlers, and never use blocking browser dialogs.
func TestSystemPagesCSPAndDialogRules(t *testing.T) {
	dialog := regexp.MustCompile(`\b(?:alert|confirm|prompt)\s*\(`)
	for _, name := range []string{"notifications.html", "settings.html", "docs.html"} {
		b, err := Content.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !ratchetScriptRE.MatchString(src) {
			t.Errorf("%s has no script tag", name)
		}
		if strings.Contains(src, ` style=`) {
			t.Errorf("%s has an inline style attribute", name)
		}
		if strings.Contains(src, "hx-confirm") || dialog.MatchString(src) {
			t.Errorf("%s uses a blocking dialog", name)
		}
		for _, m := range ratchetScriptRE.FindAllString(src, -1) {
			if !strings.Contains(m, `nonce="{{$.CSPNonce}}"`) {
				t.Errorf("%s: script without nonce: %s", name, m)
			}
		}
	}
}
