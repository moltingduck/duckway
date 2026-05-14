package models

type AdminUser struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	CreatedAt    string `json:"created_at"`
}

type Service struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	UpstreamURL  string `json:"upstream_url"`
	HostPattern  string `json:"host_pattern"`
	AuthType     string `json:"auth_type"`     // bearer, header, query, basic
	AuthHeader   string `json:"auth_header"`   // e.g. "Authorization"
	AuthPrefix   string `json:"auth_prefix"`   // e.g. "Bearer "
	KeyPrefix    string `json:"key_prefix"`    // e.g. "sk-", "ghp_"
	KeyLength    int    `json:"key_length"`    // real key total length
	KeyDirectory string `json:"key_directory"` // default key file path, e.g. ".config/openai/credentials"
	DefaultACL   string `json:"default_acl"`   // JSON ACL config, applied when placeholder has no permission_config
	// DeliveryMode controls how the duckway-client sidecar handles requests for this service.
	//   "proxy"      — full MITM via gateway. Gateway sees every request, enforces ACL per-request.
	//   "loan_proxy" — sidecar caches the real token from gateway and forwards directly to upstream.
	//                  Lower latency, no body buffering on gateway. Coarse-grained ACL only.
	DeliveryMode string `json:"delivery_mode"`
	IsActive     bool   `json:"is_active"`
	CreatedAt    string `json:"created_at"`
}

type APIKey struct {
	ID               string  `json:"id"`
	ServiceID        string  `json:"service_id"`
	Name             string  `json:"name"`
	KeyEncrypted     string  `json:"-"`
	ACL              string  `json:"acl"`
	RefreshToken     string  `json:"-"`                 // encrypted, empty = static key
	ExpiresAt        int64   `json:"expires_at"`        // Unix ms, 0 = no expiry
	TokenEndpoint    string  `json:"token_endpoint"`    // OAuth refresh URL
	SubscriptionInfo string  `json:"subscription_info"` // JSON: type, tier, scopes, etc.
	UsageSnapshot    string  `json:"usage_snapshot"`    // JSON: latest upstream rate-limit headers
	IsActive         bool    `json:"is_active"`
	UsageCount       int64   `json:"usage_count"`
	LastUsedAt       *string `json:"last_used_at"`
	CreatedAt        string  `json:"created_at"`
	// Joined
	ServiceName string `json:"service_name,omitempty"`
	// Derived
	IsRefreshable bool `json:"is_refreshable"` // true if refresh_token is set
}

type APIKeyGroup struct {
	ID        string `json:"id"`
	ServiceID string `json:"service_id"`
	Name      string `json:"name"`
	Strategy  string `json:"strategy"` // round-robin, least-used, failover
	LastIndex int    `json:"last_index"`
	CreatedAt string `json:"created_at"`
	// Joined
	ServiceName string   `json:"service_name,omitempty"`
	Members     []APIKey `json:"members,omitempty"`
}

type APIKeyGroupMember struct {
	GroupID  string `json:"group_id"`
	APIKeyID string `json:"api_key_id"`
	Priority int    `json:"priority"`
}

type Client struct {
	ID             string  `json:"id"`
	ShortID        string  `json:"short_id"` // 6 alphanumeric chars, used for canary email tagging
	Name           string  `json:"name"`
	TokenHash      string  `json:"-"`
	IsActive       bool    `json:"is_active"`
	CanaryEnabled  bool    `json:"canary_enabled"`
	LastSeenAt     *string `json:"last_seen_at"`
	CreatedAt      string  `json:"created_at"`
}

type PlaceholderKey struct {
	ID                 string  `json:"id"`
	EnvName            string  `json:"env_name"`
	Placeholder        string  `json:"placeholder"`
	ServiceID          string  `json:"service_id"`
	APIKeyID           *string `json:"api_key_id"`
	GroupID            *string `json:"group_id"`
	KeyGroupID         *string `json:"key_group_id"`
	ClientID           string  `json:"client_id"`
	PermissionConfig   *string `json:"permission_config"`
	RequiresApproval   bool    `json:"requires_approval"`
	ApprovalTTLMinutes int     `json:"approval_ttl_minutes"`
	KeyPath            string  `json:"key_path"` // override path, falls back to service.key_directory
	IsActive           bool    `json:"is_active"`
	UsageCount         int64   `json:"usage_count"`
	LastUsedAt         *string `json:"last_used_at"`
	CreatedAt          string  `json:"created_at"`
	// Joined
	ServiceName string  `json:"service_name,omitempty"`
	ClientName  string  `json:"client_name,omitempty"`
	APIKeyName  *string `json:"api_key_name,omitempty"`
}

