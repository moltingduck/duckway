package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDucklordShellRetirementContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	handle := fmt.Sprintf("retire%x", time.Now().UnixNano()&0xffffff)
	marker := "DUCKLORD_RETAINED_E2E_" + handle
	args := []string{"exec", controller, "ducklord", "start", "client-a", "--name", handle, "--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "sh"}
	if out, err := exec.Command(runtime, args...).CombinedOutput(); err != nil {
		t.Fatalf("start Shell Session: %v (output bytes=%d)", err, len(out))
	}
	var sessionID string
	var generation uint64
	waitE2E(t, 15*time.Second, func() bool {
		for _, item := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
			if item.Name == handle {
				sessionID, generation = item.SessionID, item.RuntimeGeneration
				return sessionID != "" && generation != 0
			}
		}
		return false
	}, func() string { return "created Shell Session did not enter inventory" })
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "send", "client-a", sessionID,
		"printf '"+marker+"\\n'; exit", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("exit root shell: %v (output bytes=%d)", err, len(out))
	}
	waitE2E(t, 15*time.Second, func() bool {
		for _, item := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
			if item.SessionID == sessionID {
				return false
			}
		}
		return true
	}, func() string { return "exited root Shell remained in selectable Session inventory" })
	retained, err := exec.Command(runtime, "exec", controller, "ducklord", "retained", "client-a", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
	if err != nil || !strings.Contains(string(retained), sessionID) {
		t.Fatalf("retained diagnostic index is unavailable: err=%v output bytes=%d", err, len(retained))
	}
	read, err := exec.Command(runtime, "exec", controller, "ducklord", "read-retained", "client-a", sessionID,
		fmt.Sprint(generation), "--lines", "30", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
	if err != nil || !strings.Contains(string(read), marker) {
		t.Fatalf("retained PTY output is unavailable: err=%v output bytes=%d", err, len(read))
	}
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "read-retained", "client-a", sessionID,
		fmt.Sprint(generation+1), "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err == nil {
		t.Fatalf("stale generation read was accepted (output bytes=%d)", len(out))
	}
}

func TestDucklordExplicitShellEndRetainsLogContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	handle := fmt.Sprintf("end%x", time.Now().UnixNano()&0xffffff)
	marker := "DUCKLORD_EXPLICIT_END_" + handle
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "start", "client-a", "--name", handle,
		"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "sh").CombinedOutput(); err != nil {
		t.Fatalf("start Shell Session: %v (output bytes=%d)", err, len(out))
	}
	var sessionID string
	var generation uint64
	waitE2E(t, 15*time.Second, func() bool {
		for _, item := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
			if item.Name == handle {
				sessionID, generation = item.SessionID, item.RuntimeGeneration
				return sessionID != "" && generation != 0
			}
		}
		return false
	}, func() string { return "explicit-End Shell did not enter inventory" })
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "send", "client-a", sessionID,
		"printf '"+marker+"\\n'", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("write Shell diagnostic marker: %v (output bytes=%d)", err, len(out))
	}
	waitE2E(t, 10*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", sessionID,
			"--lines", "30", "--config", "/root/.ducklord/config.yaml").Output()
		return err == nil && strings.Contains(string(out), marker)
	}, func() string { return "diagnostic marker never reached the Shell PTY" })
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "end", "client-a", sessionID,
		"--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("end Shell Session: %v (output bytes=%d)", err, len(out))
	}
	waitE2E(t, 15*time.Second, func() bool {
		for _, item := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
			if item.SessionID == sessionID {
				return false
			}
		}
		return true
	}, func() string { return "explicitly ended Shell remained selectable" })
	retained, err := exec.Command(runtime, "exec", controller, "ducklord", "retained", "client-a", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
	if err != nil || !strings.Contains(string(retained), sessionID) {
		t.Fatalf("explicit End lost diagnostic index: err=%v output bytes=%d", err, len(retained))
	}
	read, err := exec.Command(runtime, "exec", controller, "ducklord", "read-retained", "client-a", sessionID,
		fmt.Sprint(generation), "--lines", "30", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
	if err != nil || !strings.Contains(string(read), marker) {
		t.Fatalf("explicit End lost retained PTY output: err=%v output bytes=%d", err, len(read))
	}
}
