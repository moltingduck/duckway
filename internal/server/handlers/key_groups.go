package handlers

import (
	"database/sql"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
)

type KeyGroupHandler struct {
	db *sql.DB
}

func NewKeyGroupHandler(db *sql.DB) *KeyGroupHandler {
	return &KeyGroupHandler{db: db}
}

// GET /api/key-groups
func (h *KeyGroupHandler) List(w http.ResponseWriter, r *http.Request) {
	groups, err := queries.ListKeyGroups(h.db)
	if err != nil {
		jsonError(w, "failed to list key groups", http.StatusInternalServerError)
		return
	}
	if groups == nil {
		jsonResponse(w, []struct{}{})
		return
	}
	jsonResponse(w, groups)
}

// POST /api/key-groups
func (h *KeyGroupHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		ServiceName      string `json:"service_name"`
		RotationStrategy string `json:"rotation_strategy"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.ServiceName == "" {
		req.ServiceName = "anthropic"
	}
	if !validStrategy(req.RotationStrategy) {
		req.RotationStrategy = "score"
	}

	group, err := queries.CreateKeyGroup(h.db, req.Name, req.Description, req.ServiceName, req.RotationStrategy)
	if err != nil {
		jsonError(w, "failed to create key group: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, group)
}

// PATCH /api/key-groups/{id}
func (h *KeyGroupHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		RotationStrategy string `json:"rotation_strategy"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validStrategy(req.RotationStrategy) {
		jsonError(w, "rotation_strategy must be one of: score, round_robin, failover, random", http.StatusBadRequest)
		return
	}
	if err := queries.UpdateKeyGroup(h.db, id, req.Name, req.Description, req.RotationStrategy); err != nil {
		jsonError(w, "failed to update key group: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "updated"})
}

// GET /api/key-groups/{id}
func (h *KeyGroupHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	group, err := queries.GetKeyGroupWithMembers(h.db, id)
	if err != nil {
		jsonError(w, "key group not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, group)
}

// DELETE /api/key-groups/{id}
func (h *KeyGroupHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := queries.DeleteKeyGroup(h.db, id); err != nil {
		jsonError(w, "failed to delete key group", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// POST /api/key-groups/{id}/members
func (h *KeyGroupHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	var req struct {
		APIKeyID string `json:"api_key_id"`
		Position int    `json:"position"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.APIKeyID == "" {
		jsonError(w, "api_key_id is required", http.StatusBadRequest)
		return
	}

	if err := queries.AddKeyToGroup(h.db, groupID, req.APIKeyID, req.Position); err != nil {
		jsonError(w, "failed to add key to group: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "added"})
}

// PATCH /api/key-groups/{id}/members/{key_id}
func (h *KeyGroupHandler) UpdateMember(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	keyID := r.PathValue("key_id")
	var req struct {
		Position int `json:"position"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := queries.UpdateMemberPosition(h.db, groupID, keyID, req.Position); err != nil {
		jsonError(w, "failed to update member position: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "updated"})
}

// DELETE /api/key-groups/{id}/members/{key_id}
func (h *KeyGroupHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	keyID := r.PathValue("key_id")
	if err := queries.RemoveKeyFromGroup(h.db, groupID, keyID); err != nil {
		jsonError(w, "failed to remove key from group", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "removed"})
}

// POST /client/usage — sidecar reports rate-limit headers after each Anthropic response.
// Body: {"api_key_id": "...", "headers": {"x-ratelimit-remaining-tokens": "45000", ...}}
func (h *KeyGroupHandler) ReportUsage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKeyID string            `json:"api_key_id"`
		Headers  map[string]string `json:"headers"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.APIKeyID == "" || len(req.Headers) == 0 {
		jsonResponse(w, map[string]string{"status": "noop"})
		return
	}

	if err := queries.UpdateAnthropicUsage(h.db, req.APIKeyID, req.Headers); err != nil {
		jsonError(w, "failed to update usage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

func validStrategy(s string) bool {
	switch s {
	case "score", "round_robin", "failover", "random":
		return true
	}
	return false
}
