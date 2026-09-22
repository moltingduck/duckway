package daemon

import (
	"context"
	"encoding/json"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"testing"
)

func TestHostResourcesAuthorizationAndFields(t *testing.T) {
	s, err := Open(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	request := protocol.Request{ID: "resources", Type: "host.resources", InstanceID: string(s.instanceID), Body: []byte(`{}`)}
	for _, tc := range []struct {
		name              string
		caps              []string
		role              protocol.PeerRole
		session, instance string
		code              protocol.ErrorCode
	}{
		{"observer", nil, protocol.RoleDucklord, "", request.InstanceID, protocol.ErrNotOwner},
		{"cc", []string{"host_config"}, protocol.RoleDuckwayCC, "", request.InstanceID, protocol.ErrNotOwner},
		{"missing capability", nil, protocol.RoleDucklord, "", request.InstanceID, protocol.ErrNotOwner},
		{"session scoped", []string{"host_config"}, protocol.RoleDucklord, "session", request.InstanceID, protocol.ErrNotOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request
			r.SessionID, r.InstanceID = tc.session, tc.instance
			response := s.route(r, tc.caps, tc.role, "test")
			if response.Error == nil || response.Error.Code != tc.code {
				t.Fatalf("response=%+v", response)
			}
		})
	}
	response := s.route(request, []string{"host_config"}, protocol.RoleDucklord, "test")
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	var status protocol.HostResourceStatus
	if err := json.Unmarshal(response.Result, &status); err != nil {
		t.Fatal(err)
	}
	if status.GOOS == "" || status.GOARCH == "" || status.CPUCount < 1 || status.ManagedSessionCount != 0 || status.ActivePTYCount != 0 {
		t.Fatalf("status=%+v", status)
	}
}
