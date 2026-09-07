package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/models"
	"golang.org/x/net/websocket"
)

// TestDiscordGatewayResumeReplayE2E exercises the real websocket handshake,
// READY/RESUMED state, heartbeat ACK, routing policy, durable admission and
// replay dedup against an in-process Discord fixture.
func TestDiscordGatewayResumeReplayE2E(t *testing.T) {
	h := newCommandHarness(t)
	clientID := "client1"
	if err := h.cc.CreateChannel(&models.CCChannel{Handle: "dwch_e2e", CCID: "cc1", ClientID: &clientID,
		ChannelID: "9001", Name: "e2e", Kind: "task"}); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	var connections atomic.Int32
	var mu sync.Mutex
	var receivedOps []int
	wsHandler := websocket.Handler(func(ws *websocket.Conn) {
		connection := connections.Add(1)
		defer ws.Close()
		mustSendGateway(t, ws, gatewayPayload{Op: 10, D: json.RawMessage(`{"heartbeat_interval":20}`)})
		var handshake gatewayPayload
		if err := websocket.JSON.Receive(ws, &handshake); err != nil {
			t.Errorf("receive handshake: %v", err)
			return
		}
		mu.Lock()
		receivedOps = append(receivedOps, handshake.Op)
		mu.Unlock()
		if connection == 1 {
			var identify struct {
				Token   string `json:"token"`
				Intents int    `json:"intents"`
			}
			if err := json.Unmarshal(handshake.D, &identify); err != nil || identify.Token != "fixture-token" || identify.Intents == 0 {
				t.Errorf("invalid identify payload: %s", handshake.D)
				return
			}
			ready := fmt.Sprintf(`{"session_id":"sess-e2e","resume_gateway_url":%q,"user":{"id":"BOT"}}`, strings.Replace(server.URL, "http://", "ws://", 1)+"/ws")
			seq := 10
			mustSendGateway(t, ws, gatewayPayload{Op: 0, T: "READY", S: &seq, D: json.RawMessage(ready)})
			var heartbeat gatewayPayload
			if err := websocket.JSON.Receive(ws, &heartbeat); err != nil {
				t.Errorf("receive heartbeat: %v", err)
				return
			}
			if heartbeat.Op != 1 {
				t.Errorf("heartbeat op=%d", heartbeat.Op)
				return
			}
			mustSendGateway(t, ws, gatewayPayload{Op: 11})
			seq = 11
			mustSendGateway(t, ws, discordDispatch("MESSAGE_CREATE", seq, "111111111111111111", "first"))
			mustSendGateway(t, ws, gatewayPayload{Op: 7})
			return
		}
		var resume struct {
			Token     string `json:"token"`
			SessionID string `json:"session_id"`
			Seq       int    `json:"seq"`
		}
		if err := json.Unmarshal(handshake.D, &resume); err != nil || resume.Token != "fixture-token" || resume.SessionID != "sess-e2e" || resume.Seq != 11 {
			t.Errorf("invalid resume payload: %s", handshake.D)
			return
		}
		mustSendGateway(t, ws, gatewayPayload{Op: 0, T: "RESUMED", D: json.RawMessage(`{}`)})
		seq := 11
		mustSendGateway(t, ws, discordDispatch("MESSAGE_CREATE", seq, "111111111111111111", "first replay"))
		seq = 12
		mustSendGateway(t, ws, discordDispatch("MESSAGE_CREATE", seq, "222222222222222222", "second"))
		mustSendGateway(t, ws, gatewayPayload{Op: 7})
	})

	mux := http.NewServeMux()
	mux.Handle("/ws/", wsHandler)
	mux.HandleFunc("/api/v10/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bot fixture-token" {
			t.Errorf("gateway auth=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": strings.Replace(server.URL, "http://", "ws://", 1) + "/ws"})
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("DUCKWAY_DISCORD_BASE_URL", server.URL+"/api/v10")

	conn := &ccBotConn{apiKeyID: "key1", botToken: "fixture-token", cc: h.cc, hub: h.hub, stopCh: make(chan struct{})}
	if err := conn.connect(); err == nil || !strings.Contains(err.Error(), "reconnect requested") {
		t.Fatalf("first connect err=%v", err)
	}
	if err := conn.connect(); err == nil || !strings.Contains(err.Error(), "reconnect requested") {
		t.Fatalf("resume connect err=%v", err)
	}
	mu.Lock()
	ops := append([]int(nil), receivedOps...)
	mu.Unlock()
	if fmt.Sprint(ops) != "[2 6]" {
		t.Fatalf("handshake ops=%v, want IDENTIFY then RESUME", ops)
	}
	rows, err := h.cc.PullInbox("cc1", 0, []string{"dwch_e2e"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("durable rows=%d, want replay dedup to 2", len(rows))
	}
	want := map[string]string{
		"MESSAGE_CREATE:111111111111111111": "first",
		"MESSAGE_CREATE:222222222222222222": "second",
	}
	for _, row := range rows {
		needle, ok := want[row.EventKey]
		if !ok || !strings.Contains(row.Payload, needle) || row.LaneKey != "dwch_e2e" || row.Status != "admitted" {
			t.Fatalf("unexpected durable row: %+v", row)
		}
		delete(want, row.EventKey)
	}
	if len(want) != 0 {
		t.Fatalf("missing durable events: %v", want)
	}
}

func discordDispatch(event string, seq int, id, content string) gatewayPayload {
	data := fmt.Sprintf(`{"id":%q,"guild_id":"G1","channel_id":"9001","content":%q,"author":{"id":"U1","bot":false}}`, id, content)
	return gatewayPayload{Op: 0, T: event, S: &seq, D: json.RawMessage(data)}
}

func mustSendGateway(t *testing.T, ws *websocket.Conn, payload gatewayPayload) {
	t.Helper()
	if err := websocket.JSON.Send(ws, payload); err != nil {
		t.Errorf("gateway fixture send: %v", err)
	}
}

func TestDiscordProspectiveThreadContinuityE2E(t *testing.T) {
	h := newCommandHarness(t)
	t.Setenv("DUCKWAY_DISCORD_BASE_URL", h.bot.BaseURL)
	conn := &ccBotConn{apiKeyID: "key1", botToken: "fake-token", cc: h.cc, hub: h.hub, commands: h.handler, botUserID: "BOT", stopCh: make(chan struct{})}
	starterID := "333333333333333333"
	starter := json.RawMessage(fmt.Sprintf(`{"id":%q,"guild_id":"G1","channel_id":"MGMT1","content":"research task","author":{"id":"U1","bot":false}}`, starterID))
	conn.routeMessageEvent("MESSAGE_CREATE", "MGMT1", starter)
	conn.routeMessageEvent("MESSAGE_CREATE", "MGMT1", starter) // gateway replay
	thread, err := h.cc.GetChannelByRealID("cc1", starterID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Kind != "task" || thread.Handle != prospectiveThreadHandle("cc1", starterID) {
		t.Fatalf("thread binding=%+v", thread)
	}
	if got := h.hitsFor("POST", "/channels/MGMT1/messages/"+starterID+"/threads"); got != 1 {
		t.Fatalf("thread creates=%d", got)
	}
	followup := json.RawMessage(fmt.Sprintf(`{"id":"444444444444444444","guild_id":"G1","channel_id":%q,"content":"continue","author":{"id":"U1","bot":false}}`, starterID))
	conn.routeMessageEvent("MESSAGE_CREATE", starterID, followup)
	rows, err := h.cc.PullInbox("cc1", 0, []string{thread.Handle}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("thread session rows=%d, want starter+followup", len(rows))
	}
}

func TestDiscordPolicyFailClosedE2E(t *testing.T) {
	h := newCommandHarness(t)
	clientID := "client1"
	if err := h.cc.CreateChannel(&models.CCChannel{Handle: "dwch_policy", CCID: "cc1", ClientID: &clientID,
		ChannelID: "9010", Name: "policy", Kind: "task"}); err != nil {
		t.Fatal(err)
	}
	_, err := h.db.Exec(`UPDATE control_channels SET config=? WHERE id='cc1'`,
		`{"guild_id":"G1","category_id":"CAT1","allowed_user_ids":["U1"],"allowed_role_ids":["R1"],"require_mention":true}`)
	if err != nil {
		t.Fatal(err)
	}
	conn := &ccBotConn{apiKeyID: "key1", cc: h.cc, hub: h.hub, botUserID: "BOT"}
	cases := []struct {
		name, payload string
		allowed       bool
	}{
		{"wrong guild", `{"id":"501","guild_id":"G2","channel_id":"9010","content":"x","author":{"id":"U1"},"member":{"roles":["R1"]},"mentions":[{"id":"BOT"}]}`, false},
		{"legacy user allowlist ignored", `{"id":"502","guild_id":"G1","channel_id":"9010","content":"x","author":{"id":"U2"},"member":{"roles":["R1"]},"mentions":[{"id":"BOT"}]}`, true},
		{"legacy role allowlist ignored", `{"id":"503","guild_id":"G1","channel_id":"9010","content":"x","author":{"id":"U1"},"member":{"roles":["R2"]},"mentions":[{"id":"BOT"}]}`, true},
		{"missing mention", `{"id":"504","guild_id":"G1","channel_id":"9010","content":"@BOT text spoof","author":{"id":"U1"},"member":{"roles":["R1"]}}`, false},
		{"allowed", `{"id":"505","guild_id":"G1","channel_id":"9010","content":"x","author":{"id":"U1"},"member":{"roles":["R1"]},"mentions":[{"id":"BOT"}]}`, true},
		{"control command needs no mention", `{"id":"507","guild_id":"G1","channel_id":"9010","content":"!yield -w","author":{"id":"U2"},"member":{"roles":["R2"]}}`, true},
		{"unknown command still needs mention", `{"id":"508","guild_id":"G1","channel_id":"9010","content":"!future-command","author":{"id":"U2"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := h.cc.LatestInboxID("cc1")
			conn.routeMessageEvent("MESSAGE_CREATE", "9010", json.RawMessage(tc.payload))
			after, _ := h.cc.LatestInboxID("cc1")
			if (after > before) != tc.allowed {
				t.Fatalf("admitted=%v want=%v", after > before, tc.allowed)
			}
		})
	}
	before, _ := h.cc.LatestInboxID("cc1")
	conn.routeMessageEvent("MESSAGE_CREATE", "9010", json.RawMessage(`{"id":"506","guild_id":"G1","channel_id":"9010","content":"!! env","author":{"id":"U1"},"member":{"roles":["R1"]},"mentions":[{"id":"BOT"}]}`))
	after, _ := h.cc.LatestInboxID("cc1")
	if after != before {
		t.Fatal("task-channel direct shell command was admitted")
	}
}

func TestDiscordMentionPolicyIsActivationOnly(t *testing.T) {
	cc := models.ControlChannel{IsActive: true, Config: `{"guild_id":"G1","category_id":"CAT1","require_mention":true}`}
	task := models.CCChannel{Kind: "task"}
	management := models.CCChannel{Kind: "management"}
	tests := []struct {
		name, content string
		channel       models.CCChannel
		mentions      bool
		wantDeny      string
	}{
		{name: "yield", content: "!yield -w", channel: task},
		{name: "status", content: "!status", channel: management},
		{name: "direct shell", content: "!!pwd", channel: management},
		{name: "unknown command", content: "!future-command", channel: task, wantDeny: "mention_required"},
		{name: "ordinary prompt", content: "please continue", channel: task, wantDeny: "mention_required"},
		{name: "mentioned prompt", content: "please continue", channel: task, mentions: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mentions := ""
			if tc.mentions {
				mentions = `,"mentions":[{"id":"BOT"}]`
			}
			payload := json.RawMessage(fmt.Sprintf(`{"id":"%d","guild_id":"G1","channel_id":"C1","content":%q,"author":{"id":"U1"}%s}`, 900+i, tc.content, mentions))
			_, deny := authorizeCCInbound(cc, tc.channel, payload, "BOT")
			if deny != tc.wantDeny {
				t.Fatalf("deny=%q want=%q", deny, tc.wantDeny)
			}
		})
	}
}

func TestDiscordClientCommandUsesDurableInboxE2E(t *testing.T) {
	h := newCommandHarness(t)
	_, unsubscribe := h.hub.Subscribe("client1")
	defer unsubscribe()
	conn := &ccBotConn{apiKeyID: "key1", botToken: "fake-token", cc: h.cc, hub: h.hub, commands: h.handler, botUserID: "BOT", stopCh: make(chan struct{})}
	payload := json.RawMessage(`{"id":"777777777777777777","guild_id":"G1","channel_id":"MGMT1","content":"!new review","author":{"id":"U1","bot":false}}`)
	conn.routeMessageEvent("MESSAGE_CREATE", "MGMT1", payload)
	// Admission is synchronous with Gateway dispatch; it must not depend on
	// the volatile command worker getting scheduled.
	rows, err := h.cc.PullInbox("cc1", 0, []string{"dwch_mgmt"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].EventType != "CLIENT_COMMAND" || rows[0].EventKey != "CLIENT_COMMAND:777777777777777777" || rows[0].Status != "admitted" ||
		!strings.Contains(rows[0].Payload, `"request_id":"777777777777777777"`) || !strings.Contains(rows[0].Payload, `"command":"!new"`) {
		t.Fatalf("durable command=%+v", rows)
	}
	conn.stop()
}

func TestDiscordCommandReplyCannotBlockGatewayAdmissionE2E(t *testing.T) {
	h := newCommandHarness(t)
	_, unsubscribe := h.hub.Subscribe("client1")
	defer unsubscribe()
	release := make(chan struct{})
	started := make(chan struct{})
	conn := &ccBotConn{apiKeyID: "key1", botToken: "fake-token", cc: h.cc, hub: h.hub, commands: h.handler, botUserID: "BOT", stopCh: make(chan struct{}),
		commandHandler: func(context.Context, ccGatewayCommand) { close(started); <-release }}
	// Wrong-scope !yield requires a Discord error reply and therefore remains
	// on the async worker. Block that worker to model a rate-limited REST call.
	conn.routeMessageEvent("MESSAGE_CREATE", "MGMT1", json.RawMessage(`{"id":"7001","guild_id":"G1","channel_id":"MGMT1","content":"!yield","author":{"id":"U1"}}`))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async command worker did not start")
	}
	// A valid daemon command must still be inserted synchronously by the
	// Gateway read path without waiting for that blocked REST work.
	done := make(chan struct{})
	go func() {
		conn.routeMessageEvent("MESSAGE_CREATE", "MGMT1", json.RawMessage(`{"id":"7002","guild_id":"G1","channel_id":"MGMT1","content":"!new next","author":{"id":"U1"}}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		close(release)
		t.Fatal("Gateway dispatch blocked behind command reply")
	}
	rows, err := h.cc.PullInbox("cc1", 0, []string{"dwch_mgmt"}, 10)
	if err != nil || len(rows) != 1 || rows[0].EventKey != "CLIENT_COMMAND:7002" {
		t.Fatalf("durable command rows=%+v err=%v", rows, err)
	}
	close(release)
	conn.stop()
}

func TestDiscordHeartbeatMissingAckClosesSocketE2E(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer close(closed)
		defer ws.Close()
		mustSendGateway(t, ws, gatewayPayload{Op: 10, D: json.RawMessage(`{"heartbeat_interval":15}`)})
		var handshake gatewayPayload
		_ = websocket.JSON.Receive(ws, &handshake)
		var heartbeat gatewayPayload
		_ = websocket.JSON.Receive(ws, &heartbeat)
		var next gatewayPayload
		if err := websocket.JSON.Receive(ws, &next); err == nil {
			t.Errorf("expected client close after missing ACK")
		}
	}))
	defer server.Close()
	ws, err := websocket.Dial(strings.Replace(server.URL, "http://", "ws://", 1), "", "http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	conn := &ccBotConn{apiKeyID: "ack-e2e", ws: ws, hbMs: 15, stopCh: make(chan struct{})}
	done := make(chan struct{})
	go conn.heartbeat(ws, done)
	if err := websocket.JSON.Send(ws, gatewayPayload{Op: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("missing ACK did not close socket")
	}
}
