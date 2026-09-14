package protocol

// HostRetentionUpdate is a structured, Host-scoped Ducklord control request.
// It changes Ducklion's persisted retention policy without restarting any PTY.
type HostRetentionUpdate struct {
	PTYLogRetentionDays int `json:"pty_log_retention_days"`
}
