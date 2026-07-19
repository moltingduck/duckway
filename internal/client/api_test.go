package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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

func TestPostCCReplyRetriesDialFailure(t *testing.T) {
	var attempts int
	api := NewAPIClient("http://gateway.invalid", "tok")
	api.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}

	err := api.postCCReplyOneWithBackoff(context.Background(), "dwch_t", "result", "message-id", []time.Duration{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d, want 3", attempts)
	}
}

func TestPostCCReplyDoesNotRetryAmbiguousFailure(t *testing.T) {
	var attempts int
	api := NewAPIClient("http://gateway.invalid", "tok")
	api.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, io.ErrUnexpectedEOF
	})}

	err := api.postCCReplyOneWithBackoff(context.Background(), "dwch_t", "result", "message-id", []time.Duration{0, 0, 0})
	if err == nil {
		t.Fatal("expected post failure")
	}
	if attempts != 1 {
		t.Fatalf("ambiguous failure attempts=%d, want 1", attempts)
	}
}
