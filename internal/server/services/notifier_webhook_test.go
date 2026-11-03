package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestWebhookTestAndRealSendBothCarrySecretHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Duckway-Secret"))
	}))
	defer srv.Close()
	n := NewNotifier(nil)

	withSecret, _ := json.Marshal(map[string]string{"url": srv.URL, "secret": "s3cret"})
	if res := n.TestChannel(queries.NotificationChannel{ChannelType: "webhook", Config: string(withSecret)}); !res.SendOK {
		t.Fatalf("test send failed: %+v", res)
	}
	if err := n.sendWebhook(string(withSecret), ApprovalNotification{ApprovalID: "a1"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "s3cret" || got[1] != "s3cret" {
		t.Fatalf("secret header on test and real send = %q", got)
	}

	got = nil
	noSecret, _ := json.Marshal(map[string]string{"url": srv.URL})
	n.TestChannel(queries.NotificationChannel{ChannelType: "webhook", Config: string(noSecret)})
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("header must be absent without a secret: %q", got)
	}
}

func TestWebhookTestReportsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL, "secret": "x"})
	res := NewNotifier(nil).TestChannel(queries.NotificationChannel{ChannelType: "webhook", Config: string(cfg)})
	if res.SendOK || res.Message != "webhook returned 403" {
		t.Fatalf("%+v", res)
	}
}
