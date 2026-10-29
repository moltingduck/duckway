package handlers

import "net/http"

// Plugins renders server-managed plugins. Catalog data is loaded through the
// session-authenticated admin API, so rendering never embeds source data or tokens.
func (h *AdminHandler) Plugins(w http.ResponseWriter, _ *http.Request) {
	h.render(w, "plugins", pageData{Title: "Plugins", Active: "plugins"})
}
