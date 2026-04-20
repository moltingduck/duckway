package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type NotificationHandler struct {
	channels *queries.NotificationQueries
	notifier *svc.Notifier
}

func NewNotificationHandler(channels *queries.NotificationQueries, notifier *svc.Notifier) *NotificationHandler {
	return &NotificationHandler{channels: channels, notifier: notifier}
}

func (h *NotificationHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.channels.List()
	if err != nil {
		jsonError(w, "failed to list channels", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []queries.NotificationChannel{}
	}
	jsonResponse(w, list)
}

func (h *NotificationHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelType string `json:"channel_type"` // telegram, discord, webhook
		Name        string `json:"name"`
		Config      string `json:"config"` // JSON string
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ChannelType == "" || req.Name == "" || req.Config == "" {
		jsonError(w, "channel_type, name, and config are required", http.StatusBadRequest)
		return
	}

	// Validate channel type
	switch req.ChannelType {
	case "telegram", "discord", "webhook":
	default:
		jsonError(w, "channel_type must be telegram, discord, or webhook", http.StatusBadRequest)
		return
	}

	// Validate config is valid JSON
	var configMap map[string]interface{}
	if err := json.Unmarshal([]byte(req.Config), &configMap); err != nil {
		jsonError(w, "config must be valid JSON", http.StatusBadRequest)
		return
	}

	id, _ := svc.GenerateToken(16)
	ch := &queries.NotificationChannel{
		ID:          id,
		ChannelType: req.ChannelType,
		Name:        req.Name,
		Config:      req.Config,
		IsActive:    true,
	}

	if err := h.channels.Create(ch); err != nil {
		jsonError(w, "failed to create channel: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, ch)
}

func (h *NotificationHandler) Test(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Find the channel
	channels, err := h.channels.List()
	if err != nil {
		jsonError(w, "failed to list channels", http.StatusInternalServerError)
		return
	}

	var target *queries.NotificationChannel
	for i := range channels {
		if channels[i].ID == id {
			target = &channels[i]
			break
		}
	}
	if target == nil {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}

	// Send a test notification through the notifier
	h.notifier.NotifyApprovalNeeded(svc.ApprovalNotification{
		ApprovalID:    "test-000",
		PlaceholderID: "test-placeholder",
		ClientName:    "test-client",
		ServiceName:   "test-service",
		Method:        "POST",
		Path:          "/v1/test",
		AdminURL:      "/admin/approvals",
	})

	jsonResponse(w, map[string]string{"status": "sent"})
}

func (h *NotificationHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.channels.Delete(id); err != nil {
		jsonError(w, "failed to delete channel", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}
