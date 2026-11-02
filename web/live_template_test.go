package web

import (
	"io/fs"
	"strings"
	"testing"
)

func readWeb(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(Content, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLiveUpdatesWiring(t *testing.T) {
	layout := readWeb(t, "templates/layout.html")
	if !strings.Contains(layout, `nonce="{{$.CSPNonce}}" src="/static/live.js"`) {
		t.Error("layout must load live.js with the CSP nonce")
	}
	if strings.Index(layout, "/static/live.js") < strings.Index(layout, "/static/dw.js") {
		t.Error("live.js must load after dw.js")
	}
	if !strings.Contains(layout, `id="live-status"`) || !strings.Contains(layout, `aria-live="polite"`) {
		t.Error("layout needs the aria-live status slot")
	}
	for page, topic := range map[string]string{
		"approvals.html": "'approvals'", "logs.html": "'logs'",
		"client_updates.html": "'rollouts'", "dashboard.html": "'logs'",
	} {
		if !strings.Contains(readWeb(t, "templates/"+page), "DW.live.on("+topic) {
			t.Errorf("%s does not subscribe to %s", page, topic)
		}
	}
	if strings.Contains(readWeb(t, "templates/client_updates.html"), "setInterval") {
		t.Error("client_updates must use live updates, not setInterval")
	}
	if !strings.Contains(readWeb(t, "templates/logs.html"), `id="live-tail"`) {
		t.Error("logs needs the Live tail toggle")
	}
	js := readWeb(t, "static/live.js")
	for _, want := range []string{"/api/events", "EventSource", "FAIL_LIMIT", "POLL_MS = 15000", "SSE_RETRY_POLLING_MS = 60000"} {
		if !strings.Contains(js, want) {
			t.Errorf("live.js missing %q", want)
		}
	}
}
