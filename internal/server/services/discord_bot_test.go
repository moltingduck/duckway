package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockDiscord stands in for the Discord API so we can verify request shape +
// auth header without going to the network. Each handler receives the
// captured request via the channel for inspection.
func mockDiscord(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bot ") {
			t.Errorf("missing Bot auth, got %q", got)
		}
		handler(w, r)
	}))
}

func TestSanitizeChannelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"  test  ", "test"},
		{"My Project!!!", "my-project"},
		{"agent-001", "agent-001"},
		{"", "channel"},
		{"---", "channel"},
		{"UPPER_lower-123", "upper_lower-123"},
		{strings.Repeat("a", 200), strings.Repeat("a", 100)},
	}
	for _, c := range cases {
		if got := sanitizeChannelName(c.in); got != c.want {
			t.Errorf("sanitizeChannelName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCreateChannel(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/guilds/G123/channels" || r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var b map[string]interface{}
		_ = json.Unmarshal(body, &b)
		if b["parent_id"] != "C456" {
			t.Errorf("parent_id = %v, want C456", b["parent_id"])
		}
		if b["name"] != "alpha-bot" {
			t.Errorf("name = %v, want alpha-bot", b["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"NEW1","name":"alpha-bot","type":0,"parent_id":"C456","guild_id":"G123"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}

	ch, err := b.CreateChannel(context.Background(), "tok", CreateChannelOpts{
		GuildID: "G123", ParentID: "C456", Name: "Alpha Bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID != "NEW1" || ch.Name != "alpha-bot" {
		t.Errorf("got %+v", ch)
	}
}

func TestCreateChannelForbidden(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"code": 50013, "message": "Missing Permissions"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}

	_, err := b.CreateChannel(context.Background(), "tok", CreateChannelOpts{
		GuildID: "G", ParentID: "C", Name: "x",
	})
	derr, ok := err.(*DiscordError)
	if !ok {
		t.Fatalf("expected *DiscordError, got %T: %v", err, err)
	}
	if !derr.IsForbidden() || derr.Code != 50013 {
		t.Errorf("wrong error: %+v", derr)
	}
}

func TestArchiveChannel(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/CH1" || r.Method != "PATCH" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var b map[string]interface{}
		_ = json.Unmarshal(body, &b)
		if b["name"] != "archived-old-name" {
			t.Errorf("name = %v", b["name"])
		}
		if _, hasParent := b["parent_id"]; !hasParent {
			t.Errorf("parent_id should be present (and null) to remove from category")
		}
		w.Write([]byte(`{"id":"CH1"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}

	if err := b.ArchiveChannel(context.Background(), "tok", "CH1", "old-name"); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveChannel_404IsIdempotent(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"code":10003,"message":"Unknown Channel"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := b.ArchiveChannel(context.Background(), "tok", "CH1", "x"); err != nil {
		t.Fatalf("404 should be swallowed: %v", err)
	}
}

func TestArchiveChannel_EmptyID(t *testing.T) {
	b := NewDiscordBot()
	// no upstream call expected
	if err := b.ArchiveChannel(context.Background(), "tok", "", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestPostMessage(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/CH/messages" || r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"id":"M1","channel_id":"CH"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	id, err := b.PostMessage(context.Background(), "tok", "CH", "hi")
	if err != nil || id != "M1" {
		t.Errorf("got %q, %v", id, err)
	}
}

func TestGetMessages(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "10" {
			t.Errorf("limit = %v", r.URL.Query().Get("limit"))
		}
		w.Write([]byte(`[
		  {"id":"M2","channel_id":"CH","content":"hello","author":{"id":"U1","username":"alice","bot":false},"timestamp":"2026-01-01T00:00:00Z"},
		  {"id":"M1","channel_id":"CH","content":"hi","author":{"id":"U1","username":"alice","bot":false},"timestamp":"2026-01-01T00:00:00Z"}
		]`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	msgs, err := b.GetMessages(context.Background(), "tok", "CH", 10)
	if err != nil || len(msgs) != 2 || msgs[0].ID != "M2" {
		t.Errorf("got %+v, %v", msgs, err)
	}
}

func TestListGuildChannels(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/guilds/G/channels" {
			t.Errorf("path: %v", r.URL.Path)
		}
		w.Write([]byte(`[{"id":"A","name":"alpha","type":0,"parent_id":"CAT"},{"id":"B","name":"beta","type":0,"parent_id":"CAT"}]`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	chans, err := b.ListGuildChannels(context.Background(), "tok", "G")
	if err != nil || len(chans) != 2 {
		t.Errorf("got %+v, %v", chans, err)
	}
}

func TestDoErrorBubbling(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"message":"You are being rate limited.","retry_after":1.5}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := b.PostMessage(context.Background(), "tok", "CH", "x")
	derr, ok := err.(*DiscordError)
	if !ok {
		t.Fatalf("expected DiscordError, got %T", err)
	}
	if derr.Status != 429 {
		t.Errorf("got status %d", derr.Status)
	}
}
