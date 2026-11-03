package web

import (
	"regexp"
	"strings"
	"testing"
)

// Generic CSP checks (handlers, innerHTML, hx-on, eval, nonces) live in csp_ratchet_test.go.

func TestCredentialsTemplatesEscapeServerValuesInMarkup(t *testing.T) {
	read := func(name string) string {
		b, err := Content.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	svc := read("templates/services.html")
	if !strings.Contains(svc, "<strong>${t.name}</strong>") || !strings.Contains(svc, "${t.description}") {
		t.Error("services ACL template cards must interpolate name/description through DW.html")
	}
	kp := read("templates/key_pools.html")
	if !strings.Contains(kp, "<strong>${m.name}</strong>") || !strings.Contains(kp, "<strong>${p.name}</strong>") {
		t.Error("key_pools must interpolate pool and member names through DW.html")
	}
	kb := read("templates/key_bundles.html")
	if !strings.Contains(kb, "<h2 id=\"kb-h-${b.id}\">${d.name}</h2>") {
		t.Error("key_bundles must interpolate bundle names through DW.html")
	}
	// Server values must travel through DW.html/data-* attributes, never JS string literals.
	for _, name := range []string{"templates/key_pools.html", "templates/key_bundles.html"} {
		src := read(name)
		if regexp.MustCompile(`'\{\{\.(Name|Description)\}\}'`).MatchString(src) {
			t.Errorf("%s interpolates server text into a JS string literal", name)
		}
		if regexp.MustCompile(`(^|[^A-Za-z0-9_.])(alert|confirm|prompt)\(`).MatchString(src) {
			t.Errorf("%s must not call alert/confirm/prompt", name)
		}
		for _, banned := range []string{"hx-confirm", "location.reload", "style=\""} {
			if strings.Contains(src, banned) {
				t.Errorf("%s must not use %s", name, banned)
			}
		}
	}
}
