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
