package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

func TestServerStatusAndSingleInstanceLock(t *testing.T) {
	root := t.TempDir()
	server, err := Open(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	if _, err := Open(context.Background(), Options{Root: root}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second daemon error=%v", err)
	}
	client, err := Dial(server.SocketPath(), "test-terminal")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Call(protocol.Request{ID: "status-1", Type: "status", InstanceID: string(server.InstanceID())})
	client.Close()
	if err != nil || response.Error != nil {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	var result struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil || result.InstanceID != string(server.InstanceID()) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	info, err := os.Stat(server.SocketPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode=%v err=%v", info.Mode(), err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
}

func TestServerRefusesToReplaceNonSocket(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ducklion.sock")
	if err := os.WriteFile(path, []byte("do not replace"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Options{Root: root}); err == nil {
		t.Fatal("daemon replaced a non-socket path")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "do not replace" {
		t.Fatalf("path changed: %q err=%v", data, err)
	}
}

func TestServerRejectsSymlinkLock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "daemon.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Options{Root: root}); err == nil {
		t.Fatal("symlink daemon lock accepted")
	}
}

func TestServerRejectsWrongInstance(t *testing.T) {
	server, err := Open(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-serveErr; err != nil {
			t.Error(err)
		}
	})
	client, err := Dial(server.SocketPath(), "test-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Call(protocol.Request{ID: "status-1", Type: "status", InstanceID: "wrong"})
	if err != nil || response.Error == nil || response.Error.Code != protocol.ErrNotFound {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestSupervisorRecoveryRegistrationIsConnectionBound(t *testing.T) {
	server, err := Open(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveErr
	})
	publicKey, privateKey, err := model.NewRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", CWD: t.TempDir(), Status: model.StatusRecovering,
		Writer: &model.Owner{Kind: model.OwnerCC, ID: "channel"}, OwnershipEpoch: 1, RuntimeGeneration: 2, TaskState: model.TaskIdle,
		AdapterState: model.AdapterRecovering, RecoveryPublicKey: publicKey, CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	if _, _, err := server.service.CreateSession(context.Background(), "cc:channel", "create", session); err != nil {
		t.Fatal(err)
	}
	terminal, err := Dial(server.SocketPath(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := terminal.ListSessions()
	_ = terminal.Close()
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != string(session.ID) || sessions[0].Handle != session.Handle {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	client, err := RegisterSupervisor(server.SocketPath(), session.ID, session.RuntimeGeneration, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	identity := client.Identity()
	runtimeIdentity := duckruntime.RuntimeIdentity{SessionID: session.ID, Generation: identity.RuntimeGeneration, LeaseID: identity.LeaseID}
	if !server.registry.IsCurrent(runtimeIdentity) {
		t.Fatal("registered supervisor is not current")
	}
	if err := client.PublishOutput([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	viewer, err := Dial(server.SocketPath(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := viewer.SubscribeOutput(string(session.ID), session.RuntimeGeneration, 0)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := subscription.Read()
	if err != nil || string(replay.Frame.Data) != "abc" || replay.Frame.Offset != 0 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if err := client.PublishOutput([]byte("def")); err != nil {
		t.Fatal(err)
	}
	live, err := subscription.Read()
	if err != nil || string(live.Frame.Data) != "def" || live.Frame.Offset != 3 {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	if err := subscription.Close(); err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("x"), (1<<20)+12345)
	if err := client.PublishOutput(large); err != nil {
		t.Fatal(err)
	}
	replayViewer, err := Dial(server.SocketPath(), "replay-laptop")
	if err != nil {
		t.Fatal(err)
	}
	largeReplay, err := replayViewer.SubscribeOutput(string(session.ID), session.RuntimeGeneration, 6)
	if err != nil {
		t.Fatal(err)
	}
	var replayed []byte
	for len(replayed) < len(large) {
		event, err := largeReplay.Read()
		if err != nil {
			t.Fatal(err)
		}
		replayed = append(replayed, event.Frame.Data...)
	}
	if !bytes.Equal(replayed, large) {
		t.Fatalf("large replay length=%d want=%d", len(replayed), len(large))
	}
	_ = largeReplay.Close()
	connected, err := server.state.GetSession(context.Background(), session.ID)
	if err != nil || connected.Status != model.StatusRunning || connected.AdapterState != model.AdapterHealthy {
		t.Fatalf("connected session=%+v err=%v", connected, err)
	}
	if _, err := RegisterSupervisor(server.SocketPath(), session.ID, session.RuntimeGeneration, privateKey); err == nil {
		t.Fatal("second supervisor registration succeeded")
	}
	endingViewer, err := Dial(server.SocketPath(), "ending-laptop")
	if err != nil {
		t.Fatal(err)
	}
	ending, err := endingViewer.SubscribeOutput(string(session.ID), session.RuntimeGeneration, uint64(6+len(large)))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = ending.Read()
	var ended *OutputStreamEnded
	if !errors.As(err, &ended) || ended.Reason != "runtime_disconnected" {
		t.Fatalf("stream end error=%v", err)
	}
	_ = ending.Close()
	deadline := time.Now().Add(time.Second)
	for server.registry.IsCurrent(runtimeIdentity) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.registry.IsCurrent(runtimeIdentity) {
		t.Fatal("disconnected supervisor remains current")
	}
	disconnected, err := server.state.GetSession(context.Background(), session.ID)
	if err != nil || disconnected.Status != model.StatusRecovering || disconnected.AdapterState != model.AdapterRecovering {
		t.Fatalf("disconnected session=%+v err=%v", disconnected, err)
	}
}

func TestSupervisorRecoveryRejectsWrongPrivateKey(t *testing.T) {
	server, err := Open(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	defer func() { _ = server.Close(); <-serveErr }()
	publicKey, _, _ := model.NewRecoveryKey()
	_, wrongPrivateKey, _ := model.NewRecoveryKey()
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", CWD: t.TempDir(), Status: model.StatusRecovering,
		Writer: &model.Owner{Kind: model.OwnerCC, ID: "channel"}, OwnershipEpoch: 1, RuntimeGeneration: 2, TaskState: model.TaskIdle,
		AdapterState: model.AdapterRecovering, RecoveryPublicKey: publicKey, CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	if _, _, err := server.service.CreateSession(context.Background(), "cc:channel", "create", session); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterSupervisor(server.SocketPath(), session.ID, session.RuntimeGeneration, wrongPrivateKey); err == nil {
		t.Fatal("wrong recovery key registered")
	}
}
