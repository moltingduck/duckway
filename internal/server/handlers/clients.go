package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type ClientHandler struct {
	clients      *queries.ClientQueries
	placeholders *queries.PlaceholderQueries
}

func NewClientHandler(clients *queries.ClientQueries, placeholders *queries.PlaceholderQueries) *ClientHandler {
	return &ClientHandler{clients: clients, placeholders: placeholders}
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
	var name string

	ct := r.Header.Get("Content-Type")
	if ct == "application/x-www-form-urlencoded" || strings.HasPrefix(ct, "multipart/form-data") {
		r.ParseForm()
		name = r.FormValue("name")
	} else {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		name = req.Name
	}

	if name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	token, err := svc.GenerateToken(32)
	if err != nil {
		jsonError(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	id, _ := svc.GenerateToken(16)
	client := &models.Client{
		ID:        id,
		Name:      name,
		TokenHash: svc.HashToken(token),
		IsActive:  true,
	}

	if err := h.clients.Create(client); err != nil {
		jsonError(w, "failed to create client: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]string{
		"id":    id,
		"name":  name,
		"token": token, // Shown only once
	})
}

// Admin: delete a client
func (h *ClientHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.clients.Delete(id); err != nil {
		jsonError(w, "failed to delete client", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
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

	// Return in env-friendly format
	type envKey struct {
		EnvName     string `json:"env_name"`
		Placeholder string `json:"placeholder"`
		ServiceName string `json:"service_name"`
	}

	result := make([]envKey, 0, len(keys))
	for _, k := range keys {
		if k.IsActive {
			result = append(result, envKey{
				EnvName:     k.EnvName,
				Placeholder: k.Placeholder,
				ServiceName: k.ServiceName,
			})
		}
	}

	jsonResponse(w, result)
}
