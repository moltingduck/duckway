package handlers

import (
	"database/sql"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
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
	keySuites     *queries.KeySuiteQueries
	crypto        *svc.Crypto
	auth          *middleware.AdminAuth
	db            *sql.DB
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
		"dashboard", "services", "api_keys", "usage", "oauth", "placeholders",
		"clients", "client_detail", "client_updates", "plugins", "key_pools", "key_bundles", "approvals", "logs", "notifications", "canary", "supplychain", "settings", "docs", "cc", "onboarding", "moved",
	}

	pages := make(map[string]*template.Template)
	for _, name := range pageNames {
		pageContent, err := fs.ReadFile(templateFS, "templates/"+name+".html")
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

// WithKeySuites wires in the key-suite query object after construction.
// Called from routes.go after building the handler.
func (h *AdminHandler) WithKeySuites(ks *queries.KeySuiteQueries) *AdminHandler {
	h.keySuites = ks
	return h
}

func (h *AdminHandler) WithDB(db *sql.DB) *AdminHandler {
	h.db = db
	return h
}

func (h *AdminHandler) WithCrypto(crypto *svc.Crypto) *AdminHandler {
	h.crypto = crypto
	return h
}

func (h *AdminHandler) markMintableKeys(keys []models.APIKey) []models.APIKey {
	if h.crypto == nil {
		return keys
	}
	for i := range keys {
		if keys[i].KeyEncrypted == "" {
			continue
		}
		plain, err := h.crypto.Decrypt(keys[i].KeyEncrypted)
		if err != nil {
			continue
		}
		keys[i].IsMintable = isGitHubAppCredentialJSON(plain)
	}
	return keys
}

type pageData struct {
	Title  string
	Active string
	Error  string
	// CSPNonce is the per-request script nonce; every <script> must carry it.
	CSPNonce string
	// PendingApprovals feeds the nav badge.
	PendingApprovals int
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
	KeyGroups    interface{}
	KeyGroup     interface{} // single group detail
	KeySuites    interface{}
	Approvals    interface{}
	Logs         interface{}
	Channels     interface{}
	CanaryTokens interface{}
	// Moved is set on the page-moved 404 notice (see moved.go).
	Moved *MovedNotice
}

func (h *AdminHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	h.loginTmpl.ExecuteTemplate(w, "login.html", pageData{CSPNonce: middleware.CSPNonce(r.Context())})
}

func (h *AdminHandler) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.users.GetByUsername(username)
	if err != nil {
		h.loginTmpl.ExecuteTemplate(w, "login.html", pageData{Error: "Invalid credentials", CSPNonce: middleware.CSPNonce(r.Context())})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		h.loginTmpl.ExecuteTemplate(w, "login.html", pageData{Error: "Invalid credentials", CSPNonce: middleware.CSPNonce(r.Context())})
		return
	}

	cookie := h.auth.CreateSession(user.Username, r)
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (h *AdminHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	// Counters and the attention queue load from GET /api/overview.
	logs, _ := h.requestLog.Recent(10)

	h.render(w, r, "dashboard", pageData{
		Title:      "Overview",
		Active:     "dashboard",
		RecentLogs: logs,
	})
}

func (h *AdminHandler) ServicesPage(w http.ResponseWriter, r *http.Request) {
	svcs, _ := h.services.List()
	h.render(w, r, "services", pageData{
		Title:    "Services",
		Active:   "services",
		Services: svcs,
	})
}

func (h *AdminHandler) KeysPage(w http.ResponseWriter, r *http.Request) {
	keys, _ := h.apiKeys.List("")
	keys = h.markMintableKeys(keys)
	svcs, _ := h.services.List()
	h.render(w, r, "api_keys", pageData{
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
	keys = h.markMintableKeys(keys)
	groups, _ := h.groups.List("")
	h.render(w, r, "placeholders", pageData{
		Title:        "Placeholders",
		Active:       "placeholders",
		Placeholders: phs,
		Services:     svcs,
		Clients:      clients,
		Keys:         keys,
		Groups:       groups,
	})
}

func (h *AdminHandler) ClientsPage(w http.ResponseWriter, r *http.Request) {
	clients, _ := h.clients.List()
	keys, _ := h.apiKeys.List("")
	keys = h.markMintableKeys(keys)
	groups, _ := h.groups.List("")
	placeholders, _ := h.placeholders.List("", "")

	// Collect canary tokens for all clients
	var allCanaries []queries.CanaryToken
	for _, c := range clients {
		ct, _ := h.canary.ListByClient(c.ID)
		allCanaries = append(allCanaries, ct...)
	}

	h.render(w, r, "clients", pageData{
		Title:        "Clients",
		Active:       "clients",
		Clients:      clients,
		Keys:         keys,
		Groups:       groups,
		Placeholders: placeholders,
		CanaryTokens: allCanaries,
	})
}

func (h *AdminHandler) ClientUpdatesPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "client_updates", pageData{Title: "Rollouts", Active: "clients"})
}

// KeyPoolsPage renders the Key pools shell. Both pool kinds (legacy
// per-request rotation groups and quota-aware lease groups) are loaded by the
// page from the admin APIs; the optional ?pool=<kind>:<id> deep link is read
// client-side.
func (h *AdminHandler) KeyPoolsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "key_pools", pageData{
		Title:  "Key pools",
		Active: "key_pools",
	})
}

