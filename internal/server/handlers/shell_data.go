package handlers

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/version"
)

// Crumb is one breadcrumb item. Href is empty for items that are not links
// (the sidebar group and the current page).
type Crumb struct {
	Label   string
	Href    string
	Current bool
}

// ShellData feeds the layout chrome: top-bar breadcrumb, sidebar brand
// sub-line and the admin user chip.
type ShellData struct {
	Crumbs      []Crumb
	AdminUser   string
	Initial     string
	GatewayHost string
	Version     string
}

type navEntry struct{ Section, Label, Href string }

// navEntries maps a pageData.Active key to its sidebar group and link, so the
// breadcrumb always matches the sidebar.
var navEntries = map[string]navEntry{
	"dashboard":     {"Overview", "Overview", "/admin/"},
	"approvals":     {"Overview", "Approvals", "/admin/approvals"},
	"clients":       {"Fleet", "Clients", "/admin/clients"},
	"plugins":       {"Fleet", "Plugins", "/admin/plugins"},
	"cc":            {"Fleet", "Control channels", "/admin/cc"},
	"services":      {"Credentials", "Services", "/admin/services"},
	"credentials":   {"Credentials", "Credentials", "/admin/credentials"},
	"key_pools":     {"Credentials", "Key pools", "/admin/key-pools"},
	"key_bundles":   {"Credentials", "Key bundles", "/admin/key-bundles"},
	"placeholders":  {"Access", "Phantom tokens", "/admin/phantom-tokens"},
	"logs":          {"Observe", "Request log", "/admin/logs"},
	"usage":         {"Observe", "Usage", "/admin/usage"},
	"canary":        {"Security", "Canary tokens", "/admin/canary"},
	"supplychain":   {"Security", "Supply chain", "/admin/supply-chain"},
	"notifications": {"System", "Notifications", "/admin/notifications"},
	"settings":      {"System", "Settings", "/admin/settings"},
	"docs":          {"System", "Docs", "/admin/docs"},
}

// detailPages extend the breadcrumb with the page title.
var detailPages = map[string]bool{
	"client_detail":        true,
	"phantom_token_detail": true,
	"client_updates":       true,
}

func buildCrumbs(page string, d pageData) []Crumb {
	e, ok := navEntries[d.Active]
	if !ok {
		return []Crumb{{Label: d.Title, Current: true}}
	}
	crumbs := []Crumb{{Label: e.Section}}
	last := ""
	switch {
	case d.Crumb != "":
		last = d.Crumb
	case detailPages[page] && d.Title != "" && d.Title != e.Label:
		last = d.Title
	}
	if last == "" {
		return append(crumbs, Crumb{Label: e.Label, Current: true})
	}
	return append(crumbs, Crumb{Label: e.Label, Href: e.Href}, Crumb{Label: last, Current: true})
}

// shortVersion reduces version.Get() to a compact label for the brand line.
func shortVersion(v string) string {
	f := strings.Fields(strings.TrimSpace(v))
	if len(f) == 0 {
		return "dev"
	}
	if f[0] == "commit" && len(f) > 1 {
		return strings.TrimRight(f[1], ",")
	}
	return strings.TrimRight(f[0], ",")
}

// hostOf returns the host (without port) of a gateway URL or host[:port].
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func requestHost(r *http.Request) string {
	h := r.Host
	if sh, _, err := net.SplitHostPort(h); err == nil {
		return sh
	}
	return strings.Trim(h, "[]")
}

func (h *AdminHandler) prepareShell(r *http.Request, page string, data *pageData) {
	s := ShellData{Crumbs: buildCrumbs(page, *data), Version: shortVersion(version.Get())}
	if u, _ := r.Context().Value(middleware.AdminUserKey).(string); u != "" {
		s.AdminUser = u
		s.Initial = strings.ToUpper(string([]rune(u)[:1]))
	}
	if h.db != nil {
		s.GatewayHost = hostOf(queries.NewSettingsQueries(h.db).Get(queries.SettingGatewayURL))
	}
	if s.GatewayHost == "" {
		s.GatewayHost = requestHost(r)
	}
	data.Shell = s
}
