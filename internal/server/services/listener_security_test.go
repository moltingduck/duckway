package services

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestTelegramPollerNeverLogsBotToken(t *testing.T) {
	const tok = "123456:AAH-poller-secret-token"
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	p := NewTelegramPoller(tok, "42", nil, nil)
	p.client = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })}
	if _, err := p.getUpdates(); err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("getUpdates err=%v must be non-nil and token-free", err)
	}
	p.answerCallback("cb1", "x")
	p.editMessage(1, 2, "x")
	// the poll loop's own log line
	log.Printf("[telegram-poll] Error: %v", scrubNetErr(&url_error_for_test{tok}))
	out := buf.String()
	if strings.Contains(out, tok) {
		t.Fatalf("log leaked the bot token: %s", out)
	}
	if !strings.Contains(out, "answerCallbackQuery failed") || !strings.Contains(out, "editMessageText failed") {
		t.Fatalf("helper failures should be logged (scrubbed): %s", out)
	}
}

type url_error_for_test struct{ tok string }

func (e *url_error_for_test) Error() string {
	return `Post "https://api.telegram.org/bot` + e.tok + `/getUpdates": dial tcp: refused`
}

func TestGuardChannelActionChecksDBAtCallTime(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := queries.NewNotificationQueries(db)
	cfg := `{"bot_token":"t","chat_id":"1"}`
	if err := q.Create(&queries.NotificationChannel{ID: "ch1", ChannelType: "telegram", Name: "n", Config: cfg, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g := GuardChannelAction(q, "ch1", cfg, func(string) error { calls++; return nil })
	if err := g("a1"); err != nil || calls != 1 {
		t.Fatalf("active+matching must run: err=%v calls=%d", err, calls)
	}
	if err := q.Update("ch1", false); err != nil {
		t.Fatal(err)
	}
	if err := g("a1"); !errors.Is(err, ErrChannelNotAuthoritative) || calls != 1 {
		t.Fatalf("disabled channel must be refused: err=%v calls=%d", err, calls)
	}
	_ = q.Update("ch1", true)
	if err := g("a1"); err != nil || calls != 2 {
		t.Fatalf("re-enabled channel works again (no caching): err=%v", err)
	}
	if err := q.UpdateFull("ch1", "n", `{"bot_token":"other","chat_id":"1"}`); err != nil {
		t.Fatal(err)
	}
	if err := g("a1"); !errors.Is(err, ErrChannelNotAuthoritative) || calls != 2 {
		t.Fatalf("edited config must be refused: err=%v", err)
	}
	if err := q.Delete("ch1"); err != nil {
		t.Fatal(err)
	}
	if err := g("a1"); !errors.Is(err, ErrChannelNotAuthoritative) {
		t.Fatalf("deleted channel must be refused: %v", err)
	}
}

func TestGuardChannelActionComparesCanonicalJSON(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := queries.NewNotificationQueries(db)
	started := `{"bot_token":"t","chat_id":"1"}`
	if err := q.Create(&queries.NotificationChannel{ID: "ch1", ChannelType: "telegram", Name: "n", Config: started, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g := GuardChannelAction(q, "ch1", started, func(string) error { calls++; return nil })
	// Formatting-only change (whitespace, key order): still the same config.
	if err := q.UpdateFull("ch1", "n", "{ \"chat_id\": \"1\",\n  \"bot_token\": \"t\" }"); err != nil {
		t.Fatal(err)
	}
	if err := g("a"); err != nil || calls != 1 {
		t.Fatalf("formatting-only change must still approve: err=%v", err)
	}
	if err := q.UpdateFull("ch1", "n", `{"bot_token":"t","chat_id":"2"}`); err != nil {
		t.Fatal(err)
	}
	if err := g("a"); !errors.Is(err, ErrChannelNotAuthoritative) || calls != 1 {
		t.Fatalf("semantic change must refuse: err=%v", err)
	}
}

func TestApprovalNotificationShowsQueryAndBodyHash(t *testing.T) {
	n := ApprovalNotification{Method: "POST", Path: "/v1/pay", Query: "mode=live", BodyHash: "abc123def456"}
	if n.requestTarget() != "/v1/pay?mode=live" {
		t.Fatalf("target=%q", n.requestTarget())
	}
	if (ApprovalNotification{Path: "/x"}).requestTarget() != "/x" || bodyLine("") != "" {
		t.Fatal("no query / no hash must add nothing")
	}
	if !strings.Contains(bodyLine(n.BodyHash), "abc123def456") {
		t.Fatalf("body line: %q", bodyLine(n.BodyHash))
	}
	// Discord and webhook payloads carry them: capture the telegram request text.
	var sent string
	nt := &Notifier{client: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		return okJSON(`{}`), nil
	})}}
	if err := nt.sendTelegram(`{"bot_token":"1:x","chat_id":"2"}`, n); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "/v1/pay?mode=live") || !strings.Contains(sent, "abc123def456") {
		t.Fatalf("telegram text lacks query or hash: %s", sent)
	}
}

// Request-controlled text must not break out of the markdown code span.
func TestRequestTargetNeutralisesBackticks(t *testing.T) {
	n := ApprovalNotification{Path: "/a`b", Query: "x=`*bold*`"}
	if got := n.requestTarget(); strings.Contains(got, "`") || got != "/a'b?x='*bold*'" {
		t.Fatalf("target=%q", got)
	}
}