type Approval struct {
	ID            string  `json:"id"`
	PlaceholderID string  `json:"placeholder_id"`
	Status        string  `json:"status"` // pending, approved, rejected
	ApprovedAt    *string `json:"approved_at"`
	ExpiresAt     *string `json:"expires_at"`
	RequestInfo   *string `json:"request_info"`
	CreatedAt     string  `json:"created_at"`
}

type RequestLog struct {
	ID            int64   `json:"id"`
	PlaceholderID *string `json:"placeholder_id"`
	ClientID      *string `json:"client_id"`
	ServiceName   string  `json:"service_name"`
	Method        string  `json:"method"`
	Path          string  `json:"path"`
	StatusCode    *int    `json:"status_code"`
	CreatedAt     string  `json:"created_at"`
}

// KeyGroup is a named set of API keys with score-based selection and 429 auto-rotation.
type KeyGroup struct {
	ID          string `json:"id" db:"id"`
	Name        string `json:"name" db:"name"`
	Description string `json:"description" db:"description"`
	ServiceName string `json:"service_name" db:"service_name"`
	CreatedAt   string `json:"created_at" db:"created_at"`
}

type KeyGroupMember struct {
	GroupID        string  `json:"group_id" db:"group_id"`
	APIKeyID       string  `json:"api_key_id" db:"api_key_id"`
	Position       int     `json:"position" db:"position"`
	ExhaustedUntil *string `json:"exhausted_until" db:"exhausted_until"`
}

type KeyGroupWithMembers struct {
	KeyGroup
	Members []KeyGroupMemberDetail `json:"members"`
}

type KeyGroupMemberDetail struct {
	KeyGroupMember
	KeyName         string  `json:"key_name"`
	TokensRemaining *int64  `json:"tokens_remaining"`
	TokensLimit     *int64  `json:"tokens_limit"`
	ResetAt         *string `json:"reset_at"`
	BoundClients    int     `json:"bound_clients"`
	Score           float64 `json:"score"`
}

// ControlChannel binds one client to one Discord category via a bot.
// The 1:1 invariant is enforced by a UNIQUE index on client_id.
type ControlChannel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ServiceID     string `json:"service_id"`
	APIKeyID      string `json:"api_key_id"`
	ClientID      string `json:"client_id"`
	AgentType     string `json:"agent_type"`     // claude_code, openclaw, ...
	PlaceholderID string `json:"placeholder_id"` // phantom token for the bot
	Config        string `json:"config"`         // JSON; shape is per-service
	IsActive      bool   `json:"is_active"`
	CreatedAt     string `json:"created_at"`
	// Joined
	ServiceName string `json:"service_name,omitempty"`
	APIKeyName  string `json:"api_key_name,omitempty"`
	ClientName  string `json:"client_name,omitempty"`
	// Optional eager-loaded
	Channels []CCChannel `json:"channels,omitempty"`
}

// CCChannel mirrors a Discord channel under a CC's category. handle is the
// opaque token agents see; channel_id is the real Discord ID, kept
// server-side.
//
// kind:
//
//	"management" — the per-client control channel (one per CC). Daemon
//	               parses commands here, doesn't start sessions.
//	"task"       — every other channel. Each maps to one claude session.
//
// session_id + cwd are populated as the daemon runs the first prompt; cwd
// can be set up-front via `!new --cwd` or channel topic `cwd:/path`.
type CCChannel struct {
	Handle     string  `json:"handle"`
	CCID       string  `json:"cc_id"`
	ClientID   *string `json:"client_id"`
	ChannelID  string  `json:"channel_id"`
	Name       string  `json:"name"`
	Topic      string  `json:"topic"`
	Kind       string  `json:"kind"`
	SessionID  string  `json:"session_id"`
	Cwd        string  `json:"cwd"`
	Archived   bool    `json:"archived"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt *string `json:"last_seen_at"`
}

// InboxEvent is one buffered Discord gateway dispatch ready to be polled.
type InboxEvent struct {
	ID            int64   `json:"id"`
	CCID          string  `json:"cc_id"`
	ChannelHandle *string `json:"channel_handle"`
	EventType     string  `json:"event_type"`
	Payload       string  `json:"payload"`
	CreatedAt     string  `json:"created_at"`
}
