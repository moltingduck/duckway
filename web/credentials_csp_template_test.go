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
	kg := read("templates/key_groups.html")
	if !strings.Contains(kg, "<strong>${m.key_name}</strong>") {
		t.Error("key_groups member table must interpolate key_name through DW.html")
	}
	// Server values must travel through data-* attributes, never JS string literals.
	for _, name := range []string{"templates/key_groups.html", "templates/key_suites.html"} {
		src := read(name)
		if regexp.MustCompile(`'\{\{\.(Name|Description)\}\}'`).MatchString(src) {
			t.Errorf("%s interpolates server text into a JS string literal", name)
		}
		if !strings.Contains(src, `data-name="{{.Name}}"`) {
			t.Errorf("%s should pass Name via data-name", name)
		}
	}
}
