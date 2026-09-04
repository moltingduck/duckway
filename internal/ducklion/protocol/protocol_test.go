package protocol

import (
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
