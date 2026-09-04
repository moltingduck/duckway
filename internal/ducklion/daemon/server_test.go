package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
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
