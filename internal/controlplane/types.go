package controlplane

const (
	ProtocolVersion         = 1
	CapabilityManagedUpdate = "managed_update_v1"
	CommandUpdateRestart    = "update_restart"

	JobLeased  = "leased"
	JobRunning = "running"
	JobHealthy = "healthy"
	JobFailed  = "failed"
)

type CurrentJob struct {
	ID             string `json:"id"`
	LeaseToken     string `json:"lease_token"`
	Status         string `json:"status"`
	RunningVersion string `json:"running_version,omitempty"`
	Error          string `json:"error,omitempty"`
}

type HeartbeatRequest struct {
	ProtocolVersion int               `json:"protocol_version"`
	Version         string            `json:"version"`
	OS              string            `json:"os"`
	Arch            string            `json:"arch"`
	BootID          string            `json:"boot_id"`
	InstallPath     string            `json:"install_path"`
	InstallWritable bool              `json:"install_writable"`
	Capabilities    []string          `json:"capabilities"`
	Components      map[string]string `json:"components"`
	CurrentJob      *CurrentJob       `json:"current_job,omitempty"`
}

type Command struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	TargetVersion  string `json:"target_version"`
	Binary         string `json:"binary"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	LeaseToken     string `json:"lease_token"`
	LeaseExpiresAt string `json:"lease_expires_at"`
	Attempt        int    `json:"attempt"`
}

type HeartbeatResponse struct {
	NextHeartbeatSeconds int      `json:"next_heartbeat_seconds"`
	ServerTime           string   `json:"server_time"`
	Command              *Command `json:"command"`
}

type JobStatusRequest struct {
	LeaseToken     string `json:"lease_token"`
	Status         string `json:"status"`
	RunningVersion string `json:"running_version,omitempty"`
	Error          string `json:"error,omitempty"`
}

type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Binary string `json:"binary"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
