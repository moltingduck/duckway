package web

import (
	"strings"
	"testing"
)

func TestHardenFrontendFixes(t *testing.T) {
	for path, wants := range map[string][]string{
		"templates/cc.html":            {"addEventListener('hashchange'", "function openHashCC"},
		"templates/services.html":      {"usage_references", `id="p-locked"`, "el('p-model').disabled = locked"},
		"templates/settings.html":      {"Every other signed-in session is signed out"},
		"templates/client_detail.html": {"canary_remote_failed", "dw-canary-remote-failed"},
		"templates/clients.html":       {`id="cl-flash"`, "dw-canary-remote-failed", "canarytokens.org"},
		"static/style.css":             {".pt-rowactions .btn { white-space: nowrap; }", ".pt-table td.pt-c-actions { min-width: 11rem; }"},
	} {
		src := readWeb(t, path)
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Errorf("%s missing %q", path, w)
			}
		}
	}
	if strings.Contains(readWeb(t, "templates/settings.html"), "are <strong>not</strong> signed out") {
		t.Error("settings still claims other sessions stay signed in")
	}
}
