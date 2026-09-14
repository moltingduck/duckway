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

func TestHostAgentHookConfigRejectsUnauthorizedAndMalformedRequests(t *testing.T) {
	server, err := Open(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	valid := []byte(`{"agent":"codex","action":"install"}`)
	request := protocol.Request{ID: uuid.NewString(), Type: "host.agent_hook_config", InstanceID: string(server.instanceID), Body: valid}
	for _, tc := range []struct {
		name         string
		request      protocol.Request
		capabilities []string
		role         protocol.PeerRole
		wantCode     protocol.ErrorCode
	}{
		{name: "observer", request: request, role: protocol.RoleDucklord, wantCode: protocol.ErrNotOwner},
		{name: "cc", request: request, capabilities: []string{"host_config"}, role: protocol.RoleDuckwayCC, wantCode: protocol.ErrNotOwner},
		{name: "wrong instance", request: protocol.Request{ID: request.ID, Type: request.Type, InstanceID: "wrong", Body: valid}, capabilities: []string{"host_config"}, role: protocol.RoleDucklord, wantCode: protocol.ErrNotFound},
		{name: "missing instance", request: protocol.Request{ID: request.ID, Type: request.Type, Body: valid}, capabilities: []string{"host_config"}, role: protocol.RoleDucklord, wantCode: protocol.ErrNotOwner},
		{name: "session scoped", request: protocol.Request{ID: request.ID, Type: request.Type, InstanceID: request.InstanceID, SessionID: uuid.NewString(), Body: valid}, capabilities: []string{"host_config"}, role: protocol.RoleDucklord, wantCode: protocol.ErrNotOwner},
		{name: "unknown field", request: protocol.Request{ID: request.ID, Type: request.Type, InstanceID: request.InstanceID, Body: []byte(`{"agent":"codex","action":"install","path":"/tmp/hijack"}`)}, capabilities: []string{"host_config"}, role: protocol.RoleDucklord, wantCode: protocol.ErrInvalidArgument},
		{name: "unknown agent", request: protocol.Request{ID: request.ID, Type: request.Type, InstanceID: request.InstanceID, Body: []byte(`{"agent":"other","action":"install"}`)}, capabilities: []string{"host_config"}, role: protocol.RoleDucklord, wantCode: protocol.ErrInvalidArgument},
		{name: "unknown action", request: protocol.Request{ID: request.ID, Type: request.Type, InstanceID: request.InstanceID, Body: []byte(`{"agent":"claude","action":"toggle"}`)}, capabilities: []string{"host_config"}, role: protocol.RoleDucklord, wantCode: protocol.ErrInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := server.route(tc.request, tc.capabilities, tc.role, "test")
			if response.Error == nil || response.Error.Code != tc.wantCode {
				t.Fatalf("response=%+v want %s", response, tc.wantCode)
			}
		})
	}
	server.maintenanceMu.Lock()
	server.maintenance = true
	server.maintenanceMu.Unlock()
	response := server.route(request, []string{"host_config"}, protocol.RoleDucklord, "test")
	if response.Error == nil || response.Error.Code != protocol.ErrDraining {
		t.Fatalf("maintenance did not reject Host hook update: %+v", response)
	}
}

func TestHostAgentHookConfigControlRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server, err := Open(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() {
		_ = server.Close()
		<-done
	}()
	processID := uuid.NewString()
	control, err := DialDucklord(server.SocketPath(), "hook-owner", processID, uuid.NewString(), protocol.ConnectionControl)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	observer, err := DialDucklord(server.SocketPath(), "hook-owner", processID, uuid.NewString(), protocol.ConnectionObserver)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if _, err := observer.ConfigureHostAgentHook(context.Background(), "codex", "install"); err == nil {
		t.Fatal("observer should not have Host configuration capability")
	}
	result, err := control.ConfigureHostAgentHook(context.Background(), "codex", "install")
	if err != nil || !result.Installed || !result.Changed || result.Activation != "pending" {
		t.Fatalf("install result=%+v err=%v", result, err)
	}
	result, err = control.ConfigureHostAgentHook(context.Background(), "codex", "install")
	if err != nil || !result.Installed || result.Changed {
		t.Fatalf("idempotent install result=%+v err=%v", result, err)
	}
	result, err = control.ConfigureHostAgentHook(context.Background(), "codex", "remove")
	if err != nil || result.Installed || !result.Changed {
		t.Fatalf("remove result=%+v err=%v", result, err)
	}
}
