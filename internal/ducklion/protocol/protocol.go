package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	Major = 1
	Minor = 0
)

type Handshake struct {
	Major        int      `json:"major"`
	Minor        int      `json:"minor"`
	Role         PeerRole `json:"role"`
	Principal    string   `json:"principal,omitempty"`
	Capabilities []string `json:"capabilities"`
}

type HandshakeResponse struct {
	Handshake *Handshake `json:"handshake,omitempty"`
	Error     *Error     `json:"error,omitempty"`
}

func (r HandshakeResponse) Validate() error {
	if (r.Handshake == nil) == (r.Error == nil) {
		return fmt.Errorf("handshake response must contain exactly one outcome")
	}
	return nil
}

type PeerRole string

const (
	RoleDucklord          PeerRole = "ducklord"
	RoleDuckwayCC         PeerRole = "duckway_cc"
	RoleSupervisor        PeerRole = "supervisor"
	RoleSupervisorControl PeerRole = "supervisor_control"
)

type Request struct {
	ID                string          `json:"id"`
	Type              string          `json:"type"`
	InstanceID        string          `json:"instance_id,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	OwnershipEpoch    *uint64         `json:"ownership_epoch,omitempty"`
	RuntimeGeneration *uint64         `json:"runtime_generation,omitempty"`
	Body              json.RawMessage `json:"body,omitempty"`
}

func (r Request) Validate() error {
	if strings.TrimSpace(r.ID) == "" || len(r.ID) > 128 {
		return fmt.Errorf("request id is required and must not exceed 128 bytes")
	}
	if strings.TrimSpace(r.Type) == "" || len(r.Type) > 128 {
		return fmt.Errorf("request type is required and must not exceed 128 bytes")
	}
	return nil
}

type Response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

type Error struct {
	Code              ErrorCode `json:"code"`
	Message           string    `json:"message"`
	Retryable         bool      `json:"retryable"`
	OwnershipEpoch    *uint64   `json:"ownership_epoch,omitempty"`
	RuntimeGeneration *uint64   `json:"runtime_generation,omitempty"`
}

type ErrorCode string

const (
	ErrInvalidArgument     ErrorCode = "invalid_argument"
	ErrNotFound            ErrorCode = "not_found"
	ErrNotOwner            ErrorCode = "not_owner"
	ErrBusy                ErrorCode = "busy"
	ErrTaskActive          ErrorCode = "task_active"
	ErrTaskNotActive       ErrorCode = "task_not_active"
	ErrPendingYield        ErrorCode = "pending_yield_exists"
	ErrStaleEpoch          ErrorCode = "stale_epoch"
	ErrStaleGeneration     ErrorCode = "stale_generation"
	ErrAdapterUnhealthy    ErrorCode = "adapter_unhealthy"
	ErrDraining            ErrorCode = "draining"
	ErrIncompatible        ErrorCode = "incompatible_version"
	ErrIdempotencyConflict ErrorCode = "idempotency_conflict"
	ErrInternal            ErrorCode = "internal"
)

func Negotiate(local, remote Handshake) (Handshake, *Error) {
	if local.Major < 0 || local.Minor < 0 || remote.Major < 0 || remote.Minor < 0 {
		return Handshake{}, &Error{Code: ErrInvalidArgument, Message: "protocol versions must not be negative"}
	}
	if local.Major != remote.Major {
		return Handshake{}, &Error{Code: ErrIncompatible, Message: "protocol major version mismatch"}
	}
	if remote.Role != RoleDucklord && remote.Role != RoleDuckwayCC && remote.Role != RoleSupervisor && remote.Role != RoleSupervisorControl {
		return Handshake{}, &Error{Code: ErrInvalidArgument, Message: "invalid peer role"}
	}
	if strings.TrimSpace(remote.Principal) == "" {
		return Handshake{}, &Error{Code: ErrInvalidArgument, Message: "peer principal is required"}
	}
	available := make(map[string]bool, len(local.Capabilities))
	for _, capability := range local.Capabilities {
		available[capability] = true
	}
	capabilities := make([]string, 0)
	for _, capability := range remote.Capabilities {
		if available[capability] {
			capabilities = append(capabilities, capability)
		}
	}
	minor := local.Minor
	if remote.Minor < minor {
		minor = remote.Minor
	}
	return Handshake{Major: local.Major, Minor: minor, Role: remote.Role, Principal: remote.Principal, Capabilities: capabilities}, nil
}

func (r Response) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("response id is required")
	}
	if (r.Error == nil) == (len(r.Result) == 0) {
		return fmt.Errorf("response must contain exactly one of result or error")
	}
	if r.Error == nil && !json.Valid(r.Result) {
		return fmt.Errorf("response result is invalid JSON")
	}
	return nil
}
