package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// Exercises the installed Ducklion hook helper through a real SSH bridge and
// a shell-first PTY, without contacting Codex or Claude services.
func TestDucklordShellFirstHookContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	handle := fmt.Sprintf("hook-e2e-%d", time.Now().UnixNano())
	invoke := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(runtime, append([]string{"exec", controller, "ducklord"}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("ducklord %v: %v: %s", args, err, output)
		}
		return output
	}
	invoke("start", "--config", "/tmp/e2e-inspector.yaml", "client-a", "--name", handle, "--kind", "shell", "--cwd", "/home/duck/projects/alpha", "--", "sh")
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "stop", "client-a", handle, "--config", "/tmp/e2e-inspector.yaml").CombinedOutput()
	})
	command := `ducklion __ducklion_agent_hook_v1 codex '{"type":"agent-turn-complete","last-assistant-message":"private answer"}'`
	invoke("send", "--config", "/tmp/e2e-inspector.yaml", "client-a", handle, command)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		output := invoke("sessions", "client-a", "--json", "--config", "/tmp/e2e-inspector.yaml")
		var sessions []ducklord.RemoteSession
		if err := json.Unmarshal(output, &sessions); err != nil {
			t.Fatalf("decode sessions: %v", err)
		}
		for _, session := range sessions {
			if session.Name != handle {
				continue
			}
			if session.ActivitySequences[model.NotificationTaskCompleted] == 1 {
				if session.Status != "running" || session.TaskState != string(model.TaskIdle) {
					t.Fatalf("hook affected shell lifecycle: %+v", session)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("shell-first hook helper did not produce a completion notification")
}
