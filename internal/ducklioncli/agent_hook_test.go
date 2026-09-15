package ducklioncli

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func hookReceiver(t *testing.T) <-chan protocol.SupervisorAgentEvent {
	return hookReceiverWithToken(t, "test-token")
}

func hookReceiverWithToken(t *testing.T, token string) <-chan protocol.SupervisorAgentEvent {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("DUCKLION_AGENT_EVENT_SOCKET", path)
	t.Setenv("DUCKLION_AGENT_EVENT_TOKEN", token)
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
		if json.NewDecoder(conn).Decode(&envelope) == nil && envelope.Token == token {
			events <- envelope.Event
			_, _ = conn.Write([]byte("ok\n"))
		}
	}()
	return events
}

func TestRunAgentHookShellFirstSendsPayloadFreeAdvisoryEvent(t *testing.T) {
	events := hookReceiverWithToken(t, "")
	if err := runAgentHook(strings.NewReader(`{"type":"agent-turn-complete","last-assistant-message":"secret answer"}`), []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	event := waitHookEvent(t, events)
	if event.Kind != "completed" || event.Response != "" || event.Summary != "" {
		t.Fatalf("shell-first hook leaked agent payload: %+v", event)
	}
}

func TestRunAgentHookActionNeededIsAlwaysPayloadFree(t *testing.T) {
	for _, token := range []string{"", "test-token"} {
		for _, tc := range []struct{ source, hook, notification, kind string }{
			{"codex", "PermissionRequest", "", "approval_required"},
			{"claude", "PermissionRequest", "", "approval_required"},
			{"claude", "Notification", "permission_prompt", "approval_required"},
			{"claude", "Notification", "idle_prompt", "agent_needs_input"},
			{"claude", "Notification", "elicitation_dialog", "agent_needs_input"},
			{"claude", "Notification", "elicitation_url_dialog", "agent_needs_input"},
			{"claude", "Notification", "agent_needs_input", "agent_needs_input"},
		} {
			t.Run(token+tc.source+tc.hook+tc.notification, func(t *testing.T) {
				events := hookReceiverWithToken(t, token)
				input, _ := json.Marshal(map[string]string{"hook_event_name": tc.hook, "notification_type": tc.notification, "last_assistant_message": "secret", "error": "secret", "message": "secret"})
				if err := runAgentHook(strings.NewReader(string(input)), []string{tc.source}); err != nil {
					t.Fatal(err)
				}
				if event := waitHookEvent(t, events); event.Kind != tc.kind || event.Response != "" || event.Summary != "" || event.TaskID != "" {
					t.Fatalf("action-needed event = %+v", event)
				}
			})
		}
	}
}

func TestRunAgentHookIgnoresUnrelatedClaudeNotifications(t *testing.T) {
	for _, kind := range []string{"auth_success", "elicitation_complete", "agent_completed", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			events := hookReceiverWithToken(t, "")
			if err := runAgentHook(strings.NewReader(`{"hook_event_name":"Notification","notification_type":"`+kind+`"}`), []string{"claude"}); err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-events:
				t.Fatalf("unrelated notification emitted %+v", event)
			default:
			}
		})
	}
}

func TestRunAgentHookShellFirstEmptySuccessfulTurnIsCompleted(t *testing.T) {
	events := hookReceiverWithToken(t, "")
	if err := runAgentHook(strings.NewReader(`{"hook_event_name":"Stop"}`), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if event := waitHookEvent(t, events); event.Kind != "completed" || event.Response != "" || event.Summary != "" {
		t.Fatalf("empty successful shell-first turn = %+v", event)
	}
}

func waitHookEvent(t *testing.T, events <-chan protocol.SupervisorAgentEvent) protocol.SupervisorAgentEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("hook receiver timed out")
		return protocol.SupervisorAgentEvent{}
	}
}

func TestRunAgentHookNormalizesClaudeAndCodexPayloads(t *testing.T) {
	for name, test := range map[string]struct{ source, input string }{
		"claude":        {"claude", `{"hook_event_name":"Stop","last_assistant_message":"claude done"}`},
		"codex-current": {"codex", `{"type":"agent-turn-complete","last-assistant-message":"codex-current done"}`},
		"codex-legacy":  {"codex", `{"type":"agent-turn-complete","last-agent-message":"codex-legacy done"}`},
		"codex-stop":    {"codex", `{"hook_event_name":"Stop","last_assistant_message":"codex-stop done"}`},
	} {
		t.Run(name, func(t *testing.T) {
			events := hookReceiver(t)
			if err := runAgentHook(strings.NewReader(test.input), []string{test.source}); err != nil {
				t.Fatal(err)
			}
			event := waitHookEvent(t, events)
			if event.Kind != "completed" || event.Response != name+" done" {
				t.Fatalf("event=%+v", event)
			}
		})
	}
}

func TestRunAgentHookConvertsOversizedResponseToFailure(t *testing.T) {
	events := hookReceiver(t)
	input := `{"hook_event_name":"Stop","last_assistant_message":"` + strings.Repeat("x", protocol.MaxAgentResponseBytes+1) + `"}`
	if err := runAgentHook(strings.NewReader(input), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if event := waitHookEvent(t, events); event.Kind != "failed" {
		t.Fatalf("event=%+v", event)
	}
}

func TestRunAgentHookMapsClaudeStopFailure(t *testing.T) {
	events := hookReceiver(t)
	if err := runAgentHook(strings.NewReader(`{"hook_event_name":"StopFailure","last_assistant_message":"partial"}`), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if event := waitHookEvent(t, events); event.Kind != "failed" || event.Response != "" {
		t.Fatalf("event=%+v", event)
	}
}

func TestRunAgentHookRejectsMissingSourceAndUnknownEvents(t *testing.T) {
	for name, test := range map[string]struct{ source, input string }{
		"missing source":       {"", `{"hook_event_name":"Stop","last_assistant_message":"partial"}`},
		"unknown claude event": {"claude", `{"hook_event_name":"PreToolUse","last_assistant_message":"partial"}`},
		"unknown codex event":  {"codex", `{"type":"future-event","last-assistant-message":"partial"}`},
	} {
		t.Run(name, func(t *testing.T) {
			var args []string
			if test.source != "" {
				args = []string{test.source}
			}
			if err := runAgentHook(strings.NewReader(test.input), args); err == nil {
				t.Fatal("unsupported hook event was accepted")
			}
		})
	}
}

func TestTruncateUTF8BytesPreservesCharacters(t *testing.T) {
	got := truncateUTF8Bytes(strings.Repeat("a", 499)+"界", 500)
	if !strings.HasSuffix(got, "a") || len(got) != 499 {
		t.Fatalf("truncateUTF8Bytes returned %q (%d bytes)", got, len(got))
	}
}

func TestRunAgentHookReportsServerRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DUCKLION_AGENT_EVENT_SOCKET", path)
	t.Setenv("DUCKLION_AGENT_EVENT_TOKEN", "wrong-token")
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer conn.Close()
			var envelope any
			_ = json.NewDecoder(conn).Decode(&envelope)
			_, _ = conn.Write([]byte("rejected\n"))
		}
	}()
	err = runAgentHook(strings.NewReader(`{"type":"agent-turn-complete","last-assistant-message":"done"}`), []string{"codex"})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("rejection error=%v", err)
	}
}
