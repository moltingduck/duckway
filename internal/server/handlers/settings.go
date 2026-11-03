package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// MaxStatuslineScriptBytes bounds the agent statusline script body.
const MaxStatuslineScriptBytes = 64 * 1024

// MinAdminPasswordLen is the minimum length enforced by change-password.
const MinAdminPasswordLen = 12

// SettingsHandler serves the key/value settings API. Known keys are validated
// server-side (the UI mirrors these rules but this code is authoritative);
// other keys written by other pages pass through unchanged.
type SettingsHandler struct {
	settings *queries.SettingsQueries
}

func NewSettingsHandler(s *queries.SettingsQueries) *SettingsHandler {
	return &SettingsHandler{settings: s}
}

func (h *SettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, h.settings.GetAll())
}

// ValidateSetting returns an error message for an invalid known setting, or "".
func ValidateSetting(key, value string) string {
	switch key {
	case queries.SettingGatewayURL:
		v := strings.TrimSpace(value)
		if v == "" {
			return ""
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "Gateway URL must start with http:// or https:// and include a host"
		}
	case queries.SettingProxyPort:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 1024 || n > 65535 {
			return "Client proxy port must be a number from 1024 to 65535"
		}
	case queries.SettingApprovalRequestTTL:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 1 || n > 10080 {
			return "Approval timeout must be a whole number of minutes from 1 to 10080"
		}
	case queries.SettingAgentStatuslineScript:
		if len(value) > MaxStatuslineScriptBytes {
			return "Statusline script must be 64 KB or smaller"
		}
	}
	return ""
}

func (h *SettingsHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	// Validate everything before writing anything.
	for k, v := range req {
		if msg := ValidateSetting(k, v); msg != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": msg, "field": k})
			return
		}
	}
	for k, v := range req {
		switch k {
		case queries.SettingGatewayURL, queries.SettingProxyPort, queries.SettingApprovalRequestTTL:
			v = strings.TrimSpace(v)
		}
		if err := h.settings.Set(k, v); err != nil {
			jsonError(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}
