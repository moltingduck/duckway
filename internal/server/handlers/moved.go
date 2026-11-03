package handlers

import (
	"net/http"
	"net/url"
	"sort"
)

// MovedPage describes where a removed console page now lives.
type MovedPage struct {
	NewPath string // absolute console path of the new location
	NewName string // human-readable location, e.g. "Clients › Rollouts"
}

// MovedNotice is the data rendered by the page-moved 404 template.
type MovedNotice struct {
	OldPath string
	NewPath string
	NewName string
}

// movedPages maps removed console paths to their new location. Later
// batches append entries here; no redirects are issued, the old URL answers
// 404 with a notice inside the console shell.
var movedPages = map[string]MovedPage{
	"/admin/client-updates": {NewPath: "/admin/clients/rollouts", NewName: "Clients › Rollouts"},
	"/admin/onboarding":     {NewPath: "/admin/clients/new", NewName: "Clients › Add client"},
	"/admin/groups":         {NewPath: "/admin/key-pools", NewName: "Key pools"},
	"/admin/key-groups":     {NewPath: "/admin/key-pools", NewName: "Key pools"},
	"/admin/key-suites":     {NewPath: "/admin/key-bundles", NewName: "Key bundles"},
}

// movedKeyGroupPattern is the one removed path that carries an identifier:
// the old quota-aware lease group detail page. It answers the same 404 notice
// and links straight to the pool's deep link on the new page.
const movedKeyGroupPattern = "GET /admin/key-groups/{id}"

// MovedPaths returns the registered old paths in sorted order.
func MovedPaths() []string {
	paths := make([]string, 0, len(movedPages))
	for p := range movedPages {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// LookupMoved returns the new location for a removed path.
func LookupMoved(oldPath string) (MovedPage, bool) {
	m, ok := movedPages[oldPath]
	return m, ok
}

// RegisterMovedPages registers a page-moved 404 handler for every entry in
// movedPages on the (admin-authenticated) page mux.
func (h *AdminHandler) RegisterMovedPages(mux *http.ServeMux) {
	for _, oldPath := range MovedPaths() {
		oldPath := oldPath
		mux.HandleFunc("GET "+oldPath, func(w http.ResponseWriter, r *http.Request) {
			h.MovedPage(w, r, oldPath)
		})
	}
	mux.HandleFunc(movedKeyGroupPattern, h.MovedKeyGroupDetail)
}

// MovedKeyGroupDetail renders the page-moved notice for /admin/key-groups/{id}.
func (h *AdminHandler) MovedKeyGroupDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.renderMoved(w, r, r.URL.Path, MovedPage{
		NewPath: "/admin/key-pools?pool=" + url.QueryEscape("lease:"+id),
		NewName: "Key pools",
	})
}

// MovedPage renders the page-moved notice with status 404.
func (h *AdminHandler) MovedPage(w http.ResponseWriter, r *http.Request, oldPath string) {
	m, ok := LookupMoved(oldPath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.renderMoved(w, r, oldPath, m)
}

func (h *AdminHandler) renderMoved(w http.ResponseWriter, r *http.Request, oldPath string, m MovedPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	h.render(w, r, "moved", pageData{
		Title:  "Page moved",
		Active: "",
		Moved:  &MovedNotice{OldPath: oldPath, NewPath: m.NewPath, NewName: m.NewName},
	})
}
