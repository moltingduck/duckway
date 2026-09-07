package protocol

import (
	"fmt"
	"strings"
)

type SessionLifecycleOperation string

const (
	SessionLifecycleEnd     SessionLifecycleOperation = "end"
	SessionLifecycleDestroy SessionLifecycleOperation = "destroy"
	SessionLifecycleRestart SessionLifecycleOperation = "restart"
)

type SessionLifecycleMode string

const (
	SessionLifecycleImmediate SessionLifecycleMode = "immediate"
	SessionLifecycleWait      SessionLifecycleMode = "wait"
	SessionLifecycleForce     SessionLifecycleMode = "force"
)

type SessionLifecycleRequest struct {
	Operation SessionLifecycleOperation `json:"operation"`
	Mode      SessionLifecycleMode      `json:"mode"`
}

func (r SessionLifecycleRequest) Validate() error {
	if r.Operation != SessionLifecycleEnd && r.Operation != SessionLifecycleDestroy && r.Operation != SessionLifecycleRestart {
		return fmt.Errorf("invalid session lifecycle operation")
	}
	if r.Mode != SessionLifecycleImmediate && r.Mode != SessionLifecycleWait && r.Mode != SessionLifecycleForce {
		return fmt.Errorf("invalid session lifecycle mode")
	}
	return nil
}

type SessionLifecycleState string

const (
	SessionLifecycleWaiting   SessionLifecycleState = "waiting"
	SessionLifecycleExecuting SessionLifecycleState = "executing"
	SessionLifecycleCompleted SessionLifecycleState = "completed"
)

type SessionLifecycleResult struct {
	SessionID         string                    `json:"session_id"`
	Operation         SessionLifecycleOperation `json:"operation"`
	Mode              SessionLifecycleMode      `json:"mode"`
	State             SessionLifecycleState     `json:"state"`
	OwnershipEpoch    uint64                    `json:"ownership_epoch"`
	RuntimeGeneration uint64                    `json:"runtime_generation"`
}

func (r SessionLifecycleResult) Validate() error {
	if strings.TrimSpace(r.SessionID) == "" {
		return fmt.Errorf("session lifecycle result requires a session id")
	}
	if err := (SessionLifecycleRequest{Operation: r.Operation, Mode: r.Mode}).Validate(); err != nil {
		return err
	}
	if r.State != SessionLifecycleWaiting && r.State != SessionLifecycleExecuting && r.State != SessionLifecycleCompleted {
		return fmt.Errorf("invalid session lifecycle state")
	}
	if r.OwnershipEpoch == 0 || r.RuntimeGeneration == 0 {
		return fmt.Errorf("session lifecycle result requires ownership and runtime fences")
	}
	return nil
}
