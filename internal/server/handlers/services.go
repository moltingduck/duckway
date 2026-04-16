package handlers

import (
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

type ServiceHandler struct {
	services *queries.ServiceQueries
}

func NewServiceHandler(services *queries.ServiceQueries) *ServiceHandler {
	return &ServiceHandler{services: services}
}

func (h *ServiceHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.services.List()
	if err != nil {
		jsonError(w, "failed to list services", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.Service{}
	}
	jsonResponse(w, list)
}

func (h *ServiceHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		DisplayName  string `json:"display_name"`
		UpstreamURL  string `json:"upstream_url"`
		HostPattern  string `json:"host_pattern"`
		AuthType     string `json:"auth_type"`
		AuthHeader   string `json:"auth_header"`
		AuthPrefix   string `json:"auth_prefix"`
		KeyPrefix    string `json:"key_prefix"`
		KeyLength    int    `json:"key_length"`
		KeyDirectory string `json:"key_directory"`
		DefaultACL   string `json:"default_acl"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.UpstreamURL == "" {
		jsonError(w, "name and upstream_url are required", http.StatusBadRequest)
		return
	}

	id, _ := services.GenerateToken(16)
	if req.DisplayName == "" {
		req.DisplayName = req.Name
	}
	if req.HostPattern == "" {
		req.HostPattern = req.Name
	}
	if req.AuthType == "" {
		req.AuthType = "bearer"
	}
	if req.AuthHeader == "" {
		req.AuthHeader = "Authorization"
	}
	if req.AuthPrefix == "" && req.AuthType == "bearer" {
		req.AuthPrefix = "Bearer "
	}
	if req.KeyLength == 0 {
		req.KeyLength = 64
	}

	svc := &models.Service{
		ID:           id,
		Name:         req.Name,
		DisplayName:  req.DisplayName,
		UpstreamURL:  req.UpstreamURL,
		HostPattern:  req.HostPattern,
		AuthType:     req.AuthType,
		AuthHeader:   req.AuthHeader,
		AuthPrefix:   req.AuthPrefix,
		KeyPrefix:    req.KeyPrefix,
		KeyLength:    req.KeyLength,
		KeyDirectory: req.KeyDirectory,
		DefaultACL:   req.DefaultACL,
		IsActive:     true,
	}

	if err := h.services.Create(svc); err != nil {
		jsonError(w, "failed to create service: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, svc)
}

func (h *ServiceHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc, err := h.services.GetByID(id)
	if err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, svc)
}

func (h *ServiceHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc, err := h.services.GetByID(id)
	if err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}

	var req struct {
		Name        *string `json:"name"`
		DisplayName *string `json:"display_name"`
		UpstreamURL *string `json:"upstream_url"`
		HostPattern *string `json:"host_pattern"`
		AuthType    *string `json:"auth_type"`
		AuthHeader  *string `json:"auth_header"`
		AuthPrefix  *string `json:"auth_prefix"`
		KeyPrefix    *string `json:"key_prefix"`
		KeyLength    *int    `json:"key_length"`
		KeyDirectory *string `json:"key_directory"`
		DefaultACL   *string `json:"default_acl"`
		IsActive     *bool   `json:"is_active"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name != nil {
		svc.Name = *req.Name
	}
	if req.DisplayName != nil {
		svc.DisplayName = *req.DisplayName
	}
	if req.UpstreamURL != nil {
		svc.UpstreamURL = *req.UpstreamURL
	}
	if req.HostPattern != nil {
		svc.HostPattern = *req.HostPattern
	}
	if req.AuthType != nil {
		svc.AuthType = *req.AuthType
	}
	if req.AuthHeader != nil {
		svc.AuthHeader = *req.AuthHeader
	}
	if req.AuthPrefix != nil {
		svc.AuthPrefix = *req.AuthPrefix
	}
	if req.KeyPrefix != nil {
		svc.KeyPrefix = *req.KeyPrefix
	}
	if req.KeyLength != nil {
		svc.KeyLength = *req.KeyLength
	}
	if req.KeyDirectory != nil {
		svc.KeyDirectory = *req.KeyDirectory
	}
	if req.DefaultACL != nil {
		svc.DefaultACL = *req.DefaultACL
	}
	if req.IsActive != nil {
		svc.IsActive = *req.IsActive
	}

	if err := h.services.Update(svc); err != nil {
		jsonError(w, "failed to update service", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, svc)
}

// ListACLTemplates returns the available ACL templates for a service.
// GET /api/services/{id}/acl-templates
func (h *ServiceHandler) ListACLTemplates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc, err := h.services.GetByID(id)
	if err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}

	templates := services.GetACLTemplates(svc.Name)
	if templates == nil {
		templates = []services.ACLTemplate{}
	}
	jsonResponse(w, map[string]interface{}{
		"service":   svc.Name,
		"current":   svc.DefaultACL,
		"templates": templates,
	})
}

// ApplyACLTemplate sets the service's default_acl to a template's config.
// POST /api/services/{id}/acl-templates  body: {"template_id":"chat-only"}
func (h *ServiceHandler) ApplyACLTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc, err := h.services.GetByID(id)
	if err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}

	var req struct {
		TemplateID string `json:"template_id"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	tmpl := services.GetACLTemplate(svc.Name, req.TemplateID)
	if tmpl == nil {
		jsonError(w, "template not found for service "+svc.Name, http.StatusNotFound)
		return
	}

	svc.DefaultACL = tmpl.Config
	if err := h.services.Update(svc); err != nil {
		jsonError(w, "failed to apply template", http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{
		"status":      "ok",
		"template_id": tmpl.ID,
		"template":    tmpl.Name,
	})
}

func (h *ServiceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.services.Delete(id); err != nil {
		jsonError(w, "failed to delete service", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}
