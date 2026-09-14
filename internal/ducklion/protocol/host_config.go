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
