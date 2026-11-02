package web

import (
	"regexp"
	"strings"
	"testing"
)

var credentialsTemplates = []string{
	"templates/services.html",
	"templates/api_keys.html",
	"templates/key_groups.html",
	"templates/key_group_detail.html",
	"templates/key_suites.html",
	"templates/groups.html",
	"templates/usage.html",
	"templates/oauth.html",
	"templates/placeholders.html",
}

var credentialsStatics = []string{
	"static/acl-preview.js",
	"static/usage-snapshot.js",
}

var (
	credInlineHandlerRE = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	credHxOnRE          = regexp.MustCompile(`(?i)\bhx-on[:\w-]*\s*=`)
	credInnerHTMLRE     = regexp.MustCompile(`\.innerHTML\b`)
	credScriptTagRE     = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	credDynamicCodeRE   = regexp.MustCompile(`javascript:|\beval\(|new Function\(|setTimeout\(\s*['"` + "`" + `]`)
)

func TestCredentialsTemplatesAreCSPClean(t *testing.T) {
	for _, name := range append(append([]string{}, credentialsTemplates...), credentialsStatics...) {
		b, err := Content.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if m := credInlineHandlerRE.FindString(src); m != "" {
			t.Errorf("%s contains inline event handler %q", name, m)
		}
		if m := credHxOnRE.FindString(src); m != "" {
			t.Errorf("%s contains hx-on attribute %q", name, m)
		}
		if credInnerHTMLRE.MatchString(src) {
			t.Errorf("%s writes .innerHTML (use DW.setHTML / textContent)", name)
		}
		if m := credDynamicCodeRE.FindString(src); m != "" {
			t.Errorf("%s contains dynamic code construct %q", name, m)
		}
		if strings.HasSuffix(name, ".html") {
			for _, tag := range credScriptTagRE.FindAllString(src, -1) {
				if !strings.Contains(tag, `nonce="{{.CSPNonce}}"`) {
					t.Errorf("%s script tag lacks nonce: %s", name, tag)
				}
			}
		}
	}
}

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
