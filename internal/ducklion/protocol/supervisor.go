package protocol

import "github.com/hackerduck/duckway/internal/ducklion/model"

type SessionSummary struct {
	SessionID         string              `json:"session_id"`
	Handle            string              `json:"handle"`
	Kind              model.SessionKind   `json:"kind"`
	AgentType         string              `json:"agent_type,omitempty"`
	CWD               string              `json:"cwd"`
	Status            model.SessionStatus `json:"status"`
	Writer            *model.Owner        `json:"writer,omitempty"`
	OwnershipEpoch    uint64              `json:"ownership_epoch"`
	RuntimeGeneration uint64              `json:"runtime_generation"`
	TaskState         model.TaskState     `json:"task_state"`
	AdapterState      model.AdapterState  `json:"adapter_state"`
}

type SupervisorChallenge struct {
	ChallengeID string `json:"challenge_id"`
	Nonce       []byte `json:"nonce"`
	InstanceID  string `json:"instance_id"`
}

type SupervisorRegisterComplete struct {
	ChallengeID string `json:"challenge_id"`
	Proof       []byte `json:"proof"`
}

type SupervisorRegistered struct {
	SessionID         string `json:"session_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
	LeaseID           string `json:"lease_id"`
}

type SupervisorOutput struct {
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data"`
}

type SupervisorOutputAck struct {
	Offset uint64 `json:"offset"`
	Length uint64 `json:"length"`
}

type SupervisorInput struct {
	Sequence uint64      `json:"sequence"`
	Owner    model.Owner `json:"owner"`
	Data     []byte      `json:"data"`
}

type SupervisorResize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type SupervisorControlReady struct {
	SessionID         string `json:"session_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
}

type SessionInput struct {
	Data []byte `json:"data"`
}

type SessionResize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type OutputSubscribe struct {
	Offset uint64 `json:"offset"`
}

type OutputSubscribeResult struct {
	SubscriptionID    string `json:"subscription_id"`
	RuntimeID         string `json:"runtime_id"`
	InstanceID        string `json:"instance_id"`
	SessionID         string `json:"session_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
	StartOffset       uint64 `json:"start_offset"`
	EndOffset         uint64 `json:"end_offset"`
	Gap               bool   `json:"gap,omitempty"`
}

// modelOutputFrame mirrors the runtime frame without importing runtime into
// the wire package (runtime already depends on protocol-adjacent model types).
type OutputFrame struct {
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data,omitempty"`
	Gap    bool   `json:"gap,omitempty"`
}

type OutputEvent struct {
	Type              string      `json:"type"`
	SubscriptionID    string      `json:"subscription_id"`
	RuntimeID         string      `json:"runtime_id"`
	InstanceID        string      `json:"instance_id"`
	SessionID         string      `json:"session_id"`
	RuntimeGeneration uint64      `json:"runtime_generation"`
	Frame             OutputFrame `json:"frame"`
	Reason            string      `json:"reason,omitempty"`
}
