package services

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// A button press delivered as INTERACTION_CREATE over a real gateway
// websocket handshake is acknowledged, decided and the message edited, against
// an in-process Discord fixture.
func TestDiscordInteractionCreateOverGatewayE2E(t *testing.T) {
	var server *httptest.Server
	var mu sync.Mutex
	var seen []string
	bodies := map[string]string{}
	edited := make(chan struct{})
	var once sync.Once

	ws := websocket.Handler(func(c *websocket.Conn) {
		defer c.Close()
		mustSendGateway(t, c, gatewayPayload{Op: 10, D: json.RawMessage(`{"heartbeat_interval":10000}`)})
		var identify gatewayPayload
		if err := websocket.JSON.Receive(c, &identify); err != nil || identify.Op != 2 {
			t.Errorf("identify: %v op=%d", err, identify.Op)
			return
		}
		seq := 1
		mustSendGateway(t, c, gatewayPayload{Op: 0, T: "READY", S: &seq, D: json.RawMessage(`{"session_id":"s1","user":{"id":"BOT"},"application":{"id":"app1"}}`)})
		seq = 2
		press := `{"id":"int1","token":"tok1","application_id":"app1","type":3,"channel_id":"chan1",
			"data":{"custom_id":"dwa:a:ap1","component_type":2},
			"member":{"user":{"id":"42","username":"pat"},"roles":["r1"]},"message":{"id":"msg9"}}`
		mustSendGateway(t, c, gatewayPayload{Op: 0, T: "INTERACTION_CREATE", S: &seq, D: json.RawMessage(press)})
		select {
		case <-edited:
		case <-time.After(5 * time.Second):
			t.Error("message never edited")
		}
		mustSendGateway(t, c, gatewayPayload{Op: 7})
	})
	mux := http.NewServeMux()
	mux.Handle("/ws/", ws)
	mux.HandleFunc("/api/v10/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"url": strings.Replace(server.URL, "http://", "ws://", 1) + "/ws"})
	})
	mux.HandleFunc("/api/v10/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		seen = append(seen, key)
		bodies[key] = string(b)
		mu.Unlock()
		if r.Method == "PATCH" {
			once.Do(func() { close(edited) })
		}
		_, _ = w.Write([]byte(`{}`))
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("DUCKWAY_DISCORD_BASE_URL", server.URL+"/api/v10")

	var got decisionCall
	var gotMu sync.Mutex
	g := NewDiscordGateway("fixture-token", "chan1", nil, nil).WithDecider(func(id string, approve bool, minutes int, a ChatActor) error {
		gotMu.Lock()
		defer gotMu.Unlock()
		got = decisionCall{id, approve, minutes, a}
		return nil
	})
	defer g.Stop()
	if err := g.connect(); err == nil || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("connect err=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	gotMu.Lock()
	defer gotMu.Unlock()
	if len(seen) < 2 || seen[0] != "POST /api/v10/interactions/int1/tok1/callback" {
		t.Fatalf("REST calls (the ack must come first): %v", seen)
	}
	if !strings.Contains(bodies["POST /api/v10/interactions/int1/tok1/callback"], `"type":6`) {
		t.Fatalf("ack body: %s", bodies["POST /api/v10/interactions/int1/tok1/callback"])
	}
	if got.id != "ap1" || !got.approve || got.actor.UserID != "42" || strings.Join(got.actor.RoleIDs, ",") != "r1" {
		t.Fatalf("decision: %+v", got)
	}
	var edit struct {
		Content    string `json:"content"`
		Components []any  `json:"components"`
	}
	if err := json.Unmarshal([]byte(bodies["PATCH /api/v10/channels/chan1/messages/msg9"]), &edit); err != nil ||
		!strings.Contains(edit.Content, "Approved by <@42>") || edit.Components == nil || len(edit.Components) != 0 {
		t.Fatalf("edit: %+v %v", edit, err)
	}
}