// KeyBundlesPage renders the Key bundles shell (data loaded by the page).
func (h *AdminHandler) KeyBundlesPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "key_bundles", pageData{
		Title:  "Key bundles",
		Active: "key_bundles",
	})
}

func (h *AdminHandler) ApprovalsPage(w http.ResponseWriter, r *http.Request) {
	approvals, _ := h.approvals.ListPending()
	h.render(w, r, "approvals", pageData{
		Title:     "Approvals",
		Active:    "approvals",
		Approvals: approvals,
	})
}

func (h *AdminHandler) LogsPage(w http.ResponseWriter, r *http.Request) {
	logs, _ := h.requestLog.Recent(100)
	h.render(w, r, "logs", pageData{
		Title:  "Request Log",
		Active: "logs",
		Logs:   logs,
	})
}

func (h *AdminHandler) UsagePage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "usage", pageData{
		Title:  "Usage",
		Active: "usage",
	})
}

func (h *AdminHandler) OAuthPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "oauth", pageData{
		Title:  "OAuth Tokens",
		Active: "oauth",
	})
}

func (h *AdminHandler) CCPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "cc", pageData{
		Title:  "Control Channels",
		Active: "cc",
	})
}

func (h *AdminHandler) OnboardingPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "onboarding", pageData{
		Title:  "Add client",
		Active: "clients",
	})
}

func (h *AdminHandler) SettingsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "settings", pageData{
		Title:  "Settings",
		Active: "settings",
	})
}

func (h *AdminHandler) SupplyChainPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "supplychain", pageData{
		Title:  "Supply Chain",
		Active: "supplychain",
	})
}

func (h *AdminHandler) DocsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "docs", pageData{
		Title:  "Documentation",
		Active: "docs",
	})
}

func (h *AdminHandler) CanaryPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "canary", pageData{
		Title:  "Canary Tokens",
		Active: "canary",
	})
}

func (h *AdminHandler) NotificationsPage(w http.ResponseWriter, r *http.Request) {
	channels, _ := h.notifications.List()
	h.render(w, r, "notifications", pageData{
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

func (h *AdminHandler) render(w http.ResponseWriter, r *http.Request, page string, data pageData) {
	data.CSPNonce = middleware.CSPNonce(r.Context())
	if h.approvals != nil {
		if n, err := h.approvals.CountPending(); err != nil {
			log.Printf("admin: count pending approvals for nav badge: %v", err)
		} else {
			data.PendingApprovals = n
		}
	}

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
