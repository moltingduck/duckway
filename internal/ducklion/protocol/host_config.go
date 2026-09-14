package protocol

// HostRetentionUpdate is a structured, Host-scoped Ducklord control request.
// It changes Ducklion's persisted retention policy without restarting any PTY.
type HostRetentionUpdate struct {
	PTYLogRetentionDays int `json:"pty_log_retention_days"`
}

// HostAgentHookConfig changes only Ducklion-owned agent notification hooks.
type HostAgentHookConfig struct {
	Agent  string `json:"agent"`
	Action string `json:"action"`
}

// HostAgentHookConfigResult describes the settings write, not hook activation.
// Agent hook activation still requires the agent's own trust/approval flow.
type HostAgentHookConfigResult struct {
	Agent         string `json:"agent"`
	Installed     bool   `json:"installed"`
	Changed       bool   `json:"changed"`
	BackupCreated bool   `json:"backup_created"`
	Activation    string `json:"activation"`
}

// HostAgentHookStatus separates settings presence from advisory callbacks.
// A callback's source is self-reported by a Session descendant, not proof of
// the vendor process or a trusted agent installation.
type HostAgentHookStatus struct {
	Agent               string `json:"agent"`
	Installed           bool   `json:"installed"`
	CallbackObserved    bool   `json:"callback_observed"`
	CallbackSessionID   string `json:"callback_session_id,omitempty"`
	CallbackGeneration  uint64 `json:"callback_generation,omitempty"`
	CallbackUpdatedAtMS int64  `json:"callback_updated_at_ms,omitempty"`
}
