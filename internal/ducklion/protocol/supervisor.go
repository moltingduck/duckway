package protocol

import "github.com/hackerduck/duckway/internal/ducklion/model"

const MaxAgentPromptBytes = 1 << 20
const MaxAgentResponseBytes = 256 << 10

type SessionSummary struct {
	SessionID             string                                `json:"session_id"`
	Handle                string                                `json:"handle"`
	Kind                  model.SessionKind                     `json:"kind"`
	AgentType             string                                `json:"agent_type,omitempty"`
	CWD                   string                                `json:"cwd"`
	Status                model.SessionStatus                   `json:"status"`
	Writer                *model.Owner                          `json:"writer,omitempty"`
	OwnershipEpoch        uint64                                `json:"ownership_epoch"`
	RuntimeGeneration     uint64                                `json:"runtime_generation"`
	TaskState             model.TaskState                       `json:"task_state"`
	AdapterState          model.AdapterState                    `json:"adapter_state"`
	ExitSuccess           *bool                                 `json:"exit_success,omitempty"`
	ExitReason            string                                `json:"exit_reason,omitempty"`
	ChannelHandle         string                                `json:"channel_handle,omitempty"`
	ManagementHandle      string                                `json:"management_handle,omitempty"`
	ActivitySequences     map[model.NotificationCategory]uint64 `json:"activity_sequences,omitempty"`
	RetainedOutputBytes   int64                                 `json:"retained_output_bytes,omitempty"`
	RetainedOutputUntilMS int64                                 `json:"retained_output_until_ms,omitempty"`
}

type SessionCreate struct {
	Handle    string            `json:"handle"`
	Kind      model.SessionKind `json:"kind"`
	AgentType string            `json:"agent_type,omitempty"`
	CWD       string            `json:"cwd"`
	Command   []string          `json:"command"`
	Rows      uint16            `json:"rows,omitempty"`
	Cols      uint16            `json:"cols,omitempty"`
}

type SupervisorChallenge struct {
	ChallengeID string `json:"challenge_id"`
	Nonce       []byte `json:"nonce"`
	InstanceID  string `json:"instance_id"`
}

type SupervisorRegisterComplete struct {
	ChallengeID string `json:"challenge_id"`
	Proof       []byte `json:"proof"`
	// LaunchFailure is set only when the wrapper authenticated successfully but
	// could not create the child PTY. It never registers a live runtime.
	LaunchFailure string `json:"launch_failure,omitempty"`
}

type SupervisorRegistered struct {
	SessionID         string `json:"session_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
	LeaseID           string `json:"lease_id"`
}

type SupervisorOutput struct {
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data"`
	Gap    bool   `json:"gap,omitempty"`
}

type SupervisorOutputAck struct {
	Offset uint64 `json:"offset"`
	Length uint64 `json:"length"`
}

type SupervisorActivityReceipt struct {
	Category     model.NotificationCategory `json:"category"`
	Sequence     uint64                     `json:"sequence"`
	Advanced     bool                       `json:"advanced"`
	OutputOffset uint64                     `json:"output_offset"`
	EventID      uint64                     `json:"event_id,omitempty"`
}

type SupervisorActivity struct {
	OutputOffset uint64                     `json:"output_offset"`
	Category     model.NotificationCategory `json:"category,omitempty"`
	EventID      uint64                     `json:"event_id,omitempty"`
}

type SupervisorTerminalAttention = SupervisorActivity

type SupervisorInput struct {
	Sequence uint64      `json:"sequence"`
	Owner    model.Owner `json:"owner"`
	Data     []byte      `json:"data"`
}

type SupervisorResize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type SessionResizeResult struct {
	OutputOffset uint64 `json:"output_offset"`
}

type SupervisorExit struct {
	Success bool   `json:"success"`
	Reason  string `json:"reason,omitempty"`
}

