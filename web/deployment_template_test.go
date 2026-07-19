package web

import (
	"strings"
	"testing"
)

func TestDeploymentDocsMatchUserspaceTailscaleProfile(t *testing.T) {
	body, err := Content.ReadFile("templates/docs.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, want := range []string{"http://${TS_HOSTNAME}-admin/admin/", "http://${TS_HOSTNAME}-gw/", "userspace Tailscale", "listens only on loopback", "Settings -> Gateway URL", "without certificate-domain or HTTP Host matching"} {
		if !strings.Contains(page, want) {
			t.Errorf("deployment docs missing %q", want)
		}
	}
	for _, obsolete := range []string{"https://duckway-admin.tailnet", "Tailscale terminates HTTPS", "docker-compose.prod.yml", "docker compose up -d", "kernel-mode Tailscale"} {
		if strings.Contains(page, obsolete) {
			t.Errorf("deployment docs still contain obsolete text %q", obsolete)
		}
	}
}
