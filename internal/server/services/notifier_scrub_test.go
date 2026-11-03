package services

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestScrubNetErrDropsURLWithBotToken(t *testing.T) {
	const tok = "123456:AAH-secret-telegram-token"
	c := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })}
	_, err := c.Post("https://api.telegram.org/bot"+tok+"/sendMessage", "application/json", strings.NewReader("{}"))
	if err == nil || !strings.Contains(err.Error(), tok) {
		t.Fatalf("precondition: raw error should echo the token, got %v", err)
	}
	got := scrubNetErr(err).Error()
	if strings.Contains(got, tok) || strings.Contains(got, "api.telegram.org") {
		t.Fatalf("scrubbed error still leaks: %s", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("scrubbed error lost cause: %s", got)
	}
	if scrubNetErr(errors.New("x /bot999:abc/getUpdates y")).Error() != "x /bot********/getUpdates y" {
		t.Fatal("bot path not redacted")
	}
	if scrubNetErr(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}
