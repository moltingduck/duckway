package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type testCrumb struct {
	Label, Href string
	Current     bool
}

type testShell struct {
	Crumbs                                   []testCrumb
	AdminUser, Initial, GatewayHost, Version string
}

type testPage struct {
	Title, Active, CSPNonce, WhatsNewVersion string
	PendingApprovals                         int
	WhatsNewShow                             bool
	WhatsNewMoves                            []struct{ Old, New, Path string }
	Shell                                    testShell
}

func renderLayout(t *testing.T, d testPage) string {
	t.Helper()
	layout := readWeb(t, "templates/layout.html")
	tmpl, err := template.New("layout").Parse(layout + `{{define "content"}}x{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.ExecuteTemplate(&out, "layout", d); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestLayoutBreadcrumbBrandAndUserChip(t *testing.T) {
	out := renderLayout(t, testPage{Title: "NAME", Active: "placeholders", CSPNonce: "n", Shell: testShell{
		Crumbs:      []testCrumb{{Label: "Access"}, {Label: "Phantom tokens", Href: "/admin/phantom-tokens"}, {Label: "NAME", Current: true}},
		AdminUser:   "alice",
		Initial:     "A",
		GatewayHost: "gw.example.test",
		Version:     "abc1234",
	}})
	for _, want := range []string{
		`<nav class="crumbs" aria-label="Breadcrumb">`,
		`<li>Access</li>`,
		`<li><a href="/admin/phantom-tokens">Phantom tokens</a></li>`,
		`<li class="crumb-current" aria-current="page">NAME</li>`,
		`gw.example.test &middot; abc1234`,
		`class="user-name">alice</span>`,
		`class="user-avatar" aria-hidden="true">A</span>`,
		`data-action="logout">Log out</button>`,
		`data-action="setTheme" data-theme-choice="dark"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("layout missing %q", want)
		}
	}
	if strings.Contains(out, "API Proxy Manager") {
		t.Error("static brand sub-line must be replaced by gateway host and version")
	}
}

func TestLayoutBreadcrumbEscapesLabels(t *testing.T) {
	out := renderLayout(t, testPage{Title: "x", CSPNonce: "n", Shell: testShell{Crumbs: []testCrumb{{Label: `<img src=x>`, Current: true}}}})
	if strings.Contains(out, "<img src=x>") {
		t.Error("crumb label must be escaped")
	}
}

func TestLoginShowPasswordToggle(t *testing.T) {
	tmpl, err := template.New("login.html").Parse(readWeb(t, "templates/login.html"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, struct {
		Error, CSPNonce string
	}{Error: "Invalid credentials", CSPNonce: "n"}); err != nil {
		t.Fatal(err)
	}
	h := out.String()
	for _, want := range []string{
		`id="pw-toggle" aria-pressed="false" aria-controls="password">Show password</button>`,
		`aria-describedby="login-error"`, `aria-invalid="true"`, `role="alert"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("login missing %q", want)
		}
	}
	js := readWeb(t, "static/login.js")
	for _, want := range []string{"Hide password", "Show password", "aria-pressed", "input.type"} {
		if !strings.Contains(js, want) {
			t.Errorf("login.js missing %q", want)
		}
	}
}

func TestNoNativeDialogsInStaticJS(t *testing.T) {
	entries, err := fs.ReadDir(Content, "static")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		js := readWeb(t, "static/"+e.Name())
		for _, bad := range []string{"window.confirm", "window.alert", "window.prompt"} {
			if strings.Contains(js, bad) {
				t.Errorf("%s uses %s", e.Name(), bad)
			}
		}
	}
	if !strings.Contains(readWeb(t, "static/plugins.js"), "DW.confirm(") {
		t.Error("plugins.js must use the in-page DW.confirm")
	}
}

func TestGlossaryDefinitionsAreExact(t *testing.T) {
	js := readWeb(t, "static/dw.js")
	for _, want := range []string{
		"'credential'", "'phantom-token'", "'key-pool'", "'key-bundle'", "'control-channel'", "'acl-layers'",
		"A real upstream secret stored encrypted on the server: static API key, refreshable OAuth token, or GitHub App.",
		"The stand-in a client sees. The gateway swaps it for the real credential and enforces its ACL and approval rule.",
		"Several credentials of one service rotated by a strategy: score, round-robin, failover or random.",
		"One credential or pool per service. Assigning a bundle to a client creates its phantom tokens in one step.",
		"A Discord bot + category bound to one client so its agent can be driven from chat. The bot token never leaves the server.",
		"Service \\u2192 credential \\u2192 phantom token. Each layer can only narrow what the one above allows.",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dw.js missing definition %q", want)
		}
	}
}

func TestFontsSelfHostedAndTokens(t *testing.T) {
	css := readWeb(t, "static/style.css")
	for _, f := range []string{"IBMPlexSans-400", "IBMPlexSans-500", "IBMPlexSans-600", "IBMPlexSans-700", "IBMPlexMono-400", "IBMPlexMono-500"} {
		if !strings.Contains(css, "/static/fonts/"+f+".woff2") {
			t.Errorf("style.css missing @font-face for %s", f)
		}
		b, err := fs.ReadFile(Content, "static/fonts/"+f+".woff2")
		if err != nil || len(b) < 1000 || string(b[:4]) != "wOF2" {
			t.Errorf("%s is not an embedded woff2 file (err=%v)", f, err)
		}
	}
	if n := strings.Count(css, "font-display: swap"); n != 6 {
		t.Errorf("font-display: swap count = %d, want 6", n)
	}
	if !strings.Contains(readWeb(t, "static/fonts/OFL.txt"), "SIL OPEN FONT LICENSE") {
		t.Error("OFL licence text missing")
	}
	if strings.Contains(css, "fonts.googleapis") || regexp.MustCompile(`url\(['"]?https?:`).MatchString(css) {
		t.Error("no external font/CSS loads")
	}
	for _, want := range []string{"--r-control: 7px", "--r-card: 10px", "--r-dialog: 14px", "--s-1: 4px", "--s-2: 8px", "--s-3: 12px", "--s-4: 16px", "--s-5: 24px", "--s-6: 32px", "--s-7: 48px"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing token %q", want)
		}
	}
	if n := strings.Count(css, "--raised: #FFFFFF;"); n != 2 {
		t.Errorf("light Raised must be #FFFFFF in both light blocks, found %d", n)
	}
}

func TestNoFontSizeBelow12px(t *testing.T) {
	re := regexp.MustCompile(`font-size:\s*(\d*\.?\d+)(rem|em|px)\b`)
	check := func(name, src string) {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			v, _ := strconv.ParseFloat(m[1], 64)
			px := v
			if m[2] != "px" {
				px = v * 16
			}
			if px < 12 {
				t.Errorf("%s: %s is below 12px", name, m[0])
			}
		}
	}
	check("style.css", readWeb(t, "static/style.css"))
	entries, _ := fs.ReadDir(Content, "templates")
	for _, e := range entries {
		check(e.Name(), readWeb(t, "templates/"+e.Name()))
	}
}
