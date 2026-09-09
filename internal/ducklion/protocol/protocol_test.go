package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNegotiateRequiresMajorRoleAndPrincipal(t *testing.T) {
	local := Handshake{Major: 1, Minor: 3, Capabilities: []string{"attach", "yield"}}
	remote := Handshake{Major: 1, Minor: 2, Role: RoleDucklord, Principal: "laptop", Capabilities: []string{"yield", "unknown"}}
	got, protocolError := Negotiate(local, remote)
	if protocolError != nil || got.Minor != 2 || len(got.Capabilities) != 1 || got.Capabilities[0] != "yield" {
		t.Fatalf("negotiated=%+v error=%+v", got, protocolError)
	}
	remote.Major = 2
	if _, protocolError := Negotiate(local, remote); protocolError == nil || protocolError.Code != ErrIncompatible {
		t.Fatalf("major mismatch error=%+v", protocolError)
	}
}

func TestResponseRequiresExactlyOneOutcome(t *testing.T) {
	for _, response := range []Response{{ID: "1"}, {ID: "1", Result: json.RawMessage(`{}`), Error: &Error{Code: ErrBusy}}} {
		if err := response.Validate(); err == nil {
			t.Fatalf("invalid response accepted: %+v", response)
		}
	}
	if err := (Response{ID: "1", Result: json.RawMessage(`{"ok":true}`)}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionProjectNameJSONCompatibility(t *testing.T) {
	raw, err := json.Marshal(SessionCreate{Handle: "agent", ProjectName: "中文專案"})
	if err != nil || !json.Valid(raw) {
		t.Fatalf("marshal=%q err=%v", raw, err)
	}
	var create SessionCreate
	if err := json.Unmarshal(raw, &create); err != nil || create.ProjectName != "中文專案" {
		t.Fatalf("create=%+v err=%v", create, err)
	}
	create = SessionCreate{}
	if err := json.Unmarshal([]byte(`{"handle":"legacy"}`), &create); err != nil || create.ProjectName != "" {
		t.Fatalf("legacy create=%+v err=%v", create, err)
	}
	empty, err := json.Marshal(SessionSummary{SessionID: "ABC123"})
	if err != nil || bytes.Contains(empty, []byte("project_name")) {
		t.Fatalf("empty summary=%q err=%v", empty, err)
	}
}

func TestHandshakeResponseRequiresExactlyOneOutcome(t *testing.T) {
	if err := (HandshakeResponse{}).Validate(); err == nil {
		t.Fatal("empty handshake response accepted")
	}
	if err := (HandshakeResponse{Handshake: &Handshake{}, Error: &Error{Code: ErrInternal}}).Validate(); err == nil {
		t.Fatal("ambiguous handshake response accepted")
	}
	if err := (HandshakeResponse{Handshake: &Handshake{Major: 1}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNegotiateRejectsNegativeVersion(t *testing.T) {
	local := Handshake{Major: Major, Minor: Minor, Role: RoleSupervisor, Principal: "ABC123", Capabilities: []string{"supervisor_recovery"}}
	remote := local
	remote.Minor = -1
	if _, protocolError := Negotiate(local, remote); protocolError == nil || protocolError.Code != ErrInvalidArgument {
		t.Fatalf("protocol error=%+v", protocolError)
	}
}

func TestNegotiateSupervisorActivityCapability(t *testing.T) {
	local := Handshake{Major: Major, Minor: Minor, Capabilities: []string{"terminal_attention"}}
	remote := Handshake{Major: Major, Minor: Minor, Role: RoleSupervisorActivity, Principal: "ABC123", Capabilities: []string{"terminal_attention", "unknown"}}
	got, protocolError := Negotiate(local, remote)
	if protocolError != nil || got.Role != RoleSupervisorActivity || got.Principal != "ABC123" || len(got.Capabilities) != 1 || got.Capabilities[0] != "terminal_attention" {
		t.Fatalf("negotiated=%+v error=%+v", got, protocolError)
	}
}
