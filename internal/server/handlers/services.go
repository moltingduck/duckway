package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

type ServiceHandler struct {
	services *queries.ServiceQueries
	pricing  *queries.ModelPricingQueries
}

func NewServiceHandler(services *queries.ServiceQueries, pricing ...*queries.ModelPricingQueries) *ServiceHandler {
	h := &ServiceHandler{services: services}
	if len(pricing) > 0 {
		h.pricing = pricing[0]
	}
	return h
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
		Name          string `json:"name"`
		DisplayName   string `json:"display_name"`
		UpstreamURL   string `json:"upstream_url"`
		HostPattern   string `json:"host_pattern"`
		AuthType      string `json:"auth_type"`
		AuthHeader    string `json:"auth_header"`
		AuthPrefix    string `json:"auth_prefix"`
		KeyPrefix     string `json:"key_prefix"`
		KeyLength     int    `json:"key_length"`
		KeyDirectory  string `json:"key_directory"`
		DefaultACL    string `json:"default_acl"`
		Category      string `json:"category"`
		UsageMetering string `json:"usage_metering"`
		DeliveryMode  string `json:"delivery_mode"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.UpstreamURL == "" {
		jsonError(w, "name and upstream_url are required", http.StatusBadRequest)
		return
	}
	if req.DeliveryMode != "" && req.DeliveryMode != "proxy" && req.DeliveryMode != "loan_proxy" {
		jsonError(w, "delivery_mode must be 'proxy' or 'loan_proxy'", http.StatusBadRequest)
		return
	}
	if err := services.ValidatePermissionConfig(req.DefaultACL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateUsageMetering(req.UsageMetering); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
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

	deliveryMode := req.DeliveryMode
	if deliveryMode == "" {
		deliveryMode = "proxy"
	}
	svc := &models.Service{
		ID:            id,
		Name:          req.Name,
		DisplayName:   req.DisplayName,
		UpstreamURL:   req.UpstreamURL,
		HostPattern:   req.HostPattern,
		AuthType:      req.AuthType,
		AuthHeader:    req.AuthHeader,
		AuthPrefix:    req.AuthPrefix,
		KeyPrefix:     req.KeyPrefix,
		KeyLength:     req.KeyLength,
		KeyDirectory:  req.KeyDirectory,
		DefaultACL:    req.DefaultACL,
		Category:      strings.TrimSpace(req.Category),
		UsageMetering: strings.TrimSpace(req.UsageMetering),
		DeliveryMode:  deliveryMode,
		IsActive:      true,
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
		Name          *string `json:"name"`
		DisplayName   *string `json:"display_name"`
		UpstreamURL   *string `json:"upstream_url"`
		HostPattern   *string `json:"host_pattern"`
		AuthType      *string `json:"auth_type"`
		AuthHeader    *string `json:"auth_header"`
		AuthPrefix    *string `json:"auth_prefix"`
		KeyPrefix     *string `json:"key_prefix"`
		KeyLength     *int    `json:"key_length"`
		KeyDirectory  *string `json:"key_directory"`
		DefaultACL    *string `json:"default_acl"`
		Category      *string `json:"category"`
		UsageMetering *string `json:"usage_metering"`
		DeliveryMode  *string `json:"delivery_mode"`
		IsActive      *bool   `json:"is_active"`
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
		if err := services.ValidatePermissionConfig(*req.DefaultACL); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		svc.DefaultACL = *req.DefaultACL
	}
	if req.Category != nil {
		svc.Category = strings.TrimSpace(*req.Category)
	}
	if req.UsageMetering != nil {
		if err := validateUsageMetering(*req.UsageMetering); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		svc.UsageMetering = strings.TrimSpace(*req.UsageMetering)
	}
	if req.DeliveryMode != nil {
		if *req.DeliveryMode != "proxy" && *req.DeliveryMode != "loan_proxy" {
			jsonError(w, "delivery_mode must be 'proxy' or 'loan_proxy'", http.StatusBadRequest)
			return
		}
		svc.DeliveryMode = *req.DeliveryMode
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

func validateUsageMetering(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var value map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &value); err != nil || value == nil {
		return &usageMetadataError{}
	}
	return nil
}

type usageMetadataError struct{}

func (*usageMetadataError) Error() string { return "usage_metering must be a JSON object" }

// ListPricing returns immutable price versions for one service.
func (h *ServiceHandler) ListPricing(w http.ResponseWriter, r *http.Request) {
	serviceID := r.PathValue("id")
	if _, err := h.services.GetByID(serviceID); err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}
	if h.pricing == nil {
		jsonResponse(w, []models.ModelPricing{})
		return
	}
	rows, err := h.pricing.ListByService(serviceID)
	if err != nil {
		jsonError(w, "failed to list model pricing", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, rows)
}

// CreatePricing appends a price version; existing versions are never updated.
func (h *ServiceHandler) CreatePricing(w http.ResponseWriter, r *http.Request) {
	serviceID := r.PathValue("id")
	if _, err := h.services.GetByID(serviceID); err != nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}
	if h.pricing == nil {
		jsonError(w, "pricing unavailable", http.StatusInternalServerError)
		return
	}
	var req struct {
		Model                         string `json:"model"`
		Version                       string `json:"version"`
		InputUSDMicrosPerMTok         int64  `json:"input_usd_micros_per_mtok"`
		OutputUSDMicrosPerMTok        int64  `json:"output_usd_micros_per_mtok"`
		CacheReadUSDMicrosPerMTok     int64  `json:"cache_read_usd_micros_per_mtok"`
		CacheCreationUSDMicrosPerMTok int64  `json:"cache_creation_usd_micros_per_mtok"`
		ReasoningUSDMicrosPerMTok     int64  `json:"reasoning_usd_micros_per_mtok"`
		EffectiveFrom                 string `json:"effective_from"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Model, req.Version = strings.TrimSpace(req.Model), strings.TrimSpace(req.Version)
	if req.Model == "" || req.Version == "" {
		jsonError(w, "model and version are required", http.StatusBadRequest)
		return
	}
	if req.InputUSDMicrosPerMTok < 0 || req.OutputUSDMicrosPerMTok < 0 ||
		req.CacheReadUSDMicrosPerMTok < 0 || req.CacheCreationUSDMicrosPerMTok < 0 ||
		req.ReasoningUSDMicrosPerMTok < 0 {
		jsonError(w, "pricing rates must be non-negative", http.StatusBadRequest)
		return
	}
	effectiveAt := time.Now().UTC()
	if strings.TrimSpace(req.EffectiveFrom) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(req.EffectiveFrom))
		if err != nil {
			jsonError(w, "effective_from must be RFC3339", http.StatusBadRequest)
			return
		}
		effectiveAt = parsed.UTC()
	}
	id, err := services.GenerateToken(16)
	if err != nil {
		jsonError(w, "failed to generate pricing id", http.StatusInternalServerError)
		return
	}
	pricing := &models.ModelPricing{
		ID: id, ServiceID: serviceID, Model: req.Model, Version: req.Version,
		InputUSDMicrosPerMTok:         req.InputUSDMicrosPerMTok,
		OutputUSDMicrosPerMTok:        req.OutputUSDMicrosPerMTok,
		CacheReadUSDMicrosPerMTok:     req.CacheReadUSDMicrosPerMTok,
		CacheCreationUSDMicrosPerMTok: req.CacheCreationUSDMicrosPerMTok,
		ReasoningUSDMicrosPerMTok:     req.ReasoningUSDMicrosPerMTok,
		EffectiveFrom:                 effectiveAt.Format(time.RFC3339),
	}
	if err := h.pricing.Create(pricing); err != nil {
		jsonError(w, "failed to create model pricing", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, pricing)
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
	w.WriteHeader(http.StatusOK)
}
