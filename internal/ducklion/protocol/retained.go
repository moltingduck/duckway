package protocol

// RetainedShellSummary is a diagnostic record, not a selectable Session.
// PTY bytes are obtained separately through the generation-fenced output
// subscription and never embedded in an inventory response.
type RetainedShellSummary struct {
	SessionID         string `json:"session_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
	Handle            string `json:"handle"`
	ExitedAtMS        int64  `json:"exited_at_ms"`
	ExitSuccess       bool   `json:"exit_success"`
	ExitReason        string `json:"exit_reason"`
}
