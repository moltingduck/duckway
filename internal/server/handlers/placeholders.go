package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type PlaceholderHandler struct {
	placeholders *queries.PlaceholderQueries
	services     *queries.ServiceQueries
	clients      *queries.ClientQueries
	apiKeys      *queries.APIKeyQueries
	crypto       *svc.Crypto
}

func NewPlaceholderHandler(placeholders *queries.PlaceholderQueries, services *queries.ServiceQueries, clients *queries.ClientQueries) *PlaceholderHandler {
	return &PlaceholderHandler{placeholders: placeholders, services: services, clients: clients}
}

// WithKeyLookup enables format-sniffing: when a placeholder is created with an
// explicit api_key_id, the handler decrypts and inspects the real key to pick
// the right phantom prefix/length (e.g. ghp_* vs github_pat_*).
func (h *PlaceholderHandler) WithKeyLookup(apiKeys *queries.APIKeyQueries, crypto *svc.Crypto) *PlaceholderHandler {
	h.apiKeys = apiKeys
	h.crypto = crypto
	return h
}

func (h *PlaceholderHandler) List(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	serviceID := r.URL.Query().Get("service_id")
	list, err := h.placeholders.List(clientID, serviceID)
	if err != nil {
		jsonError(w, "failed to list placeholders", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.PlaceholderKey{}
	}
	jsonResponse(w, list)
}

func (h *PlaceholderHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ph, err := h.placeholders.GetByID(id)
	if err != nil {
		jsonError(w, "placeholder not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, ph)
}

func (h *PlaceholderHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnvName            string  `json:"env_name"`
		ServiceID          string  `json:"service_id"`
		APIKeyID           *string `json:"api_key_id"`
		GroupID            *string `json:"group_id"`
		ClientID           string  `json:"client_id"`
		PermissionConfig   *string `json:"permission_config"`
		RequiresApproval   *bool   `json:"requires_approval"`
		ApprovalTTLMinutes *int    `json:"approval_ttl_minutes"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.ServiceID == "" || req.ClientID == "" {
		jsonError(w, "service_id and client_id are required", http.StatusBadRequest)
		return
	}

	if req.APIKeyID == nil && req.GroupID == nil {
		jsonError(w, "either api_key_id or group_id is required", http.StatusBadRequest)
		return
	}

	// Get service for key format
	service, err := h.services.GetByID(req.ServiceID)
	if err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}

	if req.PermissionConfig != nil {
		if err := svc.ValidatePermissionConfig(*req.PermissionConfig); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Pick prefix/length: when a specific API key is assigned and we have
	// the crypto to decrypt it, sniff the real key's format so the phantom
	// matches (e.g. ghp_* vs github_pat_* for GitHub).
	prefix, keyLen := service.KeyPrefix, service.KeyLength
	realKeyForPlaceholder := ""
	if req.APIKeyID != nil && h.apiKeys != nil && h.crypto != nil {
		if apiKey, err := h.apiKeys.GetByID(*req.APIKeyID); err == nil {
			if apiKey.ServiceID != req.ServiceID {
				jsonError(w, "api_key_id does not belong to service_id", http.StatusBadRequest)
				return
			}
			if realKey, err := h.crypto.Decrypt(apiKey.KeyEncrypted); err == nil {
				realKeyForPlaceholder = realKey
			}
		}
	}
	isGitHubAppMinterAssignment := service.Name == "github" && isGitHubAppCredentialJSON(realKeyForPlaceholder)
	if isGitHubAppMinterAssignment {
		if req.PermissionConfig == nil {
			jsonError(w, "github app minter assignments require repository-scoped permission_config", http.StatusBadRequest)
			return
		}
		if err := svc.ValidateGitHubRepoScopePermissionConfig(*req.PermissionConfig); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if req.EnvName == "" {
		req.EnvName = defaultEnvName(service.Name)
	}
	if isGitHubAppMinterAssignment && req.APIKeyID != nil {
		req.EnvName = githubMintableAssignmentEnvName(req.EnvName, *req.APIKeyID)
	}

	requiresApproval := true
	if req.RequiresApproval != nil {
		requiresApproval = *req.RequiresApproval
	}
	ttl := 1440
	if req.ApprovalTTLMinutes != nil {
		ttl = *req.ApprovalTTLMinutes
	}

	existing, err := h.placeholders.GetByClientServiceEnv(req.ClientID, req.ServiceID, req.EnvName)
	if err == sql.ErrNoRows && isGitHubAppMinterAssignment && req.APIKeyID != nil {
		existing, err = h.placeholders.GetByClientAPIKey(req.ClientID, *req.APIKeyID)
	}
	if err == nil {
		if existing.SuiteID != nil {
			jsonError(w, "phantom token already exists for this client/service/env from a key suite; edit or remove the suite assignment first", http.StatusConflict)
			return
		}
		if strings.TrimSpace(existing.Placeholder) == "" || !sameOptionalString(existing.APIKeyID, req.APIKeyID) || !sameOptionalString(existing.GroupID, req.GroupID) {
			placeholder, err := generatePlaceholderForAssignment(realKeyForPlaceholder, prefix, keyLen)
			if err != nil {
				jsonError(w, "failed to generate placeholder", http.StatusInternalServerError)
				return
			}
			existing.Placeholder = placeholder
		}
		existing.APIKeyID = req.APIKeyID
		existing.GroupID = req.GroupID
		existing.PermissionConfig = req.PermissionConfig
		existing.RequiresApproval = requiresApproval
		existing.ApprovalTTLMinutes = ttl
		existing.IsActive = true
		if err := h.placeholders.Update(existing); err != nil {
			jsonError(w, "failed to update existing phantom token", http.StatusInternalServerError)
			return
		}
		updated, _ := h.placeholders.GetByID(existing.ID)
		if updated != nil {
			existing = updated
		}
		jsonResponse(w, existing)
		return
	} else if err != sql.ErrNoRows {
		jsonError(w, "failed to check existing phantom token", http.StatusInternalServerError)
		return
	}

	placeholder, err := generatePlaceholderForAssignment(realKeyForPlaceholder, prefix, keyLen)
	if err != nil {
		jsonError(w, "failed to generate placeholder", http.StatusInternalServerError)
		return
	}

	id, _ := svc.GenerateToken(16)

	pk := &models.PlaceholderKey{
		ID:                 id,
		EnvName:            req.EnvName,
		Placeholder:        placeholder,
		ServiceID:          req.ServiceID,
		APIKeyID:           req.APIKeyID,
		GroupID:            req.GroupID,
		ClientID:           req.ClientID,
		PermissionConfig:   req.PermissionConfig,
		RequiresApproval:   requiresApproval,
		ApprovalTTLMinutes: ttl,
		IsActive:           true,
	}

	if err := h.placeholders.Create(pk); err != nil {
		jsonError(w, "failed to create placeholder: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Fetch full object with joins
	created, _ := h.placeholders.GetByID(id)
	if created != nil {
		pk = created
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, pk)
}

func generatePlaceholderForAssignment(realKeyForPlaceholder, prefix string, keyLen int) (string, error) {
	if realKeyForPlaceholder != "" {
		return svc.GeneratePlaceholderForRealKey(realKeyForPlaceholder, prefix, keyLen)
	}
	return svc.GeneratePlaceholder(prefix, keyLen)
}

func githubMintableAssignmentEnvName(base, apiKeyID string) string {
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return base
	}
	sum := sha256.Sum256([]byte(apiKeyID))
	return base + "_" + strings.ToUpper(hex.EncodeToString(sum[:4]))
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ListWithApprovals returns placeholders enriched with latest approval status.
func (h *PlaceholderHandler) ListWithApprovals(w http.ResponseWriter, r *http.Request, approvalQ interface {
	LatestByPlaceholder(string) (*models.Approval, error)
}) {
	clientID := r.URL.Query().Get("client_id")
	serviceID := r.URL.Query().Get("service_id")
	list, err := h.placeholders.List(clientID, serviceID)
	if err != nil {
		jsonError(w, "failed to list", http.StatusInternalServerError)
		return
	}

	type phWithApproval struct {
		models.PlaceholderKey
		ApprovalStatus string  `json:"approval_status"`
		ApprovedAt     *string `json:"approved_at"`
		ExpiresAt      *string `json:"expires_at"`
	}

	var result []phWithApproval
	for _, p := range list {
		pa := phWithApproval{PlaceholderKey: p}
		if a, err := approvalQ.LatestByPlaceholder(p.ID); err == nil {
			pa.ApprovalStatus = a.Status
			pa.ApprovedAt = a.ApprovedAt
			pa.ExpiresAt = a.ExpiresAt
		}
		result = append(result, pa)
	}
	if result == nil {
		result = []phWithApproval{}
	}
	jsonResponse(w, result)
}

func (h *PlaceholderHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ph, err := h.placeholders.GetByID(id)
	if err != nil {
		jsonError(w, "placeholder not found", http.StatusNotFound)
		return
	}

	var req struct {
		EnvName            string  `json:"env_name"`
		RequiresApproval   *bool   `json:"requires_approval"`
		ApprovalTTLMinutes *int    `json:"approval_ttl_minutes"`
		KeyPath            *string `json:"key_path"`
		PermissionConfig   *string `json:"permission_config"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if req.EnvName != "" {
		ph.EnvName = req.EnvName
	}
	if req.RequiresApproval != nil {
		ph.RequiresApproval = *req.RequiresApproval
	}
	if req.ApprovalTTLMinutes != nil {
		ph.ApprovalTTLMinutes = *req.ApprovalTTLMinutes
	}
	if req.KeyPath != nil {
		ph.KeyPath = *req.KeyPath
	}
	if req.PermissionConfig != nil {
		if err := svc.ValidatePermissionConfig(*req.PermissionConfig); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if h.isGitHubAppMinterPlaceholder(ph) {
			if err := svc.ValidateGitHubRepoScopePermissionConfig(*req.PermissionConfig); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		ph.PermissionConfig = req.PermissionConfig
	}

	if err := h.placeholders.Update(ph); err != nil {
		jsonError(w, "failed to update", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, ph)
}

func (h *PlaceholderHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.placeholders.Delete(id); err != nil {
		jsonError(w, "failed to unassign phantom token", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *PlaceholderHandler) isGitHubAppMinterPlaceholder(ph *models.PlaceholderKey) bool {
	if ph == nil || ph.APIKeyID == nil || h.apiKeys == nil || h.crypto == nil {
		return false
	}
	if ph.ServiceName != "" && ph.ServiceName != "github" {
		return false
	}
	apiKey, err := h.apiKeys.GetByID(*ph.APIKeyID)
	if err != nil || apiKey.ServiceName != "github" {
		return false
	}
	plain, err := h.crypto.Decrypt(apiKey.KeyEncrypted)
	return err == nil && isGitHubAppCredentialJSON(plain)
}

func defaultEnvName(serviceName string) string {
	switch serviceName {
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "github":
		return "GITHUB_TOKEN"
	default:
		return strings.ToUpper(serviceName) + "_API_KEY"
	}
}
