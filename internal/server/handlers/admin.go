package handlers

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"golang.org/x/crypto/bcrypt"
)

type AdminHandler struct {
	pages         map[string]*template.Template
	loginTmpl     *template.Template
	templateFS    fs.FS
	funcMap       template.FuncMap
	liveReload    bool // true = re-parse templates on every request
	users         *queries.AdminUserQueries
	services      *queries.ServiceQueries
	apiKeys       *queries.APIKeyQueries
	placeholders  *queries.PlaceholderQueries
	clients       *queries.ClientQueries
	groups        *queries.GroupQueries
	approvals     *queries.ApprovalQueries
	requestLog    *queries.RequestLogQueries
	notifications *queries.NotificationQueries
	canary        *queries.CanaryQueries
	auth          *middleware.AdminAuth
}

func NewAdminHandler(
	templateFS fs.FS,
	users *queries.AdminUserQueries,
	services *queries.ServiceQueries,
	apiKeys *queries.APIKeyQueries,
	placeholders *queries.PlaceholderQueries,
	clients *queries.ClientQueries,
	groups *queries.GroupQueries,
	approvals *queries.ApprovalQueries,
	requestLog *queries.RequestLogQueries,
	notifications *queries.NotificationQueries,
	canary *queries.CanaryQueries,
	auth *middleware.AdminAuth,
) *AdminHandler {
	funcMap := template.FuncMap{
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
		"deref": func(p interface{}) interface{} {
			switch v := p.(type) {
			case *string:
				if v == nil {
					return ""
				}
				return *v
			case *int:
				if v == nil {
					return 0
				}
				return *v
			default:
				return p
			}
		},
		"upper": strings.ToUpper,
	}

	// Parse layout once as the base
	layoutContent, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		log.Fatalf("Failed to read layout template: %v", err)
	}

	// Parse each page template paired with the layout
	pageNames := []string{
		"dashboard", "services", "api_keys", "placeholders",
		"clients", "groups", "approvals", "logs", "notifications", "canary", "docs",
	}

	pages := make(map[string]*template.Template)
	for _, name := range pageNames {
		pageContent, err := fs.ReadFile(templateFS, "templates/" + name + ".html")
		if err != nil {
			log.Fatalf("Failed to read template %s: %v", name, err)
		}

		tmpl, err := template.New("layout").Funcs(funcMap).Parse(string(layoutContent))
		if err != nil {
			log.Fatalf("Failed to parse layout for %s: %v", name, err)
		}

		_, err = tmpl.New(name).Parse(string(pageContent))
		if err != nil {
			log.Fatalf("Failed to parse page %s: %v", name, err)
		}

		pages[name] = tmpl
	}

	// Login page is standalone (no layout)
	loginTmpl, err := template.New("login").Funcs(funcMap).ParseFS(templateFS, "templates/login.html")
	if err != nil {
		log.Fatalf("Failed to parse login template: %v", err)
	}

	liveReload := os.Getenv("DUCKWAY_WEB_DIR") != ""

	return &AdminHandler{
		pages:         pages,
		loginTmpl:     loginTmpl,
		templateFS:    templateFS,
		funcMap:       funcMap,
		liveReload:    liveReload,
		users:         users,
		services:      services,
		apiKeys:       apiKeys,
		placeholders:  placeholders,
		clients:       clients,
		groups:        groups,
		approvals:     approvals,
		requestLog:    requestLog,
		notifications: notifications,
		canary:        canary,
		auth:          auth,
	}
}

type pageData struct {
	Title  string
	Active string
	Error  string
	// Dashboard
	ServiceCount     int
	KeyCount         int
	ClientCount      int
	PlaceholderCount int
	RecentLogs       interface{}
	// CRUD pages
	Services     interface{}
	Keys         interface{}
	Placeholders interface{}
	Clients      interface{}
	Groups       interface{}
	Approvals    interface{}
	Logs         interface{}
	Channels      interface{}
	CanaryTokens  interface{}
}

func (h *AdminHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	h.loginTmpl.ExecuteTemplate(w, "login.html", pageData{})
}