type SupervisorControlReady struct {
	SessionID         string `json:"session_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
}

type SessionInput struct {
	Data []byte `json:"data"`
}

// AgentTaskSubmit is the public CC request. TaskID is stable across durable
// inbox retries; PromptDigest lets Ducklion persist idempotency metadata
// without persisting the prompt itself.
type AgentTaskSubmit struct {
	TaskID       string   `json:"task_id"`
	Prompt       []byte   `json:"prompt"`
	PromptDigest [32]byte `json:"prompt_digest"`
}

type SupervisorAgentPrepare struct {
	TaskID       string      `json:"task_id"`
	Prompt       []byte      `json:"prompt"`
	PromptDigest [32]byte    `json:"prompt_digest"`
	Owner        model.Owner `json:"owner"`
}

type SupervisorAgentCommit struct {
	TaskID       string      `json:"task_id"`
	PromptDigest [32]byte    `json:"prompt_digest"`
	Owner        model.Owner `json:"owner"`
}

type SupervisorAgentAbort struct {
	TaskID string `json:"task_id"`
}

type SupervisorAgentStatus struct {
	TaskID       string   `json:"task_id"`
	PromptDigest [32]byte `json:"prompt_digest"`
}

type SupervisorAgentStatusResult struct {
	Status string `json:"status"`
}

type AgentTaskState struct {
	SessionID         string      `json:"session_id"`
	TaskID            string      `json:"task_id"`
	Status            string      `json:"status"`
	OwnershipEpoch    uint64      `json:"ownership_epoch"`
	RuntimeGeneration uint64      `json:"runtime_generation"`
	Writer            model.Owner `json:"writer"`
	OutputStart       uint64      `json:"output_start"`
}

type SupervisorAgentEvent struct {
	TaskID    string `json:"task_id"`
	Sequence  uint64 `json:"sequence"`
	Kind      string `json:"kind"` // progress | completed | failed
	Summary   string `json:"summary,omitempty"`
	Response  string `json:"response,omitempty"`
	OutputEnd uint64 `json:"output_end,omitempty"`
}

type SupervisorAgentEventReceipt struct {
	Recorded     bool `json:"recorded"`
	Acknowledged bool `json:"acknowledged,omitempty"`
}

type AgentTaskEventsRequest struct {
	TaskID        string `json:"task_id"`
	AfterSequence uint64 `json:"after_sequence,omitempty"`
}

type AgentTaskEventsResult struct {
	Events        []SupervisorAgentEvent `json:"events"`
	Status        string                 `json:"status"`
	FirstSequence uint64                 `json:"first_sequence,omitempty"`
	LastSequence  uint64                 `json:"last_sequence,omitempty"`
	AckedSequence uint64                 `json:"acked_sequence,omitempty"`
	Gap           bool                   `json:"gap,omitempty"`
}

type AgentTaskEventAck struct {
	TaskID   string `json:"task_id"`
	Sequence uint64 `json:"sequence"`
}

type SessionResize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type SessionYield struct {
	Wait bool `json:"wait,omitempty"`
}

type SessionYieldResult struct {
	Decision       model.YieldDecision `json:"decision"`
	SessionID      string              `json:"session_id"`
	OwnershipEpoch uint64              `json:"ownership_epoch"`
	Writer         *model.Owner        `json:"writer,omitempty"`
}

type SessionTaskResult struct {
	SessionID      string          `json:"session_id"`
	OwnershipEpoch uint64          `json:"ownership_epoch"`
	TaskState      model.TaskState `json:"task_state"`
	Writer         *model.Owner    `json:"writer,omitempty"`
}

type SessionBind struct {
	ChannelHandle string `json:"channel_handle"`
}

type SessionBinding struct {
	SessionID        string `json:"session_id"`
	ChannelHandle    string `json:"channel_handle"`
	ManagementHandle string `json:"management_handle"`
}

type OutputSubscribe struct {
	Offset    uint64 `json:"offset"`
	TailBytes uint64 `json:"tail_bytes,omitempty"`
}

type OutputUnsubscribe struct {
	SubscriptionID string `json:"subscription_id"`
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

type SessionEventsSubscribe struct {
	AfterRevision uint64 `json:"after_revision,omitempty"`
}

type SessionEventsUnsubscribe struct {
	SubscriptionID string `json:"subscription_id"`
}

type SessionSnapshotSubscribeResult struct {
	SubscriptionID   string           `json:"subscription_id"`
	InstanceID       string           `json:"instance_id"`
	SnapshotRevision uint64           `json:"snapshot_revision"`
	EarliestRevision uint64           `json:"earliest_revision,omitempty"`
	Gap              bool             `json:"gap,omitempty"`
	Sessions         []SessionSummary `json:"sessions"`
}

type SessionRevisionEvent struct {
	Type             string                     `json:"type"`
	SubscriptionID   string                     `json:"subscription_id"`
	InstanceID       string                     `json:"instance_id"`
	Revision         uint64                     `json:"revision"`
	SessionID        string                     `json:"session_id,omitempty"`
	Change           string                     `json:"change,omitempty"`
	Reason           string                     `json:"reason,omitempty"`
	ActivityCategory model.NotificationCategory `json:"activity_category,omitempty"`
	ActivitySequence uint64                     `json:"activity_sequence,omitempty"`
}
