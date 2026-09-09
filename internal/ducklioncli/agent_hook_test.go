package ducklioncli

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func hookReceiver(t *testing.T) <-chan protocol.SupervisorAgentEvent {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("DUCKLION_AGENT_EVENT_SOCKET", path)
	t.Setenv("DUCKLION_AGENT_EVENT_TOKEN", "test-token")
	events := make(chan protocol.SupervisorAgentEvent, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var envelope struct {
			Token string                        `json:"token"`
			Event protocol.SupervisorAgentEvent `json:"event"`
		}
		if json.NewDecoder(conn).Decode(&envelope) == nil && envelope.Token == "test-token" {
			events <- envelope.Event
			_, _ = conn.Write([]byte("ok\n"))
		}
	}()
	return events
}

func TestRunAgentHookNormalizesClaudeAndCodexPayloads(t *testing.T) {
	for name, input := range map[string]string{
		"claude": `{"last_assistant_message":"claude done"}`,
		"codex":  `{"type":"agent-turn-complete","last-agent-message":"codex done"}`,
	} {
		t.Run(name, func(t *testing.T) {
			events := hookReceiver(t)
			if err := runAgentHook(strings.NewReader(input), nil); err != nil {
				t.Fatal(err)
			}
			event := <-events
			if event.Kind != "completed" || event.Response != name+" done" {
				t.Fatalf("event=%+v", event)
			}
		})
	}
}

func TestRunAgentHookConvertsOversizedResponseToFailure(t *testing.T) {
	events := hookReceiver(t)
	input := `{"last_assistant_message":"` + strings.Repeat("x", protocol.MaxAgentResponseBytes+1) + `"}`
	if err := runAgentHook(strings.NewReader(input), nil); err != nil {
		t.Fatal(err)
	}
	if event := <-events; event.Kind != "failed" {
		t.Fatalf("event=%+v", event)
	}
}
