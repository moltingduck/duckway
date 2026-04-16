package handlers

import (
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type APIKeyHandler struct {
	apiKeys  *queries.APIKeyQueries
	services *queries.ServiceQueries
	crypto   *svc.Crypto
}

func NewAPIKeyHandler(apiKeys *queries.APIKeyQueries, services *queries.ServiceQueries, crypto *svc.Crypto) *APIKeyHandler {
	return &APIKeyHandler{apiKeys: apiKeys, services: services, crypto: crypto}
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
	_, err := h.services.GetByID(req.ServiceID)
	if err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}

	encrypted, err := h.crypto.Encrypt(req.Key)
	if err != nil {
		jsonError(w, "failed to encrypt key", http.StatusInternalServerError)
		return
	}

	id, _ := svc.GenerateToken(16)
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
		"key_id":   key.ID,
		"service":  key.ServiceName,
		"current":  key.ACL,
		"templates": templates,
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

func (h *APIKeyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.apiKeys.Delete(id); err != nil {
		jsonError(w, "failed to delete key", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}
