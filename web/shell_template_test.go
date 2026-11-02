package web

import (
	"regexp"
	"strings"
	"testing"
)

var inlineHandlerRE = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)

func TestShellTemplatesHaveNoInlineHandlersAndNonceEveryScript(t *testing.T) {
	scriptTag := regexp.MustCompile(`(?i)<script\b[^>]*>`)
	for _, name := range []string{"layout.html", "login.html", "dashboard.html"} {
		b, err := Content.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if m := inlineHandlerRE.FindString(src); m != "" {
			t.Errorf("%s contains inline handler %q", name, m)
		}
		for _, tag := range scriptTag.FindAllString(src, -1) {
			if !strings.Contains(tag, `nonce="{{.CSPNonce}}"`) {
				t.Errorf("%s script tag lacks nonce: %s", name, tag)
			}
		}
	}
}

func TestShellStaticAssetsHaveNoInlineHandlerMarkup(t *testing.T) {
	for _, name := range []string{"dw.js", "shell.js", "login.js"} {
		b, err := Content.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if inlineHandlerRE.MatchString(strings.ReplaceAll(string(b), "\n", " ")) && strings.Contains(string(b), `onclick=`) {
			t.Errorf("%s builds inline handler markup", name)
		}
		if name != "dw.js" && strings.Contains(string(b), "innerHTML") {
			t.Errorf("%s uses innerHTML", name)
		}
	}
}
