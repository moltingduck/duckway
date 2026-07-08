package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type APIKeyHandler struct {
	apiKeys  *queries.APIKeyQueries
	services *queries.ServiceQueries
	crypto   *svc.Crypto
	client   *http.Client
}

func NewAPIKeyHandler(apiKeys *queries.APIKeyQueries, services *queries.ServiceQueries, crypto *svc.Crypto) *APIKeyHandler {
	return &APIKeyHandler{
		apiKeys:  apiKeys,
		services: services,
		crypto:   crypto,
		client:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (h *APIKeyHandler) WithHTTPClient(client *http.Client) *APIKeyHandler {
	if client != nil {
		h.client = client
	}
	return h
}

func (h *APIKeyHandler) List(w http.ResponseWriter, r *http.Request) {
	serviceID := r.URL.Query().Get("service_id")
	list, err := h.apiKeys.List(serviceID)
	if err != nil {
		jsonError(w, "failed to list keys", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.APIKey{}
	}
	// Redact encrypted keys in response
	for i := range list {
		list[i].KeyEncrypted = ""
	}
	jsonResponse(w, list)
}

func (h *APIKeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServiceID string `json:"service_id"`
		Name      string `json:"name"`
		Key       string `json:"key"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.ServiceID == "" || req.Name == "" || req.Key == "" {
		jsonError(w, "service_id, name, and key are required", http.StatusBadRequest)
		return
	}

	// Verify service exists
	service, err := h.services.GetByID(req.ServiceID)
	if err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}
	if err := validateGitHubCredentialForService(service.Name, req.Key); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	encrypted, err := h.crypto.Encrypt(req.Key)
	if err != nil {
		jsonError(w, "failed to encrypt key", http.StatusInternalServerError)
		return
	}

	id, err := svc.GenerateToken(16)
	if err != nil {
		jsonError(w, "failed to generate key ID", http.StatusInternalServerError)
		return
	}
	key := &models.APIKey{
		ID:           id,
		ServiceID:    req.ServiceID,
		Name:         req.Name,
		KeyEncrypted: encrypted,
		IsActive:     true,
	}

	if err := h.apiKeys.Create(key); err != nil {
		jsonError(w, "failed to create key: "+err.Error(), http.StatusInternalServerError)
		return
	}

	key.KeyEncrypted = "" // Don't return encrypted value
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, key)
}

func (h *APIKeyHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := h.apiKeys.GetByID(id)
	if err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}

	// Compute a masked preview: first 6 + last 4 chars of the decrypted key.
	// Never return the full secret. For very short keys, just show stars.
	preview := ""
	if key.KeyEncrypted != "" {
		if plain, derr := h.crypto.Decrypt(key.KeyEncrypted); derr == nil && plain != "" {
			preview = maskGitHubAppCredentialPreview(plain)
		}
	}
	key.KeyEncrypted = ""

	resp := map[string]interface{}{
		"id":             key.ID,
		"service_id":     key.ServiceID,
		"service_name":   key.ServiceName,
		"name":           key.Name,
		"acl":            key.ACL,
		"is_refreshable": key.IsRefreshable,
		"is_active":      key.IsActive,
		"usage_count":    key.UsageCount,
		"last_used_at":   key.LastUsedAt,
		"created_at":     key.CreatedAt,
		"key_preview":    preview,
		"expires_at":     key.ExpiresAt,
		"token_endpoint": key.TokenEndpoint,
	}
	jsonResponse(w, resp)
}

// maskKey returns "<first6>...<last4>" for keys long enough; otherwise stars.
func maskKey(s string) string {
	if len(s) <= 12 {
		return strings.Repeat("*", len(s))
	}
	return s[:6] + "..." + s[len(s)-4:]
}

func (h *APIKeyHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := h.apiKeys.GetByID(id)
	if err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}

	var req struct {
		Name string `json:"name"`
		Key  string `json:"key"` // optional — only update if non-empty
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if req.Name != "" {
		key.Name = req.Name
	}
	if req.Key != "" {
		if err := validateGitHubCredentialForService(key.ServiceName, req.Key); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		encrypted, err := h.crypto.Encrypt(req.Key)
		if err != nil {
			jsonError(w, "failed to encrypt key", http.StatusInternalServerError)
			return
		}
		key.KeyEncrypted = encrypted
	}

	if err := h.apiKeys.Update(key); err != nil {
		jsonError(w, "failed to update", http.StatusInternalServerError)
		return
	}

	key.KeyEncrypted = ""
	jsonResponse(w, key)
}

// ListACLTemplates returns available templates for this API key (based on its service).
// GET /api/keys/{id}/acl-templates
func (h *APIKeyHandler) ListACLTemplates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := h.apiKeys.GetByID(id)
	if err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}

	templates := svc.GetACLTemplates(key.ServiceName)
	if templates == nil {
		templates = []svc.ACLTemplate{}
	}
	jsonResponse(w, map[string]interface{}{
		"key_id":     key.ID,
		"service":    key.ServiceName,
		"service_id": key.ServiceID,
		"current":    key.ACL,
		"templates":  templates,
	})
}

// ApplyACLTemplate sets an API key's ACL to a template's config.
// POST /api/keys/{id}/acl-templates  body: {"template_id":"read-only"}
func (h *APIKeyHandler) ApplyACLTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := h.apiKeys.GetByID(id)
	if err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}

	var req struct {
		TemplateID string `json:"template_id"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	tmpl := svc.GetACLTemplate(key.ServiceName, req.TemplateID)
	if tmpl == nil {
		jsonError(w, "template not found for service "+key.ServiceName, http.StatusNotFound)
		return
	}

	if err := h.apiKeys.UpdateACL(id, tmpl.Config); err != nil {
		jsonError(w, "failed to apply template", http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{
		"status":      "ok",
		"template_id": tmpl.ID,
		"template":    tmpl.Name,
	})
}

// SetACL lets admin set custom ACL JSON directly.
// POST /api/keys/{id}/acl  body: {"acl":"{...}"}
func (h *APIKeyHandler) SetACL(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.apiKeys.GetByID(id); err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}
	var req struct {
		ACL string `json:"acl"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := h.apiKeys.UpdateACL(id, req.ACL); err != nil {
		jsonError(w, "failed to update ACL", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

func (h *APIKeyHandler) TestGitHubAppMinter(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		jsonError(w, "cross-origin request denied", http.StatusForbidden)
		return
	}
	var req struct {
		Credential string `json:"credential"`
		Repository string `json:"repository"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	owner, repo, err := parseGitHubOwnerRepo(req.Repository)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cred, ok, err := parseGitHubAppCredential(req.Credential)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !ok {
		jsonError(w, "credential must be a github_app credential", http.StatusBadRequest)
		return
	}
	if err := validateGitHubAppBaseURL(req.Credential); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jwt, err := githubAppJWT(cred.AppID, cred.PrivateKey, time.Now())
	if err != nil {
		jsonError(w, "failed to sign github app jwt", http.StatusBadRequest)
		return
	}
	body := githubInstallationTokenRequest{
		Repositories: []string{repo},
		Permissions:  map[string]string{"contents": "read"},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		jsonError(w, "failed to encode github app token request", http.StatusInternalServerError)
		return
	}
	baseURL := strings.TrimRight(cred.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if err := validateGitHubAppBaseURLValue(baseURL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	minted, err := mintGitHubInstallationToken(ctx, h.client, cred, baseURL, jwt, bodyBytes, time.Now())
	if err != nil {
		jsonError(w, "github app token mint failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"status":      "ok",
		"repository":  owner + "/" + repo,
		"permissions": body.Permissions,
		"expires_at":  minted.ExpiresAt.Format(time.RFC3339),
	})
}

func parseGitHubOwnerRepo(raw string) (string, string, error) {
	repo := strings.TrimSpace(raw)
	if repo == "" {
		return "", "", fmt.Errorf("repository is required")
	}
	if strings.Contains(repo, "://") || strings.ContainsAny(repo, "?#\\") {
		return "", "", fmt.Errorf("repository must be owner/repo")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository must be owner/repo")
	}
	for _, part := range parts {
		if part == "." || part == ".." || strings.HasSuffix(part, ".git") {
			return "", "", fmt.Errorf("repository must be owner/repo")
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				continue
			}
			return "", "", fmt.Errorf("repository must be owner/repo")
		}
	}
	return parts[0], parts[1], nil
}

func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (h *APIKeyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.apiKeys.DeleteWithControlChannelCleanup(id); err != nil {
		jsonError(w, "failed to delete key", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
