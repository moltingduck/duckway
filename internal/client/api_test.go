package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestPostCCReplySplitsLongDiscordMessages(t *testing.T) {
	var (
		mu       sync.Mutex
		contents []string
		replies  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/client/cc/channels/dwch_t/messages" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		var body struct {
			Content          string `json:"content"`
			ReplyToMessageID string `json:"reply_to_message_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		contents = append(contents, body.Content)
		replies = append(replies, body.ReplyToMessageID)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	long := strings.Repeat("abcdefghi\n", 260)
	api := NewAPIClient(srv.URL, "tok")
	if err := api.PostCCReply(context.Background(), "dwch_t", long, "1783330000000000001"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(contents) < 2 {
		t.Fatalf("expected long message to be split, got %d post(s)", len(contents))
	}
	for i, content := range contents {
		if len([]rune(content)) > 2000 {
			t.Fatalf("chunk %d length = %d, want <= 2000", i, len([]rune(content)))
		}
		if !strings.HasPrefix(content, "(part ") {
			t.Fatalf("chunk %d missing part prefix: %q", i, content[:min(len(content), 32)])
		}
		if replies[i] != "1783330000000000001" {
			t.Fatalf("reply id for chunk %d = %q", i, replies[i])
		}
	}
}

func TestSplitDiscordMessageKeepsShortMessageSinglePart(t *testing.T) {
	parts := splitDiscordMessage("short reply")
	if len(parts) != 1 || parts[0] != "short reply" {
		t.Fatalf("parts = %#v", parts)
	}
}
