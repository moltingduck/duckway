package handlers

import (
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
}

func NewPlaceholderHandler(placeholders *queries.PlaceholderQueries, services *queries.ServiceQueries, clients *queries.ClientQueries) *PlaceholderHandler {
	return &PlaceholderHandler{placeholders: placeholders, services: services, clients: clients}
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

	// Generate placeholder key matching service format
	placeholder, err := svc.GeneratePlaceholder(service.KeyPrefix, service.KeyLength)
	if err != nil {
		jsonError(w, "failed to generate placeholder", http.StatusInternalServerError)
		return
	}

	if req.EnvName == "" {
		req.EnvName = defaultEnvName(service.Name)
	}

	id, _ := svc.GenerateToken(16)
	requiresApproval := true
	if req.RequiresApproval != nil {
		requiresApproval = *req.RequiresApproval
	}
	ttl := 1440
	if req.ApprovalTTLMinutes != nil {
		ttl = *req.ApprovalTTLMinutes
	}

	pk := &models.PlaceholderKey{
		ID:                 id,
		EnvName:            req.EnvName,
		Placeholder:        placeholder,
		ServiceID:          req.ServiceID,
		APIKeyID:           req.APIKeyID,
		GroupID:             req.GroupID,
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

// ListWithApprovals returns placeholders enriched with latest approval status.
func (h *PlaceholderHandler) ListWithApprovals(w http.ResponseWriter, r *http.Request, approvalQ interface{ LatestByPlaceholder(string) (*models.Approval, error) }) {
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
		jsonError(w, "failed to delete placeholder", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
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
