package services

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/models"
)

// TestParseHello_Valid accepts a well-formed Hello and returns the interval.
func TestParseHello_Valid(t *testing.T) {
	got, err := parseHello(gatewayPayload{Op: 10, D: json.RawMessage(`{"heartbeat_interval":41250}`)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 41250 {
		t.Fatalf("interval = %d, want 41250", got)
	}
}

// TestParseHello_Rejects guards every path that would otherwise leave hbMs at 0
// and panic time.NewTicker(0) in the heartbeat goroutine — the suspected cause
// of the :80 listener outage.
func TestParseHello_Rejects(t *testing.T) {
	cases := map[string]gatewayPayload{
		"wrong op":       {Op: 11, D: json.RawMessage(`{"heartbeat_interval":41250}`)},
		"zero interval":  {Op: 10, D: json.RawMessage(`{"heartbeat_interval":0}`)},
		"missing field":  {Op: 10, D: json.RawMessage(`{}`)},
		"null data":      {Op: 10, D: json.RawMessage(`null`)},
		"malformed json": {Op: 10, D: json.RawMessage(`{`)},
		"empty data":     {Op: 10, D: nil},
		"negative":       {Op: 10, D: json.RawMessage(`{"heartbeat_interval":-5}`)},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseHello(p); err == nil {
				t.Fatalf("expected error for %q, got nil", name)
			}
		})
	}
}

// TestRecoverCC confirms a cc-gw goroutine panic is swallowed, not propagated —
// so a malformed Discord frame can never crash the process.
func TestRecoverCC(t *testing.T) {
	didReturn := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped recoverCC: %v", r)
			}
		}()
		func() {
			defer recoverCC("test-key", "unit")
			panic("boom")
		}()
		didReturn = true
	}()
	if !didReturn {
		t.Fatal("expected normal return after recoverCC swallowed the panic")
	}
}

// TestCCReconnectDelay_GrowsOnUnstable verifies that repeated short-lived
// connections back off exponentially (capped), instead of re-identifying every
// fixed interval and tripping Discord's IDENTIFY rate limit.
func TestCCReconnectDelay_GrowsOnUnstable(t *testing.T) {
	min, max, stable := 1*time.Second, 60*time.Second, 60*time.Second
	unstableUptime := 3 * time.Second // like the 13:03 invalid-session storm

	base := min
	wantWaits := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second, // capped
		60 * time.Second, // stays capped
	}
	for i, want := range wantWaits {
		var wait time.Duration
		wait, base = ccReconnectDelay(base, unstableUptime, min, max, stable)
		if wait != want {
			t.Fatalf("attempt %d: wait = %v, want %v", i, wait, want)
		}
	}
}

// TestCCReconnectDelay_ResetsOnStable verifies that a connection that stayed up
// long enough (e.g. a routine op-7 reconnect after 45 min) is not penalized —
// the next wait drops back to the minimum.
func TestCCReconnectDelay_ResetsOnStable(t *testing.T) {
	min, max, stable := 1*time.Second, 60*time.Second, 60*time.Second

	// Drive the backoff up first.
	base := min
	for i := 0; i < 5; i++ {
		_, base = ccReconnectDelay(base, 2*time.Second, min, max, stable)
	}
	if base <= min {
		t.Fatalf("precondition: backoff should have grown, got %v", base)
	}

	// A healthy, long-lived connection now closes — wait must reset to min.
	wait, next := ccReconnectDelay(base, 45*time.Minute, min, max, stable)
	if wait != min {
		t.Fatalf("stable connection: wait = %v, want %v", wait, min)
	}
	if next != 2*time.Second {
		t.Fatalf("stable connection: nextBase = %v, want %v", next, 2*time.Second)
	}
}

