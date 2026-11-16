package handlers

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/audit"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func init() { audit.SetSecretNamer(isSecretFieldName) }

// AuditSnapshots reads the rule- and access-bearing resources for the audit
// log's before/after JSON. Values are raw only where the audit recorder will
// mask them (a secret-named field becomes "(set)" or "(changed)"); nothing is
// rendered to a browser from here.
type AuditSnapshots struct {
	Services      *queries.ServiceQueries
	APIKeys       *queries.APIKeyQueries
	Placeholders  *queries.PlaceholderQueries
	Groups        *queries.GroupQueries
	Clients       *queries.ClientQueries
	Notifications *queries.NotificationQueries
	Approvals     *queries.ApprovalQueries
	Settings      *queries.SettingsQueries
}

// jsonOrString keeps a JSON document as structured data so before/after line
// diffs are readable; anything else stays a string.
func jsonOrString(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

func redactProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(set)"
	}
	if u.User != nil {
		u.User = url.User("(credentials)")
	}
	return u.String()
}

// Get returns the display name and snapshot for a resource, or ok=false when
// it does not exist (or the type has no snapshot). id is ignored for "setting".
func (s *AuditSnapshots) Get(resourceType, id string) (name string, v any, ok bool) {
	if s == nil {
		return "", nil, false
	}
	switch resourceType {
	case "service":
		if s.Services == nil {
			return
		}
		svc, err := s.Services.GetByID(id)
		if err != nil || svc == nil {
			return
		}
		return svc.Name, map[string]any{
			"id": svc.ID, "name": svc.Name, "display_name": svc.DisplayName, "upstream_url": svc.UpstreamURL,
			"host_pattern": svc.HostPattern, "auth_type": svc.AuthType, "delivery_mode": svc.DeliveryMode,
			"default_acl": jsonOrString(svc.DefaultACL), "is_active": svc.IsActive,
		}, true
	case "api_key":
		if s.APIKeys == nil {
			return
		}
		k, err := s.APIKeys.GetByID(id)
		if err != nil || k == nil {
			return
		}
		return k.Name, map[string]any{
			"id": k.ID, "name": k.Name, "service_id": k.ServiceID, "acl": jsonOrString(k.ACL), "is_active": k.IsActive,
			"upstream_proxy_url": redactProxyURL(k.UpstreamProxyURL), "refreshable": k.RefreshToken != "",
			"subscription_info": jsonOrString(redactSubscriptionInfo(k.SubscriptionInfo)),
			"key_encrypted":     k.KeyEncrypted, "refresh_token": k.RefreshToken,
		}, true
	case "placeholder":
		if s.Placeholders == nil {
			return
		}
		p, err := s.Placeholders.GetByID(id)
		if err != nil || p == nil {
			return
		}
		cfg := ""
		if p.PermissionConfig != nil {
			cfg = *p.PermissionConfig
		}
		return p.EnvName, map[string]any{
			"id": p.ID, "env_name": p.EnvName, "service_id": p.ServiceID, "client_id": p.ClientID,
			"api_key_id": p.APIKeyID, "group_id": p.GroupID, "key_group_id": p.KeyGroupID,
			"permission_config": jsonOrString(cfg), "requires_approval": p.RequiresApproval,
			"approval_ttl_minutes": p.ApprovalTTLMinutes, "key_path": p.KeyPath, "is_active": p.IsActive,
		}, true
	case "group":
		if s.Groups == nil {
			return
		}
		g, err := s.Groups.GetByID(id)
		if err != nil || g == nil {
			return
		}
		var members []string
		if ms, err := s.Groups.GetMembers(id); err == nil {
			for _, m := range ms {
				members = append(members, m.ID+" "+m.Name)
			}
		}
		sort.Strings(members)
		return g.Name, map[string]any{
			"id": g.ID, "name": g.Name, "service_id": g.ServiceID, "strategy": g.Strategy, "members": members,
		}, true
	case "client":
		if s.Clients == nil {
			return
		}
		c, err := s.Clients.GetByID(id)
		if err != nil || c == nil {
			return
		}
		return c.Name, map[string]any{
			"id": c.ID, "name": c.Name, "is_active": c.IsActive, "canary_enabled": c.CanaryEnabled,
			"update_policy": c.UpdatePolicy, "token_hash": c.TokenHash,
		}, true
	case "notification":
		if s.Notifications == nil {
			return
		}
		n, err := s.Notifications.GetByID(id)
		if err != nil || n == nil {
			return
		}
		cfg := map[string]any{}
		for k, val := range parseNotificationConfig(n.Config) {
			if notificationKeyIsSecret(n.ChannelType, k) {
				k += "_secret" // masked by the recorder, with change detection
			}
			cfg[k] = val
		}
		return n.Name, map[string]any{
			"id": n.ID, "channel_type": n.ChannelType, "name": n.Name, "is_active": n.IsActive, "config": cfg,
		}, true
	case "approval":
		if s.Approvals == nil {
			return
		}
		a, err := s.Approvals.GetByID(id)
		if err != nil || a == nil {
			return
		}
		name = a.PlaceholderID
		if s.Placeholders != nil {
			if p, err := s.Placeholders.GetByID(a.PlaceholderID); err == nil && p != nil {
				name = p.EnvName
			}
		}
		return name, map[string]any{
			"id": a.ID, "placeholder_id": a.PlaceholderID, "status": a.Status,
			"approved_at": a.ApprovedAt, "expires_at": a.ExpiresAt,
		}, true
	case "setting":
		if s.Settings == nil {
			return
		}
		all := s.Settings.GetAll()
		out := map[string]any{}
		for k, val := range all {
			if reservedSetting(k) {
				continue
			}
			out[k] = val // secret-named keys are masked by the recorder
		}
		return "", out, true
	}
	return
}
