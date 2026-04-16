package services

import (
	"database/sql"
	"fmt"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

type KeyResolver struct {
	crypto       *Crypto
	apiKeys      *queries.APIKeyQueries
	placeholders *queries.PlaceholderQueries
	groups       *queries.GroupQueries
	approvals    *queries.ApprovalQueries
}

func NewKeyResolver(crypto *Crypto, apiKeys *queries.APIKeyQueries, placeholders *queries.PlaceholderQueries, groups *queries.GroupQueries, approvals *queries.ApprovalQueries) *KeyResolver {
	return &KeyResolver{
		crypto:       crypto,
		apiKeys:      apiKeys,
		placeholders: placeholders,
		groups:       groups,
		approvals:    approvals,
	}
}

type ResolveResult struct {
	RealKey          string
	APIKeyID         string
	PlaceholderID    string
	PermissionConfig string // from placeholder_keys.permission_config
	APIKeyACL        string // from api_keys.acl
	Permitted        bool
	NeedApproval     bool
	Error            string
}

// Resolve takes a placeholder key and client ID, returns the real API key.
func (r *KeyResolver) Resolve(placeholder string, clientID string) (*ResolveResult, error) {
	// Look up placeholder
	ph, err := r.placeholders.GetByPlaceholder(placeholder)
	if err != nil {
		if err == sql.ErrNoRows {
			return &ResolveResult{Error: "unknown placeholder key"}, nil
		}
		return nil, fmt.Errorf("lookup placeholder: %w", err)
	}

	// Verify client binding
	if ph.ClientID != clientID {
		return &ResolveResult{Error: "placeholder key not bound to this client"}, nil
	}

	if !ph.IsActive {
		return &ResolveResult{Error: "placeholder key is inactive"}, nil
	}

	// Check approval if required
	if ph.RequiresApproval {
		_, err := r.approvals.GetValidApproval(ph.ID)
		if err != nil {
			if err == sql.ErrNoRows {
				return &ResolveResult{NeedApproval: true, PlaceholderID: ph.ID, Error: "approval required"}, nil
			}
			return nil, fmt.Errorf("check approval: %w", err)
		}
	}

	// Resolve to real key
	var apiKey *models.APIKey

	if ph.APIKeyID != nil {
		apiKey, err = r.apiKeys.GetByID(*ph.APIKeyID)
		if err != nil {
			return nil, fmt.Errorf("get api key: %w", err)
		}
	} else if ph.GroupID != nil {
		apiKey, err = r.resolveFromGroup(*ph.GroupID)
		if err != nil {
			return nil, fmt.Errorf("resolve from group: %w", err)
		}
	} else {
		return &ResolveResult{Error: "placeholder has no key or group assigned"}, nil
	}

	if !apiKey.IsActive {
		return &ResolveResult{Error: "api key is inactive"}, nil
	}

	// Decrypt the real key
	realKey, err := r.crypto.Decrypt(apiKey.KeyEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt key: %w", err)
	}

	// Update usage counters
	r.placeholders.IncrementUsage(ph.ID)
	r.apiKeys.IncrementUsage(apiKey.ID)

	permConfig := ""
	if ph.PermissionConfig != nil {
		permConfig = *ph.PermissionConfig
	}

	return &ResolveResult{
		RealKey:          realKey,
		APIKeyID:         apiKey.ID,
		PlaceholderID:    ph.ID,
		PermissionConfig: permConfig,
		APIKeyACL:        apiKey.ACL,
		Permitted:        true,
	}, nil
}

// ResolveForService resolves a key for a client+service pair (when no explicit placeholder is provided).
func (r *KeyResolver) ResolveForService(clientID, serviceID string) (*ResolveResult, error) {
	ph, err := r.placeholders.GetByClientAndService(clientID, serviceID)
	if err != nil {
		if err == sql.ErrNoRows {
			return &ResolveResult{Error: "no placeholder key for this client+service"}, nil
		}
		return nil, fmt.Errorf("lookup placeholder: %w", err)
	}
	return r.Resolve(ph.Placeholder, clientID)
}

func (r *KeyResolver) resolveFromGroup(groupID string) (*models.APIKey, error) {
	group, err := r.groups.GetByID(groupID)
	if err != nil {
		return nil, err
	}

	members, err := r.groups.GetMembers(groupID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("group %s has no active members", groupID)
	}

	var selected *models.APIKey

	switch group.Strategy {
	case "round-robin":
		idx := group.LastIndex % len(members)
		selected = &members[idx]
		r.groups.UpdateLastIndex(groupID, group.LastIndex+1)

	case "least-used":
		selected = &members[0]
		for i := range members {
			if members[i].UsageCount < selected.UsageCount {
				selected = &members[i]
			}
		}

	case "failover":
		// Use first active key in priority order
		selected = &members[0]

	default:
		selected = &members[0]
	}

	return selected, nil
}
