package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
)

// WhatsNewVersion identifies the current release-notes dialog content. It is a
// content version, not the build version (which changes on every commit), so
// the dialog only reappears when this constant is bumped for a new release.
const WhatsNewVersion = "2026.11"

const (
	whatsNewSettingPrefix = "whats_new_seen_version:"
	whatsNewCookie        = "duckway_whatsnew"
)

// WhatsNewMove is one "where things went" row.
type WhatsNewMove struct {
	Old  string
	New  string
	Path string
}

// whatsNewMoves mirrors the real new locations registered in movedPages; a test
// keeps every Path in sync with that map.
var whatsNewMoves = []WhatsNewMove{
	{Old: "API Keys, Refreshable Tokens", New: "Credentials", Path: "/admin/credentials"},
	{Old: "Key Groups (both kinds)", New: "Key pools", Path: "/admin/key-pools"},
	{Old: "Key Suites", New: "Key bundles", Path: "/admin/key-bundles"},
	{Old: "Placeholders", New: "Access › Phantom tokens", Path: "/admin/phantom-tokens"},
	{Old: "Onboard Client", New: "Clients › Add client", Path: "/admin/clients/new"},
	{Old: "Client updates", New: "Clients › Rollouts", Path: "/admin/clients/rollouts"},
	{Old: "Dashboard", New: "Overview (now shows what needs you)", Path: "/admin/"},
}

func whatsNewKey(username string) string { return whatsNewSettingPrefix + username }

// WhatsNewHandler persists the "Don't show again" choice per admin user.
type WhatsNewHandler struct {
	settings *queries.SettingsQueries
}

func NewWhatsNewHandler(s *queries.SettingsQueries) *WhatsNewHandler {
	return &WhatsNewHandler{settings: s}
}

// Dismiss handles POST /api/whats-new/dismiss {"version":"..."}. The admin-only
// /api/ mux has already authenticated the caller. Only the current version can
// be recorded, so a client cannot suppress a future release's dialog.
func (h *WhatsNewHandler) Dismiss(w http.ResponseWriter, r *http.Request) {
	username, _ := r.Context().Value(middleware.AdminUserKey).(string)
	if username == "" {
		jsonError(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Version != WhatsNewVersion {
		jsonError(w, "unknown version", http.StatusBadRequest)
		return
	}
	if err := h.settings.Set(whatsNewKey(username), WhatsNewVersion); err != nil {
		jsonError(w, "could not save", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok", "version": WhatsNewVersion})
}

func clearWhatsNewCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: whatsNewCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// WhatsNewMovesForTest exposes the moves list to the consistency test.
func WhatsNewMovesForTest() []WhatsNewMove { return whatsNewMoves }
