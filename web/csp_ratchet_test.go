package web

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

var (
	// inline handler attribute or JS string/property assignment; data-on-* excluded by the leading boundary.
	ratchetHandlerRE = regexp.MustCompile(`(?i)(?:^|[\s"'<;(,{])on[a-z]{3,}\s*=\s*["'\\]`)
	ratchetPropRE    = regexp.MustCompile(`(?i)\.on(?:click|change|input|submit|keydown|keyup|keypress|load|error|focus|blur|mouse[a-z]+|dblclick)\s*=`)
	ratchetHxOnRE    = regexp.MustCompile(`(?i)\bhx-on\b|\bhx-on[:\-:]|\bdata-hx-on`)
	ratchetInnerRE   = regexp.MustCompile(`\.innerHTML\b|\.outerHTML\s*=|insertAdjacentHTML`)
	ratchetJSURLRE   = regexp.MustCompile(`(?i)\b(?:href|src|action|formaction)\s*=\s*["']?\s*javascript\s*:`)
	// DW.raw( must take a plain string literal: no ${} template, identifier or concatenation.
	ratchetRawCallRE = regexp.MustCompile(`DW\.raw\(`)
	ratchetRawOKRE   = regexp.MustCompile(`^DW\.raw\(\s*(?:'(?:[^'\\\n]|\\.)*'|"(?:[^"\\\n]|\\.)*")(?:\s*\+\s*(?:'(?:[^'\\\n]|\\.)*'|"(?:[^"\\\n]|\\.)*"))*\s*\)`)
	ratchetEvalRE    = regexp.MustCompile(`\beval\s*\(|\bnew\s+Function\b|\bFunction\s*\(|setTimeout\s*\(\s*["'` + "`" + `]|setInterval\s*\(\s*["'` + "`" + `]`)
	ratchetScriptRE  = regexp.MustCompile(`(?i)<script\b[^>]*>`)
)

// ratchetTargets returns every template and static JS file subject to the CSP ratchet.
func ratchetTargets(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, glob := range []string{"templates/*.html", "static/*.js"} {
		names, err := fs.Glob(Content, glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if path.Base(n) == "htmx.min.js" {
				continue
			}
			b, err := fs.ReadFile(Content, n)
			if err != nil {
				t.Fatal(err)
			}
			out[n] = string(b)
		}
	}
	if len(out) < 20 {
		t.Fatalf("ratchet found only %d files; embed glob broken?", len(out))
	}
	return out
}

func TestCSPRatchetNoInlineHandlersOrUnsafeSinks(t *testing.T) {
	for name, src := range ratchetTargets(t) {
		for i, line := range strings.Split(src, "\n") {
			where := func(what string) { t.Errorf("%s:%d: %s: %s", name, i+1, what, strings.TrimSpace(line)) }
			if ratchetHandlerRE.MatchString(line) {
				where("inline on* handler")
			}
			if ratchetPropRE.MatchString(line) {
				where("on* property handler (use addEventListener)")
			}
			if ratchetHxOnRE.MatchString(line) {
				where("hx-on usage")
			}
			if ratchetJSURLRE.MatchString(line) {
				where("javascript: URL")
			}
			if ratchetEvalRE.MatchString(line) {
				where("eval / new Function / string timer")
			}
			if ratchetInnerRE.MatchString(line) {
				// the single sanctioned write is DW.setHTML in dw.js
				if name == "static/dw.js" && strings.Contains(line, "el.innerHTML = safe.__html;") {
					continue
				}
				where("raw HTML sink outside DW.setHTML")
			}
		}
	}
}

func TestCSPRatchetSingleInnerHTMLWrite(t *testing.T) {
	n := 0
	for name, src := range ratchetTargets(t) {
		if name == "static/dw.js" {
			n += strings.Count(src, "el.innerHTML = safe.__html;")
		}
	}
	if n != 1 {
		t.Fatalf("dw.js must contain exactly one sanctioned innerHTML write, got %d", n)
	}
}

