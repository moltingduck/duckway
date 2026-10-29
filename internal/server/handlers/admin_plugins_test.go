package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/web"
)

func TestAdminPluginsRendersEmbeddedAndReloadedPage(t *testing.T) {
	for _, reload := range []bool{false, true} {
		name := "embedded"
		if reload {
			name = "live reload"
		}
		t.Run(name, func(t *testing.T) {
			h := NewAdminHandler(web.Content, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			h.liveReload = reload
			recorder := httptest.NewRecorder()
			h.Plugins(recorder, httptest.NewRequest(http.MethodGet, "/admin/plugins", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("Plugins returned %d: %s", recorder.Code, recorder.Body.String())
			}
			page := recorder.Body.String()
			for _, want := range []string{
				`<title>Plugins — Duckway</title>`,
				`href="/admin/plugins" class="active" aria-current="page"`,
				`<script src="/static/plugins.js" defer></script>`,
				`id="plugin-import-form"`, `Register and check`,
				`id="plugins-error" class="card" role="alert" hidden`,
				`id="plugins-status" class="mb-1 text-sm" role="status" aria-live="polite"`,
			} {
				if !strings.Contains(page, want) {
					t.Errorf("rendered page missing %q", want)
				}
			}
		})
	}
}
