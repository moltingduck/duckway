package web

import (
	"strings"
	"testing"
)

func TestCredentialsTemplateKeepsRefreshableContracts(t *testing.T) {
	body, err := Content.ReadFile("templates/credentials.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	required := []string{
		`/api/oauth/upload`,
		`/api/oauth/validate`,
		`'/validate'`,
		`/refresh'`,
		`data-action="refreshNow"`,
		`data-action="openToken"`,
		`Upload new token`,
		`body.is_active = true;`,
		`subscription_info`,
		`tok.sub.credential_kind = 'codex_oauth';`,
		`tok.sub.source = 'codex';`,
		`type="date"`,
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Fatalf("credentials template missing refreshable contract: %s", want)
		}
	}
	if strings.Contains(html, "Unix ms") {
		t.Fatal("expiry must be entered with a date input, not Unix milliseconds")
	}
	if strings.Contains(html, "Reveal") {
		t.Fatal("stored secrets must never be revealed")
	}
}

func TestCredentialsTemplateHasNoNativeDialogs(t *testing.T) {
	body, err := Content.ReadFile("templates/credentials.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"alert(", "confirm(", "prompt(", " style=", "location.reload"} {
		if strings.Contains(string(body), bad) {
			t.Errorf("credentials template must not contain %q", bad)
		}
	}
}