func TestCSPRatchetEveryScriptTagHasNonce(t *testing.T) {
	for name, src := range ratchetTargets(t) {
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		for _, tag := range ratchetScriptRE.FindAllString(src, -1) {
			if !strings.Contains(tag, `nonce="{{$.CSPNonce}}"`) {
				t.Errorf("%s: script tag without nonce: %s", name, tag)
			}
		}
	}
}

func TestCSPRatchetHTMXConfigDisablesEvalAndScripts(t *testing.T) {
	b, err := fs.ReadFile(Content, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`name="htmx-config"`, `"allowEval":false`, `"allowScriptTags":false`} {
		if !strings.Contains(s, want) {
			t.Errorf("layout.html missing %s", want)
		}
	}
	if strings.Index(s, `name="htmx-config"`) > strings.Index(s, "htmx.min.js") {
		t.Error("htmx-config meta must precede htmx.min.js")
	}
}

func TestCSPRatchetPatternsDetectViolations(t *testing.T) {
	bad := map[*regexp.Regexp][]string{
		ratchetHandlerRE: {`<button onclick="x()">`, `'<a onchange=\"f\">'`, `<input\n oninput='y'>`},
		ratchetPropRE:    {`btn.onclick = f;`},
		ratchetHxOnRE:    {`<div hx-on:click="x">`, `<div hx-on::after-request="x">`},
		ratchetInnerRE:   {`el.innerHTML = x`, `el.outerHTML = x`, `el.insertAdjacentHTML('beforeend', x)`},
		ratchetJSURLRE:   {`<a href="javascript:void(0)">`, `<form action=javascript:x()>`, `<img src='javascript:x'>`},
		ratchetEvalRE:    {`eval(x)`, `new Function("a")`},
	}
	for re, samples := range bad {
		for _, s := range samples {
			if !re.MatchString(s) {
				t.Errorf("pattern %v failed to flag %q", re, s)
			}
		}
	}
	for _, ok := range []string{`<div data-on-click="x">`, `data-onchange="x"`, `<button data-action="go">`} {
		if ratchetHandlerRE.MatchString(ok) {
			t.Errorf("handler pattern flagged benign %q", ok)
		}
	}
}

func TestCSPRatchetDWRawTakesOnlyStringLiterals(t *testing.T) {
	for name, src := range ratchetTargets(t) {
		for _, loc := range ratchetRawCallRE.FindAllStringIndex(src, -1) {
			if !ratchetRawOKRE.MatchString(src[loc[0]:]) {
				line := 1 + strings.Count(src[:loc[0]], "\n")
				end := loc[0] + 80
				if end > len(src) {
					end = len(src)
				}
				t.Errorf("%s:%d: DW.raw argument must be string literal(s) only: %s", name, line, strings.SplitN(src[loc[0]:end], "\n", 2)[0])
			}
		}
	}
}

func TestCSPRatchetRawPatternSamples(t *testing.T) {
	for _, ok := range []string{`DW.raw('<b>x</b>')`, `DW.raw("a" + 'b')`, "DW.raw('x' +\n  'y')"} {
		if !ratchetRawOKRE.MatchString(ok) {
			t.Errorf("literal flagged: %q", ok)
		}
	}
	for _, bad := range []string{"DW.raw(`<b>${x}</b>`)", `DW.raw(name)`, `DW.raw('a' + name)`, `DW.raw(x + 'a')`} {
		if ratchetRawOKRE.MatchString(bad) {
			t.Errorf("non-literal accepted: %q", bad)
		}
	}
}

func TestCSPRatchetFleetTemplatesStillCarryScripts(t *testing.T) {
	for _, name := range []string{"clients.html", "client_detail.html", "client_updates.html", "onboarding.html", "cc.html", "approvals.html", "notifications.html", "credentials.html"} {
		b, err := Content.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !ratchetScriptRE.Match(b) {
			t.Errorf("%s has no script tag; ratchet coverage list is stale", name)
		}
	}
}
