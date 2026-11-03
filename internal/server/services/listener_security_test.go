package services

import (
	"bytes"
	"errors"
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
