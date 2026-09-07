package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestSplitDiscordContentKeepsShortMessageSinglePart(t *testing.T) {
	parts := splitDiscordContent("short reply")
	if len(parts) != 1 || parts[0] != "short reply" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestCreateCCChannelRequestIDRecoversReservedDiscordChannel(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	crypto := services.NewCrypto(make([]byte, 32))
	encrypted, err := crypto.Encrypt("bot-token")
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO services(id,name,display_name,upstream_url,host_pattern) VALUES('svc','discord','Discord','https://discord.test','discord.test')`, nil},
		{`INSERT INTO clients(id,name,token_hash) VALUES('client1','client','hash')`, nil},
		{`INSERT INTO api_keys(id,service_id,name,key_encrypted) VALUES('key','svc','bot',?)`, []any{encrypted}},
		{`INSERT INTO control_channels(id,name,service_id,api_key_id,client_id,agent_type,placeholder_id,config,is_active) VALUES('cc1','CC','svc','key','client1','codex','',?,1)`, []any{`{"guild_id":"G1","category_id":"CAT1"}`}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	requestID := "discord-123"
	digest := sha256.Sum256([]byte("cc1:" + requestID))
	handle := fmt.Sprintf("dwch_p%x", digest[:12])
	clientID := "client1"
	q := queries.NewControlChannelQueries(db)
	if err := q.CreateChannel(&models.CCChannel{Handle: handle, CCID: "cc1", ClientID: &clientID, Name: "review", Topic: "human topic", Kind: "task", Cwd: "/work"}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/guilds/G1/channels":
			fmt.Fprintf(w, `[{"id":"D1","name":"review","type":0,"topic":"human topic\n[duckway-provision:%x]","parent_id":"CAT1","guild_id":"G1"}]`, digest[:12])
		case r.Method == http.MethodPost:
			posts++
			http.Error(w, "duplicate create", http.StatusInternalServerError)
		case r.Method == http.MethodPatch && r.URL.Path == "/channels/D1":
			w.Write([]byte(`{"id":"D1"}`))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer discord.Close()
	h := NewCCClientHandler(q, queries.NewAPIKeyQueries(db), crypto, &services.DiscordBot{BaseURL: discord.URL, HTTP: discord.Client()}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/client/cc/channels", strings.NewReader(`{"request_id":"discord-123","name":"review","topic":"human topic","cwd":"/work"}`))
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client1", IsActive: true}))
	rec := httptest.NewRecorder()
	h.CreateChannel(rec, req)
	if rec.Code != http.StatusCreated || posts != 0 || !strings.Contains(rec.Body.String(), handle) {
		t.Fatalf("status=%d posts=%d body=%s", rec.Code, posts, rec.Body.String())
	}
	channel, err := q.GetChannelByHandle(handle)
	if err != nil || channel.ChannelID != "D1" {
		t.Fatalf("channel=%+v err=%v", channel, err)
	}

	// Replaying after activation returns the same opaque handle without any
	// Discord list/create call.
	req = httptest.NewRequest(http.MethodPost, "/client/cc/channels", strings.NewReader(`{"request_id":"discord-123","name":"review","topic":"human topic","cwd":"/work"}`))
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client1", IsActive: true}))
	rec = httptest.NewRecorder()
	h.CreateChannel(rec, req)
	if rec.Code != http.StatusOK || posts != 0 || !strings.Contains(rec.Body.String(), handle) {
		t.Fatalf("replay status=%d posts=%d body=%s", rec.Code, posts, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/client/cc/channels", strings.NewReader(`{"request_id":"discord-123","name":"review","topic":"human topic","cwd":"/other"}`))
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client1", IsActive: true}))
	rec = httptest.NewRecorder()
	h.CreateChannel(rec, req)
	if rec.Code != http.StatusConflict || posts != 0 {
		t.Fatalf("conflict status=%d posts=%d body=%s", rec.Code, posts, rec.Body.String())
	}
}

func TestDeleteCCChannelIsTaskOnlyAndCrossCCFailClosed(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	crypto := services.NewCrypto(make([]byte, 32))
	encrypted, err := crypto.Encrypt("bot-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO services(id,name,display_name,upstream_url,host_pattern) VALUES('svc','discord','Discord','https://discord.test','discord.test')`,
		`INSERT INTO clients(id,name,token_hash) VALUES('client1','one','hash1'),('client2','two','hash2')`,
		`INSERT INTO api_keys(id,service_id,name,key_encrypted) VALUES('key','svc','bot','` + encrypted + `')`,
		`INSERT INTO control_channels(id,name,service_id,api_key_id,client_id,agent_type,placeholder_id,config,is_active) VALUES
		 ('cc1','One','svc','key','client1','codex','','{}',1),('cc2','Two','svc','key','client2','codex','','{}',1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	q := queries.NewControlChannelQueries(db)
	client1 := "client1"
	if err := q.CreateChannel(&models.CCChannel{Handle: "dwch_task", CCID: "cc1", ClientID: &client1, ChannelID: "D1", Name: "task", Kind: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateChannel(&models.CCChannel{Handle: "dwch_mgmt", CCID: "cc1", ClientID: &client1, ChannelID: "M1", Name: "control", Kind: "management"}); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/channels/D1" {
			deletes++
			w.Write([]byte(`{"id":"D1"}`))
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	defer discord.Close()
	h := NewCCClientHandler(q, queries.NewAPIKeyQueries(db), crypto, &services.DiscordBot{BaseURL: discord.URL, HTTP: discord.Client()}, nil, nil)

	request := func(clientID, handle string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/client/cc/channels/"+handle, nil)
		req.SetPathValue("handle", handle)
		req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: clientID, IsActive: true}))
		rec := httptest.NewRecorder()
		h.DeleteChannel(rec, req)
		return rec
	}
	if rec := request("client2", "dwch_task"); rec.Code != http.StatusForbidden || deletes != 0 {
		t.Fatalf("cross-CC delete status=%d deletes=%d body=%s", rec.Code, deletes, rec.Body.String())
	}
	if rec := request("client1", "dwch_mgmt"); rec.Code != http.StatusBadRequest || deletes != 0 {
		t.Fatalf("management delete status=%d deletes=%d body=%s", rec.Code, deletes, rec.Body.String())
	}
	if rec := request("client1", "dwch_task"); rec.Code != http.StatusNoContent || deletes != 1 {
		t.Fatalf("owner delete status=%d deletes=%d body=%s", rec.Code, deletes, rec.Body.String())
	}
	if _, err := q.GetChannelByHandle("dwch_task"); err == nil {
		t.Fatal("deleted task channel row remains")
	}
}

func TestSplitDiscordContentSplitsLongMessages(t *testing.T) {
	parts := splitDiscordContent(strings.Repeat("abcdefghi\n", 260))
	if len(parts) < 2 {
		t.Fatalf("expected long message to split, got %d part(s)", len(parts))
	}
	for i, part := range parts {
		if len([]rune(part)) > 2000 {
			t.Fatalf("part %d length = %d, want <= 2000", i, len([]rune(part)))
		}
		if !strings.HasPrefix(part, "(part ") {
			t.Fatalf("part %d missing prefix: %.32q", i, part)
		}
	}
}

func TestCanonicalDucklionSessionID(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"ABC123", "ABC123"},
		{"abc123", "ABC123"},
	} {
		got, err := canonicalDucklionSessionID(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("canonicalDucklionSessionID(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, invalid := range []string{"ABC12", "ABC12I", "ABC12O", "ABC12-"} {
		if _, err := canonicalDucklionSessionID(invalid); err == nil {
			t.Fatalf("canonicalDucklionSessionID(%q) unexpectedly succeeded", invalid)
		}
	}
}
