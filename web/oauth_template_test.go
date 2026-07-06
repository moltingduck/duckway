package web

import (
	"strings"
	"testing"
)

func TestOAuthTemplateShowsAndRepairsActiveStatus(t *testing.T) {
	body, err := Content.ReadFile("templates/oauth.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`<tr><td class="text-muted">Status</td><td id="d-status"></td></tr>`,
		`document.getElementById('d-status').innerHTML = data.is_active ? '<span class="badge badge-green">Active</span>' : '<span class="badge badge-red">Inactive</span>';`,
		`if (parsed.accessToken && parsed.refreshToken) document.getElementById('edit-active').checked = true;`,
		`if (accessToken && refreshToken) body.is_active = true;`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("oauth template missing active-status contract: %s", want)
		}
	}
}
