package handlers

import (
	"strings"
	"testing"
)

func TestRedactCapturedBodyJSON(t *testing.T) {
	body := `{"token":"ghs_real","private_key":"-----BEGIN PRIVATE KEY-----","nested":{"authorization":"Bearer secret","message":"keep"}}`
	got := redactCapturedBody(body, "application/json")
	for _, secret := range []string{"ghs_real", "BEGIN PRIVATE KEY", "Bearer secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted JSON leaked %q in %s", secret, got)
		}
	}
	if !strings.Contains(got, `"message":"keep"`) {
		t.Fatalf("redacted JSON lost normal payload: %s", got)
	}
}

func TestRedactCapturedBodyForm(t *testing.T) {
	got := redactCapturedBody("access_token=ghs_real&client_secret=supersecretvalue&message=keep", "application/x-www-form-urlencoded")
	for _, secret := range []string{"ghs_real", "supersecretvalue"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted form leaked %q in %s", secret, got)
		}
	}
	if !strings.Contains(got, "message=keep") {
		t.Fatalf("redacted form lost normal payload: %s", got)
	}
}
