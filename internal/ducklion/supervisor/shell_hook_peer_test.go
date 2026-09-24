package supervisor

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func TestShellHookPeerHelper(t *testing.T) {
	if os.Getenv("DUCKLION_TEST_SHELL_HOOK_HELPER") != "1" {
		return
	}
	conn, err := net.DialTimeout("unix", os.Getenv("DUCKLION_TEST_SHELL_HOOK_SOCKET"), time.Second)
	if err != nil {
		os.Exit(2)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("hook\n"))
	time.Sleep(100 * time.Millisecond)
}

func TestShellFirstHookEmitter(t *testing.T) {
	if os.Getenv("DUCKLION_TEST_SHELL_HOOK_EMITTER") != "1" {
		return
	}
	if os.Getenv("DUCKLION_AGENT_EVENT_TOKEN") != "" {
		os.Exit(3)
	}
	conn, err := net.DialTimeout("unix", os.Getenv("DUCKLION_AGENT_EVENT_SOCKET"), time.Second)
	if err != nil {
		os.Exit(4)
	}
	defer conn.Close()
	missing, missingErr := net.DialTimeout("unix", os.Getenv("DUCKLION_AGENT_EVENT_SOCKET"), time.Second)
	if missingErr != nil {
		os.Exit(6)
	}
	_ = json.NewEncoder(missing).Encode(agentHookEnvelope{Event: protocol.SupervisorAgentEvent{Kind: "completed"}})
	rejected, _ := io.ReadAll(missing)
	_ = missing.Close()
	if string(rejected) != "rejected\n" {
		os.Exit(7)
	}
	kind := os.Getenv("DUCKLION_TEST_SHELL_HOOK_KIND")
	if kind == "" {
		kind = "completed"
	}
	_ = json.NewEncoder(conn).Encode(agentHookEnvelope{Source: "claude", Event: protocol.SupervisorAgentEvent{Kind: kind, Response: "must not be retained"}})
	response, _ := io.ReadAll(conn)
	if string(response) != "ok\n" {
		os.Exit(5)
	}
}

func TestShellFirstHookIsAdvisoryAndRejectsForeignProcess(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		category model.NotificationCategory
	}{
		{"completed", model.NotificationTaskCompleted},
		{"approval_required", model.NotificationApprovalRequired},
		{"agent_needs_input", model.NotificationAgentNeedsInput},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			testShellFirstHookIsAdvisoryAndRejectsForeignProcess(t, tc.kind, tc.category)
		})
	}
}

func testShellFirstHookIsAdvisoryAndRejectsForeignProcess(t *testing.T, kind string, expected model.NotificationCategory) {
	session, err := Start(Options{SessionID: "ABC123", RuntimeGeneration: 1, OwnershipEpoch: 1, ShellFirstHooks: true,
		CWD: t.TempDir(), Command: []string{"sh", "-c", `DUCKLION_TEST_SHELL_HOOK_KIND="$2" DUCKLION_TEST_SHELL_HOOK_EMITTER=1 "$1" -test.run=^TestShellFirstHookEmitter$; sleep 5`, "sh", os.Args[0], kind}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Terminate(true); _ = session.Wait() }()
	if session.agentHookToken != "" {
		t.Fatal("shell-first session exposed an agent hook bearer token")
	}
	foreign, err := net.DialTimeout("unix", session.agentHookPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewEncoder(foreign).Encode(agentHookEnvelope{Source: "claude", Event: protocol.SupervisorAgentEvent{Kind: kind}})
	answer, _ := io.ReadAll(foreign)
	_ = foreign.Close()
	if string(answer) != "rejected\n" {
		t.Fatalf("foreign process event response = %q", answer)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		category, _, _, ok := session.PendingActivity()
		if ok {
			if category != expected {
				t.Fatalf("shell-first hook category = %s", category)
			}
			session.mu.Lock()
			activeTask := session.activeAgentTask
			session.mu.Unlock()
			if activeTask != "" {
				t.Fatal("advisory hook changed managed task state")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shell-first hook never produced completion activity")
}

func TestShellHookPeerMustBelongToRootProcessTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	command := exec.Command("sh", "-c", `"$1" -test.run=^TestShellHookPeerHelper$; wait`, "sh", os.Args[0])
	command.Env = append(os.Environ(), "DUCKLION_TEST_SHELL_HOOK_HELPER=1", "DUCKLION_TEST_SHELL_HOOK_SOCKET="+path)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	_ = listener.SetDeadline(time.Now().Add(3 * time.Second))
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	_, rootStart, ok := procIdentity(command.Process.Pid)
	if !ok {
		t.Fatal("could not read root process identity")
	}
	if !shellHookPeerIsDescendant(peer, command.Process.Pid, rootStart) {
		t.Fatalf("child helper did not match shell PID %d", command.Process.Pid)
	}
	if shellHookPeerIsDescendant(peer, command.Process.Pid, rootStart+1) {
		t.Fatal("reused root process ID was accepted")
	}
	if shellHookPeerIsDescendant(peer, os.Getpid()+1000000, rootStart) {
		t.Fatal("foreign process tree was accepted")
	}
	if parent, ok := procParentPID(os.Getpid()); !ok || parent <= 0 {
		t.Fatal("could not read process parent PID")
	}
	parent, start, ok := procIdentity(os.Getpid())
	if !ok || parent <= 0 || start == 0 {
		t.Fatal("could not read current process identity")
	}
	parentAgain, startAgain, ok := procIdentity(os.Getpid())
	if !ok || parentAgain != parent || startAgain != start {
		t.Fatal("current process identity was not stable")
	}
	for _, invalidPID := range []int{0, -1, 1 << 30} {
		if _, _, ok := procIdentity(invalidPID); ok {
			t.Fatalf("invalid process ID %d had an identity", invalidPID)
		}
	}
}

func TestShellHookPeerRejectsClosedUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if shellHookPeerIsDescendant(client, os.Getpid(), 1) {
		t.Fatal("closed Unix socket was accepted as a shell-hook peer")
	}
}
