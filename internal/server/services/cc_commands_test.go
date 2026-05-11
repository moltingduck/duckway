package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	_ "modernc.org/sqlite"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"!new task-1", []string{"!new", "task-1"}},
		{"!new task-1 --cwd /tmp/foo", []string{"!new", "task-1", "--cwd", "/tmp/foo"}},
		{`!new task-1 --topic "multi word topic"`, []string{"!new", "task-1", "--topic", "multi word topic"}},
		{"   !help   ", []string{"!help"}},
		{"", nil},
	}
	for _, c := range cases {
		got := parseArgs(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseArgs(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSplitSlugAndFlags(t *testing.T) {
	slug, flags, err := splitSlugAndFlags([]string{"task-1", "--cwd", "/tmp/foo", "--topic", "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	if slug != "task-1" || flags["cwd"] != "/tmp/foo" || flags["topic"] != "Hello" {
		t.Errorf("got slug=%q flags=%v", slug, flags)
	}
}

func TestSplitSlugAndFlags_MissingSlug(t *testing.T) {
	if _, _, err := splitSlugAndFlags(nil); err == nil {
		t.Error("expected error on empty args")
	}
	if _, _, err := splitSlugAndFlags([]string{"--cwd", "/tmp"}); err == nil {
		t.Error("expected error when only flags given")
	}
}

func TestSplitSlugAndFlags_FlagWithoutValue(t *testing.T) {
	if _, _, err := splitSlugAndFlags([]string{"task", "--cwd"}); err == nil {
		t.Error("expected error on --cwd with no value")
	}
}

func TestSplitSlugAndFlags_DuplicatePositional(t *testing.T) {
	if _, _, err := splitSlugAndFlags([]string{"task-1", "task-2"}); err == nil {
		t.Error("expected error on second positional arg")
	}
}

func TestSuggestCommands(t *testing.T) {
	cases := []struct {
		typed string
		want  []string
	}{
		{"!lits", []string{"!list"}},     // 1 edit
		{"!hep", []string{"!help"}},      // 1 deletion
		{"!hlep", []string{"!help"}},     // 1 transposition (= 2 edits)
		{"!resset", []string{"!reset"}},  // 1 insertion
		{"!frobnicate", nil},             // too far
		{"!nw", []string{"!new"}},        // 1 deletion
	}
	for _, c := range cases {
		got := suggestCommands(c.typed, 2)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if len(got) == 0 || got[0] != c.want[0] {
			t.Errorf("suggestCommands(%q) = %v, want top=%v", c.typed, got, c.want)
		}
	}
}

func TestUnknownCommandReply(t *testing.T) {
	cases := []struct {
		typed string
		want  string // substring that must appear
	}{
		{"!lits", "Did you mean `!list`"},
		{"!frobnicate", "Type `!help`"},  // no close match → fall back to !help nudge
	}
	for _, c := range cases {
		got := unknownCommandReply(c.typed)
		if !strings.Contains(got, c.want) {
			t.Errorf("unknownCommandReply(%q) = %q, want substring %q", c.typed, got, c.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"!list", "!list", 0},
		{"!list", "!lits", 2},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestBuildWelcomeMessage(t *testing.T) {
	got := BuildWelcomeMessage("my-laptop")
	want := []string{
		"my-laptop",  // client name appears
		"!new",       // every command is mentioned
		"!end",
		"!reset",
		"!list",
		"!status",
		"!help",
		"cc watch",   // operator nudge about the daemon
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("welcome message missing %q\n---\n%s", w, got)
		}
	}
}

func TestLooksLikeCommand(t *testing.T) {
	for _, in := range []string{"!help", "!new task", "  !list ", "!"} {
		if !LooksLikeCommand(in) {
			t.Errorf("LooksLikeCommand(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"hello", " hi", "/help", ""} {
		if LooksLikeCommand(in) {
			t.Errorf("LooksLikeCommand(%q) = true, want false", in)
		}
	}
}

func TestShort(t *testing.T) {
	if got := short("abcdefghij", 5); got != "abcde…" {
		t.Errorf("short = %q", got)
	}
	if got := short("ab", 5); got != "ab" {
		t.Errorf("short short-circuit = %q", got)
	}
}

// --- dispatch-level integration tests ----------------------------------

// commandHarness sets up a real sqlite DB with the minimal schema, an
// AES-256-GCM crypto, a mock Discord that records incoming REST calls,
// and a CCCommandHandler wired up against them.
type commandHarness struct {
	t       *testing.T
	db      *sql.DB
	cc      *queries.ControlChannelQueries
	apiKeys *queries.APIKeyQueries
	bot     *DiscordBot
	hub     *CCEventHub
	handler *CCCommandHandler

	mu   sync.Mutex
	hits []string         // METHOD path
	reqs []map[string]any // POST/PATCH bodies
	cc1  *models.ControlChannel
	mgmt *models.CCChannel
}

func newCommandHarness(t *testing.T) *commandHarness {
	t.Helper()

	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=off")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE control_channels (id TEXT PRIMARY KEY, name TEXT, service_id TEXT, api_key_id TEXT, client_id TEXT, agent_type TEXT, placeholder_id TEXT, config TEXT, is_active INT, created_at TEXT)`,
		`CREATE TABLE cc_channels (handle TEXT PRIMARY KEY, cc_id TEXT, client_id TEXT, channel_id TEXT, name TEXT, topic TEXT, kind TEXT, session_id TEXT, cwd TEXT, archived INT, created_at TEXT, last_seen_at TEXT)`,
		`CREATE TABLE discord_inbox (id INTEGER PRIMARY KEY AUTOINCREMENT, cc_id TEXT, channel_handle TEXT, event_type TEXT, payload TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE TABLE api_keys (id TEXT PRIMARY KEY, service_id TEXT, name TEXT, key_encrypted TEXT, acl TEXT DEFAULT '', refresh_token TEXT DEFAULT '', expires_at INT DEFAULT 0, token_endpoint TEXT DEFAULT '', subscription_info TEXT DEFAULT '', usage_snapshot TEXT DEFAULT '', is_active INT DEFAULT 1, usage_count INT DEFAULT 0, last_used_at TEXT, created_at TEXT)`,
		`CREATE TABLE services (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE clients (id TEXT PRIMARY KEY, name TEXT)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	crypto := NewCrypto([]byte("test-encryption-key-32-bytes-len"))
	encBot, _ := crypto.Encrypt("Bot fake-token")
	cfg := `{"guild_id":"G1","category_id":"CAT1"}`
	if _, err := db.Exec(`INSERT INTO services VALUES ('svc1','discord')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients VALUES ('client1','test-client')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,created_at) VALUES ('key1','svc1','bot',?,datetime('now'))`, encBot); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO control_channels VALUES ('cc1','my-cc','svc1','key1','client1','claude_code','phph',?,1,datetime('now'))`, cfg); err != nil {
		t.Fatal(err)
	}
	mgmt := &models.CCChannel{
		Handle: "dwch_mgmt", CCID: "cc1", ClientID: ptr("client1"),
		ChannelID: "MGMT1", Name: "client1-control", Kind: "management",
	}
	if _, err := db.Exec(`INSERT INTO cc_channels VALUES (?,?,?,?,?,?,?,?,?,?,datetime('now'),null)`,
		mgmt.Handle, mgmt.CCID, *mgmt.ClientID, mgmt.ChannelID, mgmt.Name, mgmt.Topic, mgmt.Kind, mgmt.SessionID, mgmt.Cwd, 0); err != nil {
		t.Fatal(err)
	}

	h := &commandHarness{t: t, db: db, hub: NewCCEventHub(),
		cc: queries.NewControlChannelQueries(db), apiKeys: queries.NewAPIKeyQueries(db),
		mgmt: mgmt,
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var bm map[string]any
		_ = json.Unmarshal(body, &bm)
		h.mu.Lock()
		h.hits = append(h.hits, r.Method+" "+r.URL.Path)
		h.reqs = append(h.reqs, bm)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
			w.Write([]byte(`{"id":"M-1","channel_id":"X"}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/channels"):
			id := bm["name"].(string) + "-id"
			w.Write([]byte(`{"id":"` + id + `","name":"` + bm["name"].(string) + `","type":0}`))
		case r.Method == "PATCH":
			w.Write([]byte(`{"id":"X"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(mock.Close)

	h.bot = &DiscordBot{BaseURL: mock.URL, HTTP: mock.Client()}
	h.handler = NewCCCommandHandler(h.cc, h.apiKeys, crypto, h.bot, h.hub)

	cc, _ := h.cc.GetByID("cc1")
	h.cc1 = cc
	return h
}

func ptr(s string) *string { return &s }

func (h *commandHarness) hitsFor(method, suffix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, hit := range h.hits {
		if strings.HasPrefix(hit, method+" ") && strings.HasSuffix(hit, suffix) {
			n++
		}
	}
	return n
}

func (h *commandHarness) lastReplyContains(needle string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.reqs) - 1; i >= 0; i-- {
		if c, ok := h.reqs[i]["content"].(string); ok && strings.Contains(c, needle) {
			return true
		}
	}
	return false
}

func TestHandle_Help(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!help")
	if !h.lastReplyContains("Duckway CC commands") {
		t.Errorf("expected help text reply, hits=%v", h.hits)
	}
}

func TestHandle_New(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, `!new task-1 --cwd /home/me/proj --topic "alpha task"`)

	if h.hitsFor("POST", "/guilds/G1/channels") != 1 {
		t.Errorf("expected 1 channel-create POST, got %v", h.hits)
	}
	if !h.lastReplyContains("Created **#task-1**") {
		t.Errorf("expected success reply, got %v", h.reqs)
	}

	rows, err := h.db.Query(`SELECT handle, name, kind, cwd, topic FROM cc_channels`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	t.Log("cc_channels rows after !new:")
	var found bool
	for rows.Next() {
		var handle, name, kind, cwd, topic string
		_ = rows.Scan(&handle, &name, &kind, &cwd, &topic)
		t.Logf("  %s name=%q kind=%q cwd=%q topic=%q", handle, name, kind, cwd, topic)
		if kind == "task" && name == "task-1" {
			found = true
			if cwd != "/home/me/proj" || topic != "alpha task" {
				t.Errorf("task row fields wrong: cwd=%q topic=%q", cwd, topic)
			}
		}
	}
	if !found {
		t.Errorf("task-1 task row not persisted")
	}
}

func TestHandle_New_RejectsInTaskChannel(t *testing.T) {
	h := newCommandHarness(t)
	task := &models.CCChannel{Handle: "dwch_t", CCID: "cc1", ChannelID: "T1", Name: "t", Kind: "task"}
	h.handler.Handle(context.Background(), "cc1", task, "!new x")
	if !h.lastReplyContains("only works in the management channel") {
		t.Errorf("expected scope error, got %v", h.reqs)
	}
}

func TestHandle_End_FromTaskChannel(t *testing.T) {
	h := newCommandHarness(t)
	task := &models.CCChannel{Handle: "dwch_t", CCID: "cc1", ChannelID: "T1", Name: "t", Kind: "task"}
	if _, err := h.db.Exec(`INSERT INTO cc_channels VALUES (?,?,?,?,?,?,?,?,?,?,datetime('now'),null)`,
		task.Handle, task.CCID, "client1", task.ChannelID, task.Name, "", task.Kind, "", "", 0); err != nil {
		t.Fatal(err)
	}
	sub, unsub := h.hub.Subscribe("client1")
	defer unsub()

	h.handler.Handle(context.Background(), "cc1", task, "!end")

	// PostMessage farewell + PATCH archive
	if h.hitsFor("POST", "/messages") != 1 {
		t.Errorf("expected farewell POST, hits=%v", h.hits)
	}
	if h.hitsFor("PATCH", "/channels/T1") != 1 {
		t.Errorf("expected archive PATCH, hits=%v", h.hits)
	}
	// Row deleted
	if _, err := h.cc.GetChannelByHandle("dwch_t"); err == nil {
		t.Error("expected cc_channels row deleted")
	}
	// Hub event fired
	select {
	case ev := <-sub:
		if ev.Type != "channel_delete" || ev.Handle != "dwch_t" {
			t.Errorf("unexpected event: %+v", ev)
		}
	default:
		t.Error("expected channel_delete event published")
	}
}

func TestHandle_End_RejectsInManagement(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!end")
	if !h.lastReplyContains("inside a task channel") {
		t.Errorf("expected scope error, got %v", h.reqs)
	}
}

func TestHandle_Destroy_FromTaskChannel(t *testing.T) {
	h := newCommandHarness(t)
	if _, err := h.db.Exec(`INSERT INTO cc_channels VALUES ('dwch_d','cc1','client1','D-real','d','','task','sess-1','/cwd',0,datetime('now'),null)`); err != nil {
		t.Fatal(err)
	}
	task, _ := h.cc.GetChannelByHandle("dwch_d")
	sub, unsub := h.hub.Subscribe("client1")
	defer unsub()

	h.handler.Handle(context.Background(), "cc1", task, "!destroy")

	// Discord DELETE call fired
	if h.hitsFor("DELETE", "/channels/D-real") != 1 {
		t.Errorf("expected DELETE /channels/D-real, hits=%v", h.hits)
	}
	// Local row gone
	if _, err := h.cc.GetChannelByHandle("dwch_d"); err == nil {
		t.Error("cc_channels row should be deleted")
	}
	// channel_delete event fired so daemon clears its session map
	select {
	case ev := <-sub:
		if ev.Type != "channel_delete" || ev.Handle != "dwch_d" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Error("expected channel_delete event")
	}
}

func TestHandle_Sessions_ForwardsToDaemon(t *testing.T) {
	h := newCommandHarness(t)
	sub, unsub := h.hub.Subscribe("client1")
	defer unsub()

	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!sessions duckway")

	select {
	case ev := <-sub:
		if ev.Type != "client_command" || ev.Handle != "dwch_mgmt" {
			t.Errorf("wrong event: %+v", ev)
		}
		var p struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("payload parse: %v", err)
		}
		if p.Command != "!sessions" {
			t.Errorf("command = %q", p.Command)
		}
		if len(p.Args) != 1 || p.Args[0] != "duckway" {
			t.Errorf("args = %v", p.Args)
		}
	case <-time.After(time.Second):
		t.Fatal("no client_command event received")
	}

	// Server should NOT have replied directly via Discord — the daemon
	// owns the response.
	if h.hitsFor("POST", "/messages") != 0 {
		t.Errorf("server should not auto-reply, hits=%v", h.hits)
	}
}

func TestHandle_Sessions_NoDaemonReturnsErrorMessage(t *testing.T) {
	h := newCommandHarness(t)
	// No subscriber → SubscriberCount=0 → must reply with the offline error.
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!sessions")
	if !h.lastReplyContains("daemon offline") {
		t.Errorf("expected daemon-offline reply, got reqs=%v", h.reqs)
	}
}

func TestHandle_Bind_ForwardsToDaemonWithMultipleIDs(t *testing.T) {
	h := newCommandHarness(t)
	sub, unsub := h.hub.Subscribe("client1")
	defer unsub()

	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!bind sess-1 sess-2 sess-3")

	select {
	case ev := <-sub:
		var p struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Command != "!bind" || len(p.Args) != 3 {
			t.Errorf("unexpected payload: %+v", p)
		}
		if p.Args[0] != "sess-1" || p.Args[2] != "sess-3" {
			t.Errorf("arg ordering broken: %v", p.Args)
		}
	case <-time.After(time.Second):
		t.Fatal("no client_command event received")
	}
}

func TestHandle_Bind_RejectsInTaskChannel(t *testing.T) {
	h := newCommandHarness(t)
	if _, err := h.db.Exec(`INSERT INTO cc_channels VALUES ('dwch_t','cc1','client1','T-real','t','','task','','/cwd',0,datetime('now'),null)`); err != nil {
		t.Fatal(err)
	}
	task, _ := h.cc.GetChannelByHandle("dwch_t")

	h.handler.Handle(context.Background(), "cc1", task, "!bind sess-1")

	if !h.lastReplyContains("only works in the management channel") {
		t.Errorf("expected mgmt-only error, got %v", h.reqs)
	}
}

func TestHandle_Destroy_RejectsInManagement(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!destroy")
	if !h.lastReplyContains("delete the CC from") {
		t.Errorf("expected mgmt-scope error pointing at admin UI, got %v", h.reqs)
	}
}

func TestHandle_Reset_FromTaskChannel(t *testing.T) {
	h := newCommandHarness(t)
	if _, err := h.db.Exec(`INSERT INTO cc_channels VALUES ('dwch_t','cc1','client1','T1','t','','task','sess-aaa','/cwd',0,datetime('now'),null)`); err != nil {
		t.Fatal(err)
	}
	task, _ := h.cc.GetChannelByHandle("dwch_t")
	sub, unsub := h.hub.Subscribe("client1")
	defer unsub()

	h.handler.Handle(context.Background(), "cc1", task, "!reset")

	// Session_id cleared in DB
	updated, _ := h.cc.GetChannelByHandle("dwch_t")
	if updated.SessionID != "" {
		t.Errorf("session_id should be cleared, got %q", updated.SessionID)
	}
	// Channel still exists (NOT archived)
	if updated.Archived {
		t.Error("!reset should not archive the channel")
	}
	// Hub fired session_reset
	select {
	case ev := <-sub:
		if ev.Type != "session_reset" || ev.Handle != "dwch_t" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Error("expected session_reset event")
	}
	if !h.lastReplyContains("Session") {
		t.Errorf("expected reset reply, got %v", h.reqs)
	}
}

func TestHandle_Reset_RejectsInManagement(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!reset")
	if !h.lastReplyContains("inside a task channel") {
		t.Errorf("expected scope error, got %v", h.reqs)
	}
}

func TestHandle_List_Empty(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!list")
	if !h.lastReplyContains("no task channels") {
		t.Errorf("expected empty-state reply, got %v", h.reqs)
	}
}

func TestHandle_List_WithTasks(t *testing.T) {
	h := newCommandHarness(t)
	h.db.Exec(`INSERT INTO cc_channels VALUES ('h1','cc1','c','D1','alpha','','task','sess-aaa','/x',0,datetime('now'),null)`)
	h.db.Exec(`INSERT INTO cc_channels VALUES ('h2','cc1','c','D2','beta','','task','','/y',0,datetime('now'),null)`)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!list")
	if !h.lastReplyContains("alpha") || !h.lastReplyContains("beta") {
		t.Errorf("expected both channels in list, got %v", h.reqs)
	}
	if !h.lastReplyContains("sess-aaa") {
		t.Errorf("expected session id in output")
	}
}

func TestHandle_Status_DaemonOffline(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!status")
	if !h.lastReplyContains("offline") {
		t.Errorf("expected offline status, got %v", h.reqs)
	}
}

func TestHandle_Status_DaemonConnected(t *testing.T) {
	h := newCommandHarness(t)
	_, unsub := h.hub.Subscribe("client1")
	defer unsub()
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!status")
	if !h.lastReplyContains("connected") {
		t.Errorf("expected connected status, got %v", h.reqs)
	}
}

func TestHandle_UnknownCommand_NoMatch(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!frobnicate")
	if !h.lastReplyContains("Unknown command") {
		t.Errorf("expected unknown-command reply, got %v", h.reqs)
	}
	// Far-from-anything typos should NOT suggest — they'd be noise.
	if h.lastReplyContains("Did you mean") {
		t.Errorf("did not expect a suggestion for !frobnicate: %v", h.reqs)
	}
}

func TestHandle_UnknownCommand_Suggests(t *testing.T) {
	h := newCommandHarness(t)
	h.handler.Handle(context.Background(), "cc1", h.mgmt, "!lits")
	if !h.lastReplyContains("Did you mean") || !h.lastReplyContains("!list") {
		t.Errorf("expected '!list' suggestion for '!lits', got %v", h.reqs)
	}
}
