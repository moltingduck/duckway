package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func TestHostRetentionUpdateIsAuthorizedHotAndPersistent(t *testing.T) {
	root := t.TempDir()
	server, err := Open(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	processID := uuid.NewString()
	control, err := DialDucklord(server.SocketPath(), "retention-owner", processID, uuid.NewString(), protocol.ConnectionControl)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(protocol.HostRetentionUpdate{PTYLogRetentionDays: 3})
	request := protocol.Request{ID: uuid.NewString(), Type: "host.retention_update", InstanceID: string(server.instanceID), Body: body}
	for _, bad := range []protocol.Request{
		{ID: uuid.NewString(), Type: request.Type, InstanceID: "wrong", Body: body},
		{ID: uuid.NewString(), Type: request.Type, InstanceID: request.InstanceID, Body: []byte(`{"pty_log_retention_days":0}`)},
		{ID: uuid.NewString(), Type: request.Type, InstanceID: request.InstanceID, Body: []byte(`{"pty_log_retention_days":3,"unknown":1}`)},
	} {
		response, callErr := control.Call(bad)
		if callErr != nil || response.Error == nil || server.retainedTTL() != 7*24*time.Hour {
			t.Fatalf("invalid Host update changed TTL: response=%+v err=%v ttl=%s", response, callErr, server.retainedTTL())
		}
	}
	observer, err := DialDucklord(server.SocketPath(), "retention-owner", processID, uuid.NewString(), protocol.ConnectionObserver)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if response, callErr := observer.Call(request); callErr != nil || response.Error == nil {
		t.Fatalf("observer changed retention: response=%+v err=%v", response, callErr)
	}
	cc, err := DialCC(server.SocketPath(), "dwch_retention")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if response, callErr := cc.Call(request); callErr != nil || response.Error == nil {
		t.Fatalf("CC changed retention: response=%+v err=%v", response, callErr)
	}
	if err := control.SetHostLogRetention(context.Background(), 3); err != nil || server.retainedTTL() != 3*24*time.Hour {
		t.Fatalf("hot retention update failed: err=%v ttl=%s", err, server.retainedTTL())
	}
	_ = control.Close()
	_ = server.Close()
	<-done
	restarted, err := Open(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.retainedTTL() != 3*24*time.Hour {
		t.Fatalf("retention was not persisted: %s", restarted.retainedTTL())
	}
}

func TestHostRetentionSettingsRejectSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"pty_log_retention_days":7}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, hostConfigFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRetainedOutputTTL(root, 7*24*time.Hour); err == nil {
		t.Fatal("host settings symlink was followed")
	}
	if err := saveRetainedOutputDays(root, 3); err == nil {
		t.Fatal("host settings symlink was overwritten")
	}
}

func TestHostRetentionUpdateFailureKeepsActiveTTL(t *testing.T) {
	root := t.TempDir()
	server, err := Open(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"pty_log_retention_days":7}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, hostConfigFilename)); err != nil {
		t.Fatal(err)
	}
	if err := server.setRetainedOutputDays(3); err == nil {
		t.Fatal("symlinked settings unexpectedly accepted")
	}
	if server.retainedTTL() != 7*24*time.Hour {
		t.Fatalf("failed persistence changed active TTL: %s", server.retainedTTL())
	}
}

func TestHostRetentionSettingsRejectPermissiveAndLinkedFiles(t *testing.T) {
	for _, mode := range []os.FileMode{0644, 0600} {
		t.Run(mode.String(), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, hostConfigFilename)
			if err := os.WriteFile(path, []byte(`{"pty_log_retention_days":7}`), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if mode == 0600 {
				if err := os.Link(path, filepath.Join(root, "second-link")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadRetainedOutputTTL(root, 7*24*time.Hour); err == nil {
				t.Fatal("insecure Host settings file was read")
			}
			if err := saveRetainedOutputDays(root, 3); err == nil {
				t.Fatal("insecure Host settings file was replaced")
			}
		})
	}
}
