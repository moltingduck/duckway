package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestControlChannelTestAgentPublishesHi(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-cc-agent-test', 'discord-test', 'Discord Test', 'https://discord.test', 'discord.test')`)
	exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-cc-agent-test', 'agent client', 'hash-agent-client')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		VALUES ('key-cc-agent-test', 'svc-cc-agent-test', 'bot key', 'encrypted')`)
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		VALUES ('cc-agent-test', 'Agent CC', 'svc-cc-agent-test', 'key-cc-agent-test', 'client-cc-agent-test', 'codex', '', '{}', 1)`)
	exec(`INSERT INTO cc_channels (handle, cc_id, client_id, channel_id, name, kind, archived)
		VALUES ('dwch_mgmt_test', 'cc-agent-test', 'client-cc-agent-test', 'CHAN1', 'agent-control', 'management', 0)`)

	hub := services.NewCCEventHub()
	events, unsubscribe := hub.Subscribe("client-cc-agent-test")
	defer unsubscribe()
	h := handlers.NewControlChannelHandler(
		queries.NewControlChannelQueries(db),
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		queries.NewServiceQueries(db),
		queries.NewClientQueries(db),
		nil,
		nil,
		nil,
	)
	h.SetHub(hub)

	req := httptest.NewRequest(http.MethodPost, "/api/cc/cc-agent-test/test-agent", strings.NewReader("{}"))
	req.SetPathValue("id", "cc-agent-test")
	rec := httptest.NewRecorder()
	h.TestAgent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		InboxID int64  `json:"inbox_id"`
		TestID  string `json:"test_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.InboxID == 0 {
		t.Fatal("inbox_id missing from response")
	}
	if res.TestID == "" {
		t.Fatal("test_id missing from response")
	}

	select {
	case ev := <-events:
		if ev.Type != "message_create" || ev.CCID != "cc-agent-test" || ev.Handle != "dwch_mgmt_test" || ev.Kind != "management" {
			t.Fatalf("unexpected event: %+v", ev)
		}
		var payload struct {
			Content string `json:"content"`
			TestID  string `json:"duckway_test_id"`
			Author  struct {
				Bot bool `json:"bot"`
			} `json:"author"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Content != "hi" || payload.Author.Bot || payload.TestID != res.TestID {
			t.Fatalf("unexpected payload: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("no event published")
	}

	var inboxPayload string
	if err := db.QueryRow(`SELECT payload FROM discord_inbox WHERE id = ?`, res.InboxID).Scan(&inboxPayload); err != nil {
		t.Fatalf("lookup inbox row: %v", err)
	}
	var inboxMsg struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(inboxPayload), &inboxMsg); err != nil {
		t.Fatalf("decode inbox payload: %v", err)
	}
	if inboxMsg.Content != "hi" {
		t.Fatalf("inbox content = %q, want hi", inboxMsg.Content)
	}
	testStatus, err := queries.NewControlChannelQueries(db).GetAgentTest("cc-agent-test", res.TestID)
	if err != nil {
		t.Fatalf("lookup agent test: %v", err)
	}
	if testStatus.Status != "queued" || testStatus.InboxID != res.InboxID {
		t.Fatalf("unexpected agent test status: %+v", testStatus)
	}
}

func TestControlChannelUpdateAgentType(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-cc-update-test', 'discord-update-test', 'Discord Update Test', 'https://discord.test', 'discord.test')`)
	exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-cc-update-test', 'update client', 'hash-update-client')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		VALUES ('key-cc-update-test', 'svc-cc-update-test', 'bot key', 'encrypted')`)
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		VALUES ('cc-update-test', 'Update CC', 'svc-cc-update-test', 'key-cc-update-test', 'client-cc-update-test', 'claude_code', '', '{}', 1)`)

	h := handlers.NewControlChannelHandler(
		queries.NewControlChannelQueries(db),
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		queries.NewServiceQueries(db),
		queries.NewClientQueries(db),
		nil,
		nil,
		nil,
	)

	req := httptest.NewRequest(http.MethodPut, "/api/cc/cc-update-test", strings.NewReader(`{"agent_type":"codex","is_active":true}`))
	req.SetPathValue("id", "cc-update-test")
	rec := httptest.NewRecorder()
	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	cc, err := queries.NewControlChannelQueries(db).GetByID("cc-update-test")
	if err != nil {
		t.Fatal(err)
	}
	if cc.AgentType != "codex" {
		t.Fatalf("agent_type = %q, want codex", cc.AgentType)
	}
}

func TestCCClientReportAgentTestRejectsUnknownOrForeignTest(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	crypto := services.NewCrypto(make([]byte, 32))
	encrypted, err := crypto.Encrypt("discord-token")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-report-test', 'discord', 'Discord', 'https://discord.test', 'discord.test')`)
	exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-a', 'client a', 'hash-a'), ('client-b', 'client b', 'hash-b')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		VALUES ('key-report-test', 'svc-report-test', 'bot key', ?)`, encrypted)
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		VALUES ('cc-a', 'CC A', 'svc-report-test', 'key-report-test', 'client-a', 'codex', '', '{}', 1),
		       ('cc-b', 'CC B', 'svc-report-test', 'key-report-test', 'client-b', 'codex', '', '{}', 1)`)
	exec(`INSERT INTO cc_agent_tests (id, cc_id, client_id, handle, agent_type, status, inbox_id)
		VALUES ('test-b', 'cc-b', 'client-b', 'dwch_b', 'codex', 'queued', 1)`)

	h := handlers.NewCCClientHandler(queries.NewControlChannelQueries(db), queries.NewAPIKeyQueries(db), crypto, nil, nil, nil)
	clientA := &models.Client{ID: "client-a", Name: "client a", IsActive: true}

	for _, testID := range []string{"test-b", "missing-test"} {
		req := httptest.NewRequest(http.MethodPost, "/client/cc/agent-tests/"+testID, strings.NewReader(`{"status":"replied"}`))
		req.SetPathValue("test_id", testID)
		req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, clientA))
		rec := httptest.NewRecorder()
		h.ReportAgentTest(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", testID, rec.Code, rec.Body.String())
		}
	}
}