func (h *AdminHandler) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.users.GetByUsername(username)
	if err != nil {
		h.loginTmpl.ExecuteTemplate(w, "login.html", pageData{Error: "Invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		h.loginTmpl.ExecuteTemplate(w, "login.html", pageData{Error: "Invalid credentials"})
		return
	}

	cookie := h.auth.CreateSession(user.Username)
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (h *AdminHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	svcs, _ := h.services.List()
	keys, _ := h.apiKeys.List("")
	clients, _ := h.clients.List()
	phs, _ := h.placeholders.List("", "")
	logs, _ := h.requestLog.Recent(10)

	h.render(w, "dashboard", pageData{
		Title:            "Dashboard",
		Active:           "dashboard",
		ServiceCount:     len(svcs),
		KeyCount:         len(keys),
		ClientCount:      len(clients),
		PlaceholderCount: len(phs),
		RecentLogs:       logs,
	})
}

func (h *AdminHandler) ServicesPage(w http.ResponseWriter, r *http.Request) {
	svcs, _ := h.services.List()
	h.render(w, "services", pageData{
		Title:    "Services",
		Active:   "services",
		Services: svcs,
	})
}

func (h *AdminHandler) KeysPage(w http.ResponseWriter, r *http.Request) {
	keys, _ := h.apiKeys.List("")
	svcs, _ := h.services.List()
	h.render(w, "api_keys", pageData{
		Title:    "API Keys",
		Active:   "keys",
		Keys:     keys,
		Services: svcs,
	})
}

func (h *AdminHandler) PlaceholdersPage(w http.ResponseWriter, r *http.Request) {
	phs, _ := h.placeholders.List("", "")
	svcs, _ := h.services.List()
	clients, _ := h.clients.List()
	keys, _ := h.apiKeys.List("")
	h.render(w, "placeholders", pageData{
		Title:        "Placeholders",
		Active:       "placeholders",
		Placeholders: phs,
		Services:     svcs,
		Clients:      clients,
		Keys:         keys,
	})
}

func (h *AdminHandler) ClientsPage(w http.ResponseWriter, r *http.Request) {
	clients, _ := h.clients.List()
	keys, _ := h.apiKeys.List("")
	placeholders, _ := h.placeholders.List("", "")

	// Collect canary tokens for all clients
	var allCanaries []queries.CanaryToken
	for _, c := range clients {
		ct, _ := h.canary.ListByClient(c.ID)
		allCanaries = append(allCanaries, ct...)
	}

	h.render(w, "clients", pageData{
		Title:        "Clients",
		Active:       "clients",
		Clients:      clients,
		Keys:         keys,
		Placeholders: placeholders,
		CanaryTokens: allCanaries,
	})
}

func (h *AdminHandler) GroupsPage(w http.ResponseWriter, r *http.Request) {
	groups, _ := h.groups.List("")
	svcs, _ := h.services.List()
	for i := range groups {
		members, _ := h.groups.GetMembers(groups[i].ID)
		groups[i].Members = members
	}
	h.render(w, "groups", pageData{
		Title:    "Key Groups",
		Active:   "groups",
		Groups:   groups,
		Services: svcs,
	})
}

func (h *AdminHandler) ApprovalsPage(w http.ResponseWriter, r *http.Request) {
	approvals, _ := h.approvals.ListPending()
	h.render(w, "approvals", pageData{
		Title:     "Approvals",
		Active:    "approvals",
		Approvals: approvals,
	})
}

func (h *AdminHandler) LogsPage(w http.ResponseWriter, r *http.Request) {
	logs, _ := h.requestLog.Recent(100)
	h.render(w, "logs", pageData{
		Title:  "Request Log",
		Active: "logs",
		Logs:   logs,
	})
}

func (h *AdminHandler) DocsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "docs", pageData{
		Title:  "Documentation",
		Active: "docs",
	})
}

func (h *AdminHandler) CanaryPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "canary", pageData{
		Title:  "Canary Tokens",
		Active: "canary",
	})
}

func (h *AdminHandler) NotificationsPage(w http.ResponseWriter, r *http.Request) {
	channels, _ := h.notifications.List()
	h.render(w, "notifications", pageData{
		Title:    "Notifications",
		Active:   "notifications",
		Channels: channels,
	})
}

func (h *AdminHandler) ApproveAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.approvals.Approve(id, "datetime('now', '+24 hours')")
	http.Redirect(w, r, "/admin/approvals", http.StatusSeeOther)
}

func (h *AdminHandler) RejectAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.approvals.Reject(id)
	http.Redirect(w, r, "/admin/approvals", http.StatusSeeOther)
}

func (h *AdminHandler) render(w http.ResponseWriter, page string, data pageData) {
	var tmpl *template.Template

	if h.liveReload {
		// Re-parse from disk on every request (dev mode)
		layoutContent, err := fs.ReadFile(h.templateFS, "templates/layout.html")
		if err != nil {
			http.Error(w, "layout template not found", http.StatusInternalServerError)
			return
		}
		pageContent, err := fs.ReadFile(h.templateFS, "templates/"+page+".html")
		if err != nil {
			http.Error(w, "page template not found: "+page, http.StatusNotFound)
			return
		}
		tmpl, err = template.New("layout").Funcs(h.funcMap).Parse(string(layoutContent))
		if err != nil {
			log.Printf("Template parse error (layout): %v", err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
		if _, err = tmpl.New(page).Parse(string(pageContent)); err != nil {
			log.Printf("Template parse error (%s): %v", page, err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
	} else {
		// Use pre-parsed templates (production)
		var ok bool
		tmpl, ok = h.pages[page]
		if !ok {
			http.Error(w, "Page not found", http.StatusNotFound)
			return
		}
	}

	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("Template error (%s): %v", page, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}
