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

func (h *APIKeyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.apiKeys.Delete(id); err != nil {
		jsonError(w, "failed to delete key", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}
