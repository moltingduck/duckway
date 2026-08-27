package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type clientEnvKey struct {
	EnvName          string `json:"env_name"`
	Placeholder      string `json:"placeholder"`
	ServiceName      string `json:"service_name"`
	KeyPath          string `json:"key_path,omitempty"`
	PermissionConfig string `json:"permission_config,omitempty"`
}

type clientServiceRoute struct {
	Name         string `json:"name"`
	HostPattern  string `json:"host_pattern"`
	UpstreamURL  string `json:"upstream_url"`
	DeliveryMode string `json:"delivery_mode"`
	Assigned     bool   `json:"assigned"`
}

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

// Admin: get single client
func (h *ClientHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	client, err := h.clients.GetByID(id)
	if err != nil {
		jsonError(w, "client not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, client)
}

// Admin: update client
func (h *ClientHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	client, err := h.clients.GetByID(id)
	if err != nil {
		jsonError(w, "client not found", http.StatusNotFound)
		return
	}

	var req struct {
		Name         *string `json:"name"`
		IsActive     *bool   `json:"is_active"`
		UpdatePolicy *string `json:"update_policy"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if req.Name != nil {
		client.Name = *req.Name
	}
	if req.IsActive != nil {
		client.IsActive = *req.IsActive
	}
	if req.UpdatePolicy != nil {
		if *req.UpdatePolicy != "managed" && *req.UpdatePolicy != "manual" {
			jsonError(w, "update_policy must be managed or manual", http.StatusBadRequest)
			return
		}
		client.UpdatePolicy = *req.UpdatePolicy
	}

	if err := h.clients.Update(client); err != nil {
		jsonError(w, "failed to update client", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, client)
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

	result, err := h.clientKeys(client.ID)
	if err != nil {
		jsonError(w, "failed to list keys", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, result)
}

// GetSync returns one client-scoped snapshot so placeholder credentials and
// routing policy are installed atomically by the sidecar.
func (h *ClientHandler) GetSync(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client not found in context", http.StatusUnauthorized)
		return
	}
	keys, err := h.clientKeys(client.ID)
	if err != nil {
		jsonError(w, "failed to list keys", http.StatusInternalServerError)
		return
	}
	services, err := h.services.List()
	if err != nil {
		jsonError(w, "failed to list services", http.StatusInternalServerError)
		return
	}
	assigned, err := h.placeholders.ActiveServiceIDsByClient(client.ID)
	if err != nil {
		jsonError(w, "failed to resolve assignments", http.StatusInternalServerError)
		return
	}
	routes := make([]clientServiceRoute, 0, len(services))
	for _, service := range services {
		if !service.IsActive || service.Name == "duckway-internal" {
			continue
		}
		routes = append(routes, clientServiceRoute{
			Name: service.Name, HostPattern: service.HostPattern,
			UpstreamURL: service.UpstreamURL, DeliveryMode: service.DeliveryMode,
			Assigned: assigned[service.ID],
		})
	}
	payload := struct {
		Keys     []clientEnvKey       `json:"keys"`
		Services []clientServiceRoute `json:"services"`
	}{Keys: keys, Services: routes}
	body, _ := json.Marshal(payload)
	revision := fmt.Sprintf("%x", sha256.Sum256(body))
	jsonResponse(w, struct {
		Revision string               `json:"revision"`
		Keys     []clientEnvKey       `json:"keys"`
		Services []clientServiceRoute `json:"services"`
	}{Revision: revision, Keys: keys, Services: routes})
}

func (h *ClientHandler) clientKeys(clientID string) ([]clientEnvKey, error) {
	keys, err := h.placeholders.ListByClient(clientID)
	if err != nil {
		return nil, err
	}

	result := make([]clientEnvKey, 0, len(keys))
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
			out := clientEnvKey{
				EnvName:     k.EnvName,
				Placeholder: k.Placeholder,
				ServiceName: k.ServiceName,
				KeyPath:     keyPath,
			}
			if k.PermissionConfig != nil {
				out.PermissionConfig = *k.PermissionConfig
			}
			result = append(result, out)
		}
	}
	return result, nil
}
