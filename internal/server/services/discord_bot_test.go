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

func TestCreateCategory(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/guilds/G123/channels" || r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var b map[string]interface{}
		_ = json.Unmarshal(body, &b)
		if b["type"] != float64(4) {
			t.Errorf("type = %v, want 4", b["type"])
		}
		if b["name"] != "duckway-control" {
			t.Errorf("name = %v", b["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"CAT1","name":"duckway-control","type":4,"guild_id":"G123"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}

	ch, err := b.CreateCategory(context.Background(), "tok", "G123", "Duckway Control")
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID != "CAT1" || ch.Type != 4 {
		t.Errorf("got %+v", ch)
	}
}

func TestGrantCategoryAccess(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/CAT1/permissions/BOT1" || r.Method != "PUT" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var b map[string]interface{}
		_ = json.Unmarshal(body, &b)
		if b["type"] != float64(1) {
			t.Errorf("type = %v, want member overwrite", b["type"])
		}
		if b["allow"] != "68688" {
			t.Errorf("allow = %v, want Duckway category permissions", b["allow"])
		}
		if b["deny"] != "0" {
			t.Errorf("deny = %v, want 0", b["deny"])
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}

	if err := b.GrantCategoryAccess(context.Background(), "tok", "CAT1", "BOT1"); err != nil {
		t.Fatal(err)
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

func TestCurrentUserAndListGuilds(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/@me":
			w.Write([]byte(`{"id":"BOT1","username":"duckway"}`))
		case "/users/@me/guilds":
			w.Write([]byte(`[{"id":"G1","name":"Alpha"},{"id":"G2","name":"Beta"}]`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}

	u, err := b.CurrentUser(context.Background(), "tok")
	if err != nil || u.ID != "BOT1" {
		t.Fatalf("CurrentUser = %+v, %v", u, err)
	}
	guilds, err := b.ListGuilds(context.Background(), "tok")
	if err != nil || len(guilds) != 2 || guilds[0].Name != "Alpha" {
		t.Fatalf("ListGuilds = %+v, %v", guilds, err)
	}
}

func TestDiscordInviteURL(t *testing.T) {
	u := DiscordInviteURL("BOT1")
	if !strings.Contains(u, "client_id=BOT1") || !strings.Contains(u, "permissions=68688") || !strings.Contains(u, "scope=bot+applications.commands") {
		t.Fatalf("invite url = %s", u)
	}
}

func TestDiscordSetupInviteURL(t *testing.T) {
	u := DiscordSetupInviteURL("BOT1")
	if !strings.Contains(u, "client_id=BOT1") || !strings.Contains(u, "permissions=268504144") || !strings.Contains(u, "scope=bot+applications.commands") {
		t.Fatalf("setup invite url = %s", u)
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

func TestDeleteChannel(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/CH9" || r.Method != "DELETE" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"id":"CH9"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := b.DeleteChannel(context.Background(), "tok", "CH9"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteChannel_404IsIdempotent(t *testing.T) {
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"code":10003,"message":"Unknown Channel"}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := b.DeleteChannel(context.Background(), "tok", "CH"); err != nil {
		t.Fatalf("404 should swallow: %v", err)
	}
}

func TestDeleteChannel_EmptyID(t *testing.T) {
	b := NewDiscordBot()
	if err := b.DeleteChannel(context.Background(), "tok", ""); err != nil {
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

func TestDiscordError_FieldDetail(t *testing.T) {
	// Real shape of a 400 Invalid Form Body — the per-field reason is
	// in errors.<field>._errors[].message.
	srv := mockDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{
		  "code": 50035,
		  "message": "Invalid Form Body",
		  "errors": {
		    "parent_id": {"_errors":[{"code":"CHANNEL_PARENT_INVALID_TYPE","message":"The parent of a channel must be a category."}]},
		    "name": {"_errors":[{"code":"STRING_TYPE_REGEX","message":"Must match ^[\\w-]+$"}]}
		  }
		}`))
	})
	defer srv.Close()
	b := &DiscordBot{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := b.CreateChannel(context.Background(), "tok", CreateChannelOpts{
		GuildID: "G", ParentID: "BAD", Name: "x",
	})
	derr, ok := err.(*DiscordError)
	if !ok {
		t.Fatalf("expected DiscordError, got %T: %v", err, err)
	}
	msg := derr.Error()
	if !strings.Contains(msg, "Invalid Form Body") {
		t.Errorf("missing main message: %q", msg)
	}
	// Both field-level reasons should appear so the operator sees the
	// real problem.
	if !strings.Contains(msg, "parent_id") || !strings.Contains(msg, "category") {
		t.Errorf("missing parent_id detail: %q", msg)
	}
	if !strings.Contains(msg, "name") {
		t.Errorf("missing name detail: %q", msg)
	}
}
