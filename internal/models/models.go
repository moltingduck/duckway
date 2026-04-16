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
	IsActive     bool   `json:"is_active"`
	CreatedAt    string `json:"created_at"`
}

type APIKey struct {
	ID           string  `json:"id"`
	ServiceID    string  `json:"service_id"`
	Name         string  `json:"name"`
	KeyEncrypted string  `json:"-"`
	ACL          string  `json:"acl"` // JSON permission config — overrides service default_acl
	IsActive     bool    `json:"is_active"`
	UsageCount   int64   `json:"usage_count"`
	LastUsedAt   *string `json:"last_used_at"`
	CreatedAt    string  `json:"created_at"`
	// Joined fields
	ServiceName string `json:"service_name,omitempty"`
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
	ServiceName string `json:"service_name,omitempty"`
	ClientName  string `json:"client_name,omitempty"`
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