// TestIsTransientGatewayErr classifies which /gateway/bot failures getGatewayURL
// should retry in place. The reported outage was a Docker DNS blip — a transport
// error with no HTTP response — which must be treated as transient so it doesn't
// escalate the reconnect backoff. A bad token (4xx) must NOT be retried.
func TestIsTransientGatewayErr(t *testing.T) {
	dnsErr := fmt.Errorf("discord request: %w",
		fmt.Errorf(`Get "https://discord.com/api/v10/gateway/bot": dial tcp: lookup discord.com on 127.0.0.11:53: read udp: connection refused`))

	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":             {nil, false},
		"dns refused":     {dnsErr, true},
		"plain transport": {fmt.Errorf("discord request: EOF"), true},
		"429 rate limit":  {&DiscordError{Status: 429}, true},
		"500 server":      {&DiscordError{Status: 500}, true},
		"503 unavailable": {&DiscordError{Status: 503}, true},
		"401 bad token":   {&DiscordError{Status: 401}, false},
		"403 forbidden":   {&DiscordError{Status: 403}, false},
		"404 not found":   {&DiscordError{Status: 404}, false},
		"wrapped 500":     {fmt.Errorf("lookup: %w", &DiscordError{Status: 500}), true},
		"wrapped 401":     {fmt.Errorf("lookup: %w", &DiscordError{Status: 401}), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isTransientGatewayErr(tc.err); got != tc.want {
				t.Fatalf("isTransientGatewayErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCCReconnectDelay_CapAtMax guards the upper bound directly.
func TestCCReconnectDelay_CapAtMax(t *testing.T) {
	min, max, stable := 1*time.Second, 60*time.Second, 60*time.Second
	_, next := ccReconnectDelay(40*time.Second, 1*time.Second, min, max, stable)
	if next != max {
		t.Fatalf("nextBase = %v, want cap %v", next, max)
	}
}

// TestHandleDispatch_ReadyCapturesSession verifies that a READY event populates
// sessionID and resumeURL on the connection so that the next reconnect can send
// a RESUME (op 6) instead of a fresh IDENTIFY (op 2).
func TestHandleDispatch_ReadyCapturesSession(t *testing.T) {
	c := &ccBotConn{stopCh: make(chan struct{})}
	payload := json.RawMessage(`{
		"session_id":          "abc123",
		"resume_gateway_url":  "wss://gateway.discord.gg"
	}`)
	c.handleDispatch("READY", payload)

	c.mu.Lock()
	sid, rurl := c.sessionID, c.resumeURL
	c.mu.Unlock()

	if sid != "abc123" {
		t.Fatalf("sessionID = %q, want %q", sid, "abc123")
	}
	if rurl != "wss://gateway.discord.gg" {
		t.Fatalf("resumeURL = %q, want %q", rurl, "wss://gateway.discord.gg")
	}
}

// TestHandleDispatch_ReadyIgnoresEmptySessionID verifies that a malformed READY
// (empty session_id) does not overwrite a previously captured valid session,
// guarding against a botched reconnect clearing usable state.
func TestHandleDispatch_ReadyIgnoresEmptySessionID(t *testing.T) {
	c := &ccBotConn{stopCh: make(chan struct{})}
	c.sessionID = "existing"
	c.resumeURL = "wss://existing"

	c.handleDispatch("READY", json.RawMessage(`{"session_id":"","resume_gateway_url":""}`))

	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()

	if sid != "existing" {
		t.Fatalf("sessionID = %q, want existing to be preserved", sid)
	}
}

// TestHandleDispatch_ResumedDoesNotPanic confirms the RESUMED event is handled
// without panicking — no state is mutated, only a log line is emitted.
func TestHandleDispatch_ResumedDoesNotPanic(t *testing.T) {
	c := &ccBotConn{stopCh: make(chan struct{})}
	c.sessionID = "abc123"

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RESUMED dispatch panicked: %v", r)
		}
	}()
	c.handleDispatch("RESUMED", json.RawMessage(`{}`))

	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "abc123" {
		t.Fatalf("RESUMED must not clear sessionID")
	}
}

func TestRouteMessageEventPublishesTaskMessage(t *testing.T) {
	h := newCommandHarness(t)
	task := &models.CCChannel{
		Handle: "dwch_task", CCID: "cc1", ClientID: ptr("client1"),
		ChannelID: "TASK1", Name: "task", Kind: "task",
	}
	if err := h.cc.CreateChannel(task); err != nil {
		t.Fatal(err)
	}
	ccs, err := h.cc.ListByAPIKeyID("key1")
	if err != nil || len(ccs) != 1 || ccs[0].ClientID != "client1" {
		t.Fatalf("ListByAPIKeyID precondition = %+v, %v", ccs, err)
	}
	ch, err := h.cc.GetChannelByRealID("cc1", "TASK1")
	if err != nil || ch == nil || ch.Handle != "dwch_task" {
		t.Fatalf("GetChannelByRealID precondition = %+v, %v", ch, err)
	}

	sub, unsub := h.hub.Subscribe("client1")
	defer unsub()
	conn := &ccBotConn{apiKeyID: "key1", cc: h.cc, hub: h.hub, commands: h.handler}
	payload := json.RawMessage(`{"id":"M1","channel_id":"TASK1","content":"hello codex","author":{"id":"U1","bot":false}}`)
	conn.routeMessageEvent("MESSAGE_CREATE", "TASK1", payload)

	select {
	case ev := <-sub:
		if ev.Type != "message_create" || ev.CCID != "cc1" || ev.Handle != "dwch_task" || ev.Kind != "task" {
			t.Fatalf("event = %+v", ev)
		}
		var msg struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(ev.Payload, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Content != "hello codex" {
			t.Fatalf("event payload content = %q", msg.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published CC event")
	}

	rows, err := h.cc.PullInbox("cc1", 0, []string{"dwch_task"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].EventType != "MESSAGE_CREATE" || rows[0].ChannelHandle == nil || *rows[0].ChannelHandle != "dwch_task" {
		t.Fatalf("inbox rows = %+v", rows)
	}
}

func TestRouteMessageEventCommandsDoNotReachDaemon(t *testing.T) {
	h := newCommandHarness(t)
	sub, unsub := h.hub.Subscribe("client1")
	defer unsub()
	conn := &ccBotConn{apiKeyID: "key1", cc: h.cc, hub: h.hub, commands: h.handler}

	payload := json.RawMessage(`{"id":"M2","channel_id":"MGMT1","content":"!status","author":{"id":"U1","bot":false}}`)
	conn.routeMessageEvent("MESSAGE_CREATE", "MGMT1", payload)

	select {
	case ev := <-sub:
		t.Fatalf("command should not be forwarded to daemon, got event %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	rows, err := h.cc.PullInbox("cc1", 0, []string{"dwch_mgmt"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("command should not be appended to inbox, got %+v", rows)
	}
}
