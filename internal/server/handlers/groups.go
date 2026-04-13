package handlers

import (
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type GroupHandler struct {
	groups   *queries.GroupQueries
	services *queries.ServiceQueries
}

func NewGroupHandler(groups *queries.GroupQueries, services *queries.ServiceQueries) *GroupHandler {
	return &GroupHandler{groups: groups, services: services}
}

func (h *GroupHandler) List(w http.ResponseWriter, r *http.Request) {
	serviceID := r.URL.Query().Get("service_id")
	list, err := h.groups.List(serviceID)
	if err != nil {
		jsonError(w, "failed to list groups", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.APIKeyGroup{}
	}
	// Load members for each group
	for i := range list {
		members, _ := h.groups.GetMembers(list[i].ID)
		list[i].Members = members
		// Redact encrypted keys
		for j := range list[i].Members {
			list[i].Members[j].KeyEncrypted = ""
		}
	}
	jsonResponse(w, list)
}

func (h *GroupHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		ServiceID string `json:"service_id"`
		Strategy  string `json:"strategy"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.ServiceID == "" {
		jsonError(w, "name and service_id are required", http.StatusBadRequest)
		return
	}
	if req.Strategy == "" {
		req.Strategy = "round-robin"
	}

	id, _ := svc.GenerateToken(16)
	group := &models.APIKeyGroup{
		ID:        id,
		ServiceID: req.ServiceID,
		Name:      req.Name,
		Strategy:  req.Strategy,
	}

	if err := h.groups.Create(group); err != nil {
		jsonError(w, "failed to create group: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, group)
}

func (h *GroupHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.groups.Delete(id); err != nil {
		jsonError(w, "failed to delete group", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}

func (h *GroupHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	var req struct {
		APIKeyID string `json:"api_key_id"`
		Priority int    `json:"priority"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.APIKeyID == "" {
		jsonError(w, "api_key_id is required", http.StatusBadRequest)
		return
	}

	if err := h.groups.AddMember(groupID, req.APIKeyID, req.Priority); err != nil {
		jsonError(w, "failed to add member", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "added"})
}

func (h *GroupHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	apiKeyID := r.PathValue("keyId")
	if err := h.groups.RemoveMember(groupID, apiKeyID); err != nil {
		jsonError(w, "failed to remove member", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "removed"})
}
