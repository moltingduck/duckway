package handlers

import (
	"log"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type ClientHandler struct {
	clients      *queries.ClientQueries
	placeholders *queries.PlaceholderQueries
	services     *queries.ServiceQueries
	apiKeys      *queries.APIKeyQueries
	canarySvc    *svc.CanaryService
}

func NewClientHandler(clients *queries.ClientQueries, placeholders *queries.PlaceholderQueries, services *queries.ServiceQueries, apiKeys *queries.APIKeyQueries, canarySvc *svc.CanaryService) *ClientHandler {
	return &ClientHandler{clients: clients, placeholders: placeholders, services: services, apiKeys: apiKeys, canarySvc: canarySvc}
}

// Admin: list all clients
func (h *ClientHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.clients.List()
	if err != nil {
		jsonError(w, "failed to list clients", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.Client{}
	}
	jsonResponse(w, list)
}

// Admin: create a client and return the token (shown once)
func (h *ClientHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	name := req.Name

	if name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	// Enforce unique name
	if existing, _ := h.clients.GetByName(name); existing != nil {
		jsonError(w, "client name '"+name+"' already exists", http.StatusConflict)
		return
	}

	token, err := svc.GenerateToken(32)
	if err != nil {
		jsonError(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	id, _ := svc.GenerateToken(16)
	shortID := svc.GenerateShortID()
	client := &models.Client{
		ID:            id,
		ShortID:       shortID,
		Name:          name,
		TokenHash:     svc.HashToken(token),
		IsActive:      true,
		CanaryEnabled: true,
	}

	if err := h.clients.Create(client); err != nil {
		jsonError(w, "failed to create client: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Auto-assign heartbeat placeholder key
	h.assignHeartbeat(id)

	// Generate canary tokens in background
	if h.canarySvc != nil {
		go func() {
			if err := h.canarySvc.GenerateForClient(id, name, shortID); err != nil {
				log.Printf("Canary token generation failed for %s: %v", name, err)
			}
		}()
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]string{
		"id":       id,
		"short_id": shortID,
		"name":     name,
		"token":    token, // Shown only once
	})
}

// Admin: delete a client
func (h *ClientHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Delete canary tokens from canarytokens.org + DB
	if h.canarySvc != nil {
		if err := h.canarySvc.DeleteClientTokens(id); err != nil {
			log.Printf("Warning: failed to clean up canary tokens for client %s: %v", id, err)
		}
	}

	if err := h.clients.Delete(id); err != nil {
		jsonError(w, "failed to delete client", http.StatusInternalServerError)
		return
	}
	// Return empty body so HTMX outerHTML swap removes the card cleanly
	w.WriteHeader(http.StatusOK)
}

func (h *ClientHandler) assignHeartbeat(clientID string) {
	if h.services == nil {
		return
	}
	hbSvc, err := h.services.GetByName("heartbeat")
	if err != nil {
		return
	}

	// Check if already assigned
	_, err = h.placeholders.GetByClientAndService(clientID, hbSvc.ID)
	if err == nil {
		return
	}

	// Find the heartbeat API key
	keys, err := h.apiKeys.List(hbSvc.ID)
	if err != nil || len(keys) == 0 {
		log.Printf("No heartbeat API key found, skipping")
		return
	}
	keyID := keys[0].ID

	placeholder, err := svc.GeneratePlaceholder(hbSvc.KeyPrefix, hbSvc.KeyLength)
	if err != nil {
		log.Printf("Failed to generate heartbeat placeholder: %v", err)
		return
	}

	phID, _ := svc.GenerateToken(16)
	ph := &models.PlaceholderKey{
		ID:                 phID,
		EnvName:            "DUCKWAY_HEARTBEAT",
		Placeholder:        placeholder,
		ServiceID:          hbSvc.ID,
		APIKeyID:           &keyID,
		ClientID:           clientID,
		RequiresApproval:   false,
		ApprovalTTLMinutes: 0,
		IsActive:           true,
	}
	if err := h.placeholders.Create(ph); err != nil {
		log.Printf("Failed to create heartbeat placeholder: %v", err)
	}
}

// Admin: toggle canary enabled for a client
func (h *ClientHandler) ToggleCanary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.clients.UpdateCanaryEnabled(id, req.Enabled); err != nil {
		jsonError(w, "failed to update", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"status": "ok", "canary_enabled": req.Enabled})
}

// Client API: get assigned placeholder keys for this client
func (h *ClientHandler) GetKeys(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client not found in context", http.StatusUnauthorized)
		return
	}

	keys, err := h.placeholders.ListByClient(client.ID)
	if err != nil {
		jsonError(w, "failed to list keys", http.StatusInternalServerError)
		return
	}

	type envKey struct {
		EnvName     string `json:"env_name"`
		Placeholder string `json:"placeholder"`
		ServiceName string `json:"service_name"`
		KeyPath     string `json:"key_path,omitempty"`
	}

	result := make([]envKey, 0, len(keys))
	for _, k := range keys {
		if k.IsActive {
			keyPath := k.KeyPath
			// Fall back to service's key_directory if not overridden
			if keyPath == "" && h.services != nil {
				svc, err := h.services.GetByID(k.ServiceID)
				if err == nil && svc.KeyDirectory != "" {
					keyPath = svc.KeyDirectory
				}
			}
			result = append(result, envKey{
				EnvName:     k.EnvName,
				Placeholder: k.Placeholder,
				ServiceName: k.ServiceName,
				KeyPath:     keyPath,
			})
		}
	}

	jsonResponse(w, result)
}
