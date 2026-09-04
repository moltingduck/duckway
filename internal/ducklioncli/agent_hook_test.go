package ducklioncli

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func TestRunAgentHookNormalizesClaudeAndCodexPayloads(t *testing.T) {
	for name, input := range map[string]string{
		"claude": `{"last_assistant_message":"claude done"}`,
		"codex":  `{"type":"agent-turn-complete","last-agent-message":"codex done"}`,
	} {
		t.Run(name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			t.Setenv("DUCKLION_AGENT_EVENT_FD", strconv.Itoa(int(writer.Fd())))
			if err := runAgentHook(strings.NewReader(input), nil); err != nil {
				t.Fatal(err)
			}
			_ = writer.Close()
			data := make([]byte, 1024)
			n, err := reader.Read(data)
			if err != nil || !strings.Contains(string(data[:n]), `"kind":"completed"`) || !strings.Contains(string(data[:n]), name+" done") {
				t.Fatalf("event=%q err=%v", data[:n], err)
			}
		})
	}
}

func TestRunAgentHookConvertsOversizedResponseToFailure(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	t.Setenv("DUCKLION_AGENT_EVENT_FD", strconv.Itoa(int(writer.Fd())))
	input := `{"last_assistant_message":"` + strings.Repeat("x", protocol.MaxAgentResponseBytes+1) + `"}`
	if err := runAgentHook(strings.NewReader(input), nil); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	data, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(data), `"kind":"failed"`) {
		t.Fatalf("event=%q err=%v", data, err)
	}
}
