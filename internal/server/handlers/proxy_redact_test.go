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

func TestRedactCapturedBodyPlainText(t *testing.T) {
	body := strings.Join([]string{
		"error included sk-1234567890abcdef",
		"github_pat_1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJK",
		"ghs_1234567890abcdefghijklmnopqrstuvwxyz",
		"rt.1234567890abcdefghijklmnopqrstuvwxyz",
		"eyJhbGciOiJub25lIn0.eyJzdWIiOiJ0ZXN0LXVzZXIifQ.signaturevalue",
		"-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----",
		"normal message",
	}, "\n")
	got := redactCapturedBody(body, "text/plain")
	for _, secret := range []string{"sk-1234567890abcdef", "github_pat_", "ghs_", "rt.", "eyJhbGci", "BEGIN PRIVATE KEY", "secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted text leaked %q in %s", secret, got)
		}
	}
	if !strings.Contains(got, "normal message") {
		t.Fatalf("redacted text lost normal payload: %s", got)
	}
}
