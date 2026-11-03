package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

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
	jsonResponse(w, NotificationViews(list))
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
	case "telegram", "discord", "discord_bot", "webhook":
	default:
		jsonError(w, "channel_type must be telegram, discord, discord_bot, or webhook", http.StatusBadRequest)
		return
	}

	if msg := validateNotificationConfig(req.ChannelType, req.Config); msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	if configHasMaskSentinel(req.ChannelType, req.Config) {
		jsonError(w, "secret fields must contain a real value when creating a channel", http.StatusBadRequest)
		return
	}

	id, err := svc.GenerateToken(16)
	if err != nil {
		jsonError(w, "generate channel ID: "+err.Error(), http.StatusInternalServerError)
		return
	}
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
	jsonResponse(w, notificationView(*ch))
}

func (h *NotificationHandler) Test(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

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

	// Test the specific channel with round-trip verification
	result := h.notifier.TestChannel(*target)
	// Transport errors can echo the request URL (bot token, webhook token).
	result.Message = scrubSecrets(target.ChannelType, target.Config, result.Message)
	jsonResponse(w, result)
}

func (h *NotificationHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name   string `json:"name"`
		Config string `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.Config == "" {
		jsonError(w, "name and config required", http.StatusBadRequest)
		return
	}
	stored, err := h.channels.GetByID(id)
	if err != nil {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}
	// A masked secret keeps the stored value; a new value replaces it.
	config := restoreNotificationConfig(stored.ChannelType, req.Config, stored.Config)
	if msg := validateNotificationConfig(stored.ChannelType, config); msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	if err := h.channels.UpdateFull(id, req.Name, config); err != nil {
		jsonError(w, "failed to update", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

// SetActive enables or disables delivery to a channel without touching its config.
func (h *NotificationHandler) SetActive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Active *bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Active == nil {
		jsonError(w, "active (boolean) is required", http.StatusBadRequest)
		return
	}
	if _, err := h.channels.GetByID(id); err != nil {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}
	if err := h.channels.Update(id, *req.Active); err != nil {
		jsonError(w, "failed to update", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"status": "ok", "is_active": *req.Active})
}

func (h *NotificationHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, err := h.channels.GetByID(id)
	if err != nil {
		jsonError(w, "channel not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, notificationView(*ch))
}

func (h *NotificationHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.channels.Delete(id); err != nil {
		jsonError(w, "failed to delete channel", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
