package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/serverplugin"
)

type PluginHandler struct {
	store     *serverplugin.Store
	configErr error
	clients   *queries.ClientQueries
}

func NewPluginHandler(dataDir string) *PluginHandler {
	store, err := serverplugin.New(dataDir, os.Getenv("DUCKWAY_PLUGIN_ALLOWED_ORIGINS"))
	return &PluginHandler{store: store, configErr: err}
}
func (h *PluginHandler) WithClients(clients *queries.ClientQueries) *PluginHandler {
	h.clients = clients
	return h
}
func (h *PluginHandler) available(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if h.configErr != nil || h.store == nil {
		JsonErrorPublic(w, "plugin storage is unavailable", 503)
		return false
	}
	return true
}
func pluginError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	message := "plugin operation failed"
	if errors.Is(err, serverplugin.ErrNotFound) {
		status = http.StatusNotFound
		message = "plugin is unavailable"
	}
	JsonErrorPublic(w, message, status)
}

// Mutations use cookie authentication and require a browser origin check in addition to JSON.
// CLI administrators send Origin: <admin origin> explicitly alongside their session cookie.
func pluginMutation(w http.ResponseWriter, r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		JsonErrorPublic(w, "application/json is required", 415)
		return false
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	expected, err := url.Parse(scheme + "://" + r.Host)
	if err != nil || expected.Hostname() == "" {
		JsonErrorPublic(w, "same-origin request required", 403)
		return false
	}
	raw := r.Header.Get("Origin")
	isOrigin := raw != ""
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.User != nil || u.Scheme != expected.Scheme || !strings.EqualFold(u.Host, expected.Host) || u.Fragment != "" || (isOrigin && (u.Path != "" && u.Path != "/" || u.RawQuery != "")) {
		JsonErrorPublic(w, "same-origin request required", 403)
		return false
	}
	return true
}
func pluginJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	data, err := io.ReadAll(r.Body)
	if err != nil || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		JsonErrorPublic(w, "invalid plugin request", 400)
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		JsonErrorPublic(w, "invalid plugin request", 400)
		return false
	}
	return true
}
func (h *PluginHandler) AdminList(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	list, err := h.store.List(r.Context(), true)
	if err != nil {
		pluginError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, e.AdminSummary())
	}
	JsonResponsePublic(w, out)
}
func (h *PluginHandler) Import(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) || !pluginMutation(w, r) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		ManifestURL string `json:"manifest_url"`
	}
	if !pluginJSON(w, r, &req) {
		return
	}
	m, err := h.store.Import(r.Context(), req.Name, req.ManifestURL)
	if err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, m)
}
func (h *PluginHandler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if !h.available(w) || !pluginMutation(w, r) {
		return
	}
	var req struct{}
	if !pluginJSON(w, r, &req) {
		return
	}
	if err := h.store.SetEnabled(r.Context(), r.PathValue("name"), enabled); err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, map[string]bool{"enabled": enabled})
}
func (h *PluginHandler) Disable(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }
func (h *PluginHandler) Enable(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, true) }
func (h *PluginHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	m, err := h.store.Catalog(r.Context())
	if err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, m)
}
func (h *PluginHandler) Manifest(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	m, err := h.store.Manifest(r.Context(), r.PathValue("name"))
	if err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, m)
}
func (h *PluginHandler) Artifact(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	f, size, err := h.store.Artifact(r.Context(), r.PathValue("name"), r.PathValue("version"), r.PathValue("os"), r.PathValue("arch"))
	if err != nil {
		pluginError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	http.ServeContent(w, r, "plugin", time.Time{}, f)
}

func (h *PluginHandler) Check(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) || !pluginMutation(w, r) {
		return
	}
	var req struct{}
	if !pluginJSON(w, r, &req) {
		return
	}
	m, err := h.store.Check(r.Context(), r.PathValue("name"))
	if err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, m)
}
func (h *PluginHandler) Publish(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) || !pluginMutation(w, r) {
		return
	}
	var req struct {
		Version        string `json:"version"`
		ManifestDigest string `json:"manifest_digest"`
	}
	if !pluginJSON(w, r, &req) {
		return
	}
	m, err := h.store.Publish(r.Context(), r.PathValue("name"), req.Version, req.ManifestDigest)
	if err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, m)
}
func (h *PluginHandler) Report(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	client := middleware.GetClient(r)
	if client == nil {
		JsonErrorPublic(w, "client authentication required", 401)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		JsonErrorPublic(w, "application/json is required", 415)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var report serverplugin.Report
	if dec.Decode(&report) != nil || dec.Decode(new(any)) != io.EOF || report.Validate() != nil {
		JsonErrorPublic(w, "invalid plugin report", 400)
		return
	}
	if err := h.store.ReceiveReport(r.Context(), client.ID, report); err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, map[string]string{"status": "ok"})
}
func (h *PluginHandler) Clients(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	if h.clients == nil {
		JsonErrorPublic(w, "client status is unavailable", 503)
		return
	}
	clients, err := h.clients.List()
	if err != nil {
		JsonErrorPublic(w, "client status is unavailable", 503)
		return
	}
	ids := make([]serverplugin.ClientIdentity, 0, len(clients))
	for _, client := range clients {
		ids = append(ids, serverplugin.ClientIdentity{ID: client.ID, Name: client.Name})
	}
	rows, err := h.store.ClientStates(r.Context(), r.PathValue("name"), ids, time.Now())
	if err != nil {
		pluginError(w, err)
		return
	}
	JsonResponsePublic(w, rows)
}
