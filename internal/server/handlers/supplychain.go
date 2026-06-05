package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/services"
)

// SupplyChainHandler serves the admin view/toggle API and the client endpoint
// that hands agents the package-manager rc-file hardening settings.
type SupplyChainHandler struct {
	settings *queries.SettingsQueries
}

func NewSupplyChainHandler(settings *queries.SettingsQueries) *SupplyChainHandler {
	return &SupplyChainHandler{settings: settings}
}

// mitigationView is one row in the admin list — the static spec plus the
// current enabled flag and the rc lines that would be written right now.
type mitigationView struct {
	services.SupplyChainMitigation
	Enabled      bool     `json:"enabled"`
	ExampleLines []string `json:"example_lines"`
}

// List — GET /api/supply-chain. Returns the cooldown and every mitigation with
// its current enabled state and the rc lines it would write now.
func (h *SupplyChainHandler) List(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	days := services.SupplyChainMinAgeDays(h.settings.Get)
	views := make([]mitigationView, 0)
	for _, m := range services.SupplyChainMitigations() {
		v := mitigationView{
			SupplyChainMitigation: m,
			Enabled:               services.SupplyChainEnabled(h.settings.Get, m.ID),
		}
		if m.Supported {
			v.ExampleLines = m.RCLines(days, now)
		}
		views = append(views, v)
	}
	jsonResponse(w, map[string]interface{}{
		"min_age_days": days,
		"mitigations":  views,
	})
}

// Toggle — POST /api/supply-chain/{id}. Body: {"enabled": bool}. Only known,
// supported mitigations can be toggled.
func (h *SupplyChainHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var known bool
	for _, m := range services.SupplyChainMitigations() {
		if m.ID == id && m.Supported {
			known = true
			break
		}
	}
	if !known {
		jsonError(w, "unknown or unsupported mitigation: "+id, http.StatusNotFound)
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	val := "0"
	if req.Enabled {
		val = "1"
	}
	if err := h.settings.Set(services.SettingKeySupplyChainEnabled(id), val); err != nil {
		jsonError(w, "save failed", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"id": id, "enabled": req.Enabled})
}

// SetMinAge — POST /api/supply-chain/min-age. Body: {"days": int}. The global
// cooldown applied to every enabled mitigation.
func (h *SupplyChainHandler) SetMinAge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Days <= 0 {
		jsonError(w, "days must be a positive integer", http.StatusBadRequest)
		return
	}
	if err := h.settings.Set(services.SettingKeySupplyChainMinAgeDays(), strconv.Itoa(req.Days)); err != nil {
		jsonError(w, "save failed", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"min_age_days": req.Days})
}

// ClientRC — GET /client/supply-chain-rc. Returns the rc lines to write,
// grouped by rc file path (relative to $HOME), resolved at request time so
// rolling-date cutoffs stay fresh. The client merges each into a managed block
// in the corresponding agent rc file.
func (h *SupplyChainHandler) ClientRC(w http.ResponseWriter, r *http.Request) {
	rc := services.ResolveSupplyChainRC(h.settings.Get, time.Now())
	jsonResponse(w, rc)
}
