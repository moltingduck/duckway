package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

func TestPluginsPageExplainsServerAndClientLifecycle(t *testing.T) {
	layout, err := Content.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	page, err := Content.ReadFile("templates/plugins.html")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("layout").Parse(string(layout) + string(page))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = tmpl.ExecuteTemplate(&output, "layout", struct {
		Title, Active, CSPNonce, WhatsNewVersion string
		PendingApprovals                         int
		WhatsNewShow                             bool
		WhatsNewMoves                            []struct{ Old, New, Path string }
	}{Title: `<script>alert("title")</script>`, Active: "plugins", CSPNonce: "testnonce"})
	if err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	if strings.Contains(rendered, `<script>alert("title")</script>`) || !strings.Contains(rendered, "&lt;script&gt;") {
		t.Fatal("page title was not escaped")
	}
	for _, want := range []string{
		"duckway plugin install NAME", "No plugin sources have been registered.",
		"does not stop or remove plugins already installed", "Client versions appear only after clients report them",
		"verifies its size and SHA-256", "do not assess whether the code is safe",
		`<script nonce="testnonce" src="/static/plugins.js" defer></script>`,
		`for="plugin-name"`, `for="plugin-manifest-url"`, `aria-describedby="plugin-import-help"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("plugin page missing lifecycle or accessible form content %q", want)
		}
	}
}
