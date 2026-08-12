package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
	"github.com/hackerduck/duckway/internal/version"
	"github.com/hackerduck/duckway/skill"
)

// SharedServices holds all query objects and services shared between admin and gateway.
type SharedServices struct {
	UserQ        *queries.AdminUserQueries
	ServiceQ     *queries.ServiceQueries
	APIKeyQ      *queries.APIKeyQueries
	PlaceholderQ *queries.PlaceholderQueries
	ClientQ      *queries.ClientQueries
	GroupQ       *queries.GroupQueries
	ApprovalQ    *queries.ApprovalQueries
	RequestLogQ  *queries.RequestLogQueries
	NotifQ       *queries.NotificationQueries
	CanaryQ      *queries.CanaryQueries
	SettingsQ    *queries.SettingsQueries
	ConvUsageQ   *queries.ConversationUsageQueries
	KeySuiteQ    *queries.KeySuiteQueries

	Crypto      *services.Crypto
	Resolver    *services.KeyResolver
	Notifier    *services.Notifier
	CanarySvc   *services.CanaryService
	Refresher   *services.TokenRefresher
	CCHub       *services.CCEventHub         // CC live-tail pub/sub
	CCApprovals *services.CCApprovalRegistry // discord_request_approval state

	AdminAuth  *middleware.AdminAuth
	ClientAuth *middleware.ClientAuth
}

func NewSharedServices(config *Config, db interface {
	QueryRow(string, ...interface{}) interface{}
}) *SharedServices {
	// This is called from the actual server constructors using the real db
	return nil // placeholder, actual init in initShared
}

func (s *Server) initShared() *SharedServices {
	userQ := queries.NewAdminUserQueries(s.db)
	serviceQ := queries.NewServiceQueries(s.db)
	apiKeyQ := queries.NewAPIKeyQueries(s.db)
	placeholderQ := queries.NewPlaceholderQueries(s.db)
	clientQ := queries.NewClientQueries(s.db)
	groupQ := queries.NewGroupQueries(s.db)
	approvalQ := queries.NewApprovalQueries(s.db)
	requestLogQ := queries.NewRequestLogQueries(s.db)
	notifQ := queries.NewNotificationQueries(s.db)
	canaryQ := queries.NewCanaryQueries(s.db)
	settingsQ := queries.NewSettingsQueries(s.db)
	convUsageQ := queries.NewConversationUsageQueries(s.db)
	keySuiteQ := queries.NewKeySuiteQueries(s.db)

	crypto := services.NewCrypto(s.config.EncryptionKey)
	resolver := services.NewKeyResolver(crypto, apiKeyQ, placeholderQ, groupQ, approvalQ)
	notifier := services.NewNotifier(notifQ)
	s.notifier = notifier
	canarySvc := services.NewCanaryService(canaryQ)

	adminAuth := middleware.NewAdminAuth(s.config.SessionSecret)
	clientAuth := middleware.NewClientAuth(clientQ)

	return &SharedServices{
		UserQ: userQ, ServiceQ: serviceQ, APIKeyQ: apiKeyQ,
		PlaceholderQ: placeholderQ, ClientQ: clientQ, GroupQ: groupQ,
		ApprovalQ: approvalQ, RequestLogQ: requestLogQ,
		NotifQ: notifQ, CanaryQ: canaryQ, SettingsQ: settingsQ, ConvUsageQ: convUsageQ,
		KeySuiteQ: keySuiteQ,
		Crypto:    crypto, Resolver: resolver, Notifier: notifier, CanarySvc: canarySvc,
		CCHub:       services.NewCCEventHub(),
		CCApprovals: services.NewCCApprovalRegistry(),
		AdminAuth:   adminAuth, ClientAuth: clientAuth,
	}
}

// SetupAdminRoutes adds admin panel + management API routes.
func (s *Server) SetupAdminRoutes(contentFS fs.FS, ss *SharedServices) {
	settingsQ := queries.NewSettingsQueries(s.db)
	authH := handlers.NewAuthHandler(ss.UserQ, ss.AdminAuth)
	serviceH := handlers.NewServiceHandler(ss.ServiceQ)
	apiKeyH := handlers.NewAPIKeyHandler(ss.APIKeyQ, ss.ServiceQ, ss.Crypto)
	placeholderH := handlers.NewPlaceholderHandler(ss.PlaceholderQ, ss.ServiceQ, ss.ClientQ).
		WithKeyLookup(ss.APIKeyQ, ss.Crypto)
	clientH := handlers.NewClientHandler(ss.ClientQ, ss.PlaceholderQ, ss.ServiceQ, ss.APIKeyQ, ss.CanarySvc)
	clientUpdateH := handlers.NewClientUpdateHandler(queries.NewClientUpdateQueries(s.db), ss.ClientQ, configuredDownloadDir())
	groupH := handlers.NewGroupHandler(ss.GroupQ, ss.ServiceQ)
	approvalH := handlers.NewApprovalHandler(ss.ApprovalQ, ss.PlaceholderQ)
	notifH := handlers.NewNotificationHandler(ss.NotifQ, ss.Notifier)
	canaryH := handlers.NewCanaryHandler(ss.CanaryQ, ss.CanarySvc)
	ccQ := queries.NewControlChannelQueries(s.db)
	ccH := handlers.NewControlChannelHandler(ccQ, ss.APIKeyQ, ss.PlaceholderQ, ss.ServiceQ, ss.ClientQ, ss.SettingsQ, ss.Crypto, services.NewDiscordBot())
	ccH.SetHub(ss.CCHub)
	ccH.SetApprovals(ss.CCApprovals)
	adminPageH := handlers.NewAdminHandler(contentFS, ss.UserQ, ss.ServiceQ, ss.APIKeyQ, ss.PlaceholderQ, ss.ClientQ, ss.GroupQ, ss.ApprovalQ, ss.RequestLogQ, ss.NotifQ, ss.CanaryQ, ss.AdminAuth).WithDB(s.db).WithKeySuites(ss.KeySuiteQ).WithCrypto(ss.Crypto)

	// Static files
	staticFS, err := fs.Sub(contentFS, "static")
	if err != nil {
		log.Fatalf("Failed to get static FS: %v", err)
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Admin pages
	s.mux.HandleFunc("GET /admin/login", adminPageH.LoginPage)
	s.mux.HandleFunc("POST /admin/login", adminPageH.LoginSubmit)

	adminPageMux := http.NewServeMux()
	adminPageMux.HandleFunc("GET /admin/", adminPageH.Dashboard)
	adminPageMux.HandleFunc("GET /admin/services", adminPageH.ServicesPage)
	adminPageMux.HandleFunc("GET /admin/keys", adminPageH.KeysPage)
	adminPageMux.HandleFunc("GET /admin/usage", adminPageH.UsagePage)
	adminPageMux.HandleFunc("GET /admin/placeholders", adminPageH.PlaceholdersPage)
	adminPageMux.HandleFunc("GET /admin/clients", adminPageH.ClientsPage)
	adminPageMux.HandleFunc("GET /admin/client-updates", adminPageH.ClientUpdatesPage)
	adminPageMux.HandleFunc("GET /admin/groups", adminPageH.GroupsPage)
	adminPageMux.HandleFunc("GET /admin/key-groups", adminPageH.KeyGroupsPage)
	adminPageMux.HandleFunc("GET /admin/key-groups/{id}", adminPageH.KeyGroupDetailPage)
	adminPageMux.HandleFunc("GET /admin/key-suites", adminPageH.KeySuitesPage)
	adminPageMux.HandleFunc("GET /admin/approvals", adminPageH.ApprovalsPage)
	adminPageMux.HandleFunc("GET /admin/logs", adminPageH.LogsPage)
	adminPageMux.HandleFunc("GET /admin/notifications", adminPageH.NotificationsPage)
	adminPageMux.HandleFunc("GET /admin/canary", adminPageH.CanaryPage)
	adminPageMux.HandleFunc("GET /admin/oauth", adminPageH.OAuthPage)
	adminPageMux.HandleFunc("GET /admin/cc", adminPageH.CCPage)
	adminPageMux.HandleFunc("GET /admin/onboarding", adminPageH.OnboardingPage)
	adminPageMux.HandleFunc("GET /admin/settings", adminPageH.SettingsPage)
	adminPageMux.HandleFunc("GET /admin/supply-chain", adminPageH.SupplyChainPage)
	adminPageMux.HandleFunc("GET /admin/docs", adminPageH.DocsPage)
	adminPageMux.HandleFunc("POST /admin/approvals/{id}/approve", adminPageH.ApproveAction)
	adminPageMux.HandleFunc("POST /admin/approvals/{id}/reject", adminPageH.RejectAction)
	s.mux.Handle("/admin/", ss.AdminAuth.Middleware(adminPageMux))

	// Public API routes
	s.mux.HandleFunc("POST /api/auth/login", authH.Login)
	s.mux.HandleFunc("POST /api/auth/logout", authH.Logout)

	// Admin API
	adminAPIMux := http.NewServeMux()
	adminAPIMux.HandleFunc("POST /api/auth/change-password", authH.ChangePassword)
	adminAPIMux.HandleFunc("GET /api/services", serviceH.List)
	adminAPIMux.HandleFunc("POST /api/services", serviceH.Create)
	adminAPIMux.HandleFunc("GET /api/services/{id}", serviceH.Get)
	adminAPIMux.HandleFunc("PUT /api/services/{id}", serviceH.Update)
	adminAPIMux.HandleFunc("DELETE /api/services/{id}", serviceH.Delete)
	adminAPIMux.HandleFunc("GET /api/services/{id}/acl-templates", serviceH.ListACLTemplates)
	adminAPIMux.HandleFunc("POST /api/services/{id}/acl-templates", serviceH.ApplyACLTemplate)

	usageH := handlers.NewUsageHandler(ss.APIKeyQ, ss.RequestLogQ, ss.ConvUsageQ)
	adminAPIMux.HandleFunc("GET /api/usage", usageH.List)
	adminAPIMux.HandleFunc("GET /api/usage/clients", usageH.Clients)
	adminAPIMux.HandleFunc("GET /api/usage/sessions", usageH.Sessions)
	adminAPIMux.HandleFunc("GET /api/usage/conversations", usageH.Conversations)

	adminAPIMux.HandleFunc("GET /api/keys", apiKeyH.List)
	adminAPIMux.HandleFunc("POST /api/keys", apiKeyH.Create)
	adminAPIMux.HandleFunc("GET /api/keys/{id}", apiKeyH.Get)
	adminAPIMux.HandleFunc("PUT /api/keys/{id}", apiKeyH.Update)
	adminAPIMux.HandleFunc("DELETE /api/keys/{id}", apiKeyH.Delete)
	adminAPIMux.HandleFunc("POST /api/keys/github-app/test", apiKeyH.TestGitHubAppMinter)
	adminAPIMux.HandleFunc("GET /api/keys/{id}/github-app/repositories", apiKeyH.ListGitHubAppRepositories)
	adminAPIMux.HandleFunc("GET /api/keys/{id}/acl-templates", apiKeyH.ListACLTemplates)
	adminAPIMux.HandleFunc("POST /api/keys/{id}/acl-templates", apiKeyH.ApplyACLTemplate)
	adminAPIMux.HandleFunc("POST /api/keys/{id}/acl", apiKeyH.SetACL)

	adminAPIMux.HandleFunc("GET /api/placeholders", placeholderH.List)
	adminAPIMux.HandleFunc("GET /api/placeholders/with-approvals", func(w http.ResponseWriter, r *http.Request) {
		placeholderH.ListWithApprovals(w, r, ss.ApprovalQ)
	})
	adminAPIMux.HandleFunc("GET /api/placeholders/{id}", placeholderH.Get)
	adminAPIMux.HandleFunc("POST /api/placeholders", placeholderH.Create)
	adminAPIMux.HandleFunc("PUT /api/placeholders/{id}", placeholderH.Update)
	adminAPIMux.HandleFunc("DELETE /api/placeholders/{id}", placeholderH.Delete)

	adminAPIMux.HandleFunc("GET /api/clients", clientH.List)
	adminAPIMux.HandleFunc("POST /api/clients", clientH.Create)
	adminAPIMux.HandleFunc("GET /api/clients/{id}", clientH.Get)
	adminAPIMux.HandleFunc("PUT /api/clients/{id}", clientH.Update)
	adminAPIMux.HandleFunc("DELETE /api/clients/{id}", clientH.Delete)
	adminAPIMux.HandleFunc("POST /api/clients/{id}/canary", clientH.ToggleCanary)
	adminAPIMux.HandleFunc("GET /api/client-updates", clientUpdateH.List)
	adminAPIMux.HandleFunc("POST /api/client-updates", clientUpdateH.Create)
	adminAPIMux.HandleFunc("GET /api/client-updates/{id}", clientUpdateH.Get)
	adminAPIMux.HandleFunc("POST /api/client-updates/{id}/{action}", clientUpdateH.Action)

	adminAPIMux.HandleFunc("GET /api/groups", groupH.List)
	adminAPIMux.HandleFunc("POST /api/groups", groupH.Create)
	adminAPIMux.HandleFunc("GET /api/groups/{id}", groupH.Get)
	adminAPIMux.HandleFunc("PUT /api/groups/{id}", groupH.Update)
	adminAPIMux.HandleFunc("DELETE /api/groups/{id}", groupH.Delete)
	adminAPIMux.HandleFunc("POST /api/groups/{id}/members", groupH.AddMember)
	adminAPIMux.HandleFunc("DELETE /api/groups/{id}/members/{keyId}", groupH.RemoveMember)

	// Key Suites: named bundles of different-service keys for bulk client assignment
	keySuiteH := handlers.NewKeySuiteHandler(ss.KeySuiteQ, ss.ServiceQ, ss.PlaceholderQ, ss.APIKeyQ, ss.Crypto)
	adminAPIMux.HandleFunc("GET /api/key-suites", keySuiteH.List)
	adminAPIMux.HandleFunc("POST /api/key-suites", keySuiteH.Create)
	adminAPIMux.HandleFunc("GET /api/key-suites/clients/{clientId}", keySuiteH.ListClientAssignments)
	adminAPIMux.HandleFunc("GET /api/key-suites/{id}", keySuiteH.Get)
	adminAPIMux.HandleFunc("PATCH /api/key-suites/{id}", keySuiteH.Update)
	adminAPIMux.HandleFunc("DELETE /api/key-suites/{id}", keySuiteH.Delete)
	adminAPIMux.HandleFunc("POST /api/key-suites/{id}/entries", keySuiteH.AddEntry)
	adminAPIMux.HandleFunc("PATCH /api/key-suites/{id}/entries/{entryId}", keySuiteH.UpdateEntry)
	adminAPIMux.HandleFunc("DELETE /api/key-suites/{id}/entries/{entryId}", keySuiteH.RemoveEntry)
	adminAPIMux.HandleFunc("POST /api/key-suites/{id}/assign", keySuiteH.AssignToClient)
	adminAPIMux.HandleFunc("DELETE /api/key-suites/{id}/assignments/{clientId}", keySuiteH.UnassignClient)

	// Key Groups v2: pluggable rotation strategies with 429 auto-rotation
	keyGroupH := handlers.NewKeyGroupHandler(s.db)
	adminAPIMux.HandleFunc("GET /api/key-groups", keyGroupH.List)
	adminAPIMux.HandleFunc("POST /api/key-groups", keyGroupH.Create)
	adminAPIMux.HandleFunc("GET /api/key-groups/{id}", keyGroupH.Get)
	adminAPIMux.HandleFunc("PATCH /api/key-groups/{id}", keyGroupH.Update)
	adminAPIMux.HandleFunc("DELETE /api/key-groups/{id}", keyGroupH.Delete)
	adminAPIMux.HandleFunc("POST /api/key-groups/{id}/members", keyGroupH.AddMember)
	adminAPIMux.HandleFunc("PATCH /api/key-groups/{id}/members/{key_id}", keyGroupH.UpdateMember)
	adminAPIMux.HandleFunc("DELETE /api/key-groups/{id}/members/{key_id}", keyGroupH.RemoveMember)

	adminAPIMux.HandleFunc("GET /api/approvals", approvalH.ListPending)
	adminAPIMux.HandleFunc("GET /api/approvals/list", approvalH.List) // enriched + filterable
	adminAPIMux.HandleFunc("POST /api/approvals/{id}/approve", approvalH.Approve)
	adminAPIMux.HandleFunc("POST /api/approvals/{id}/reject", approvalH.Reject)

	adminAPIMux.HandleFunc("GET /api/notifications", notifH.List)
	adminAPIMux.HandleFunc("POST /api/notifications", notifH.Create)
	adminAPIMux.HandleFunc("GET /api/notifications/{id}", notifH.Get)
	adminAPIMux.HandleFunc("PUT /api/notifications/{id}", notifH.Update)
	adminAPIMux.HandleFunc("DELETE /api/notifications/{id}", notifH.Delete)
	adminAPIMux.HandleFunc("POST /api/notifications/{id}/test", notifH.Test)

	adminAPIMux.HandleFunc("GET /api/canary/settings", canaryH.GetSettings)
	adminAPIMux.HandleFunc("POST /api/canary/settings", canaryH.SaveSettings)
	adminAPIMux.HandleFunc("GET /api/canary/clients/{clientId}", canaryH.ListByClient)
	adminAPIMux.HandleFunc("POST /api/canary/clients/{clientId}/generate", canaryH.GenerateForClient)
	adminAPIMux.HandleFunc("DELETE /api/canary/clients/{clientId}", canaryH.DeleteClientTokens)
	adminAPIMux.HandleFunc("DELETE /api/canary/tokens/{tokenId}", canaryH.DeleteToken)

	// Supply-chain cooldown: view supported package-manager mitigations and
	// toggle each on/off (default all-on). The client fetches the resolved env
	// vars from /client/supply-chain-env.
	supplyChainH := handlers.NewSupplyChainHandler(settingsQ)
	adminAPIMux.HandleFunc("GET /api/supply-chain", supplyChainH.List)
	adminAPIMux.HandleFunc("POST /api/supply-chain/min-age", supplyChainH.SetMinAge)
	adminAPIMux.HandleFunc("POST /api/supply-chain/{id}", supplyChainH.Toggle)

	adminAPIMux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		handlers.JsonResponsePublic(w, settingsQ.GetAll())
	})
	adminAPIMux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			handlers.JsonErrorPublic(w, "invalid request", 400)
			return
		}
		for k, v := range req {
			settingsQ.Set(k, v)
		}
		handlers.JsonResponsePublic(w, map[string]string{"status": "ok"})
	})

	oauthH := handlers.NewOAuthHandler(ss.APIKeyQ, ss.PlaceholderQ, ss.ServiceQ, ss.Crypto)
	oauthH.SetRefresher(ss.Refresher)
	adminAPIMux.HandleFunc("POST /api/oauth/validate", oauthH.Validate)
	adminAPIMux.HandleFunc("POST /api/oauth/upload", oauthH.Upload)
	adminAPIMux.HandleFunc("GET /api/oauth/{id}", oauthH.Get)
	adminAPIMux.HandleFunc("PUT /api/oauth/{id}", oauthH.Update)
	adminAPIMux.HandleFunc("DELETE /api/oauth/{id}", oauthH.Delete)
	adminAPIMux.HandleFunc("POST /api/oauth/{id}/refresh", oauthH.Refresh)

	// Control Channels (CC) — admin CRUD
	// CC v2: 1:1 client↔CC, all admin endpoints under /api/cc.
	adminAPIMux.HandleFunc("GET /api/cc", ccH.List)
	adminAPIMux.HandleFunc("POST /api/cc", ccH.Create)
	adminAPIMux.HandleFunc("GET /api/cc/discord/setup", ccH.DiscordSetup)
	adminAPIMux.HandleFunc("GET /api/cc/discord/categories", ccH.DiscordCategories)
	adminAPIMux.HandleFunc("POST /api/cc/discord/categories", ccH.DiscordCreateCategory)
	adminAPIMux.HandleFunc("POST /api/cc/discord/category-permissions", ccH.DiscordGrantCategoryPermissions)
	adminAPIMux.HandleFunc("POST /api/cc/discord/preflight", ccH.DiscordPreflight)
	adminAPIMux.HandleFunc("GET /api/cc/{id}", ccH.Get)
	adminAPIMux.HandleFunc("PUT /api/cc/{id}", ccH.Update)
	adminAPIMux.HandleFunc("DELETE /api/cc/{id}", ccH.Delete)
	adminAPIMux.HandleFunc("POST /api/cc/{id}/test", ccH.Test)
	adminAPIMux.HandleFunc("POST /api/cc/{id}/test-agent", ccH.TestAgent)
	adminAPIMux.HandleFunc("GET /api/cc/{id}/test-agent/{test_id}", ccH.GetAgentTest)
	if os.Getenv("DUCKWAY_CC_DEBUG_INJECT") == "1" {
		// Debug-only: synthetic SSE events for e2e tests.
		adminAPIMux.HandleFunc("POST /api/cc/{id}/inject_event", ccH.InjectEvent)
		log.Printf("[cc] debug inject_event endpoint enabled (DUCKWAY_CC_DEBUG_INJECT=1)")
	}

	adminAPIMux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		logs, err := ss.RequestLogQ.Recent(500)
		if err != nil {
			handlers.JsonErrorPublic(w, "failed", 500)
			return
		}
		if logs == nil {
			logs = []queries.RequestLogEntry{}
		}
		handlers.JsonResponsePublic(w, logs)
	})

	// GET /api/logs/{id}/detail — full request/response payload (only present
	// when detail capture was enabled at request time).
	adminAPIMux.HandleFunc("GET /api/logs/{id}/detail", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscanf(r.PathValue("id"), "%d", &id)
		if id <= 0 {
			handlers.JsonErrorPublic(w, "invalid id", 400)
			return
		}
		d, err := ss.RequestLogQ.GetDetail(id)
		if err != nil {
			handlers.JsonErrorPublic(w, "no detail recorded for this request", 404)
			return
		}
		handlers.JsonResponsePublic(w, d)
	})

	// DELETE /api/logs/details — drop all stored detail rows. Called by the
	// admin UI when the user toggles the capture feature OFF.
	adminAPIMux.HandleFunc("DELETE /api/logs/details", func(w http.ResponseWriter, r *http.Request) {
		if err := ss.RequestLogQ.DropAllDetails(); err != nil {
			handlers.JsonErrorPublic(w, "drop failed", 500)
			return
		}
		handlers.JsonResponsePublic(w, map[string]string{"status": "dropped"})
	})

	// POST /api/logs/capture — atomic toggle. The admin UI calls this single
	// endpoint instead of POSTing settings + DELETEing details separately,
	// which left a race window where requests arriving between the two calls
	// could persist detail rows after the user thought the feature was off.
	//
	// Body: {"enabled": bool, "clients": ["id", ...]}
	//   enabled=false → in one transaction: set toggle="0", set client filter,
	//                   drop all stored detail rows
	//   enabled=true  → in one transaction: set toggle="1", set client filter
	adminAPIMux.HandleFunc("POST /api/logs/capture", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool     `json:"enabled"`
			Clients []string `json:"clients"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			handlers.JsonErrorPublic(w, "invalid body", 400)
			return
		}
		clientsJSON := "[]"
		if len(req.Clients) > 0 {
			b, _ := json.Marshal(req.Clients)
			clientsJSON = string(b)
		}
		var err error
		if req.Enabled {
			err = ss.RequestLogQ.SetCaptureEnabled(clientsJSON)
		} else {
			err = ss.RequestLogQ.SetCaptureDisabledAndDrop(clientsJSON)
		}
		if err != nil {
			handlers.JsonErrorPublic(w, "update failed: "+err.Error(), 500)
			return
		}
		handlers.JsonResponsePublic(w, map[string]interface{}{
			"status":  "ok",
			"enabled": req.Enabled,
		})
	})

	s.mux.Handle("/api/", ss.AdminAuth.Middleware(adminAPIMux))

	// Root redirect
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	})
}

// SetupGatewayRoutes adds proxy, client API, and public endpoints.
func (s *Server) SetupGatewayRoutes(ss *SharedServices) {
	settingsQ := queries.NewSettingsQueries(s.db)
	clientH := handlers.NewClientHandler(ss.ClientQ, ss.PlaceholderQ, ss.ServiceQ, ss.APIKeyQ, ss.CanarySvc)
	clientUpdateH := handlers.NewClientUpdateHandler(queries.NewClientUpdateQueries(s.db), ss.ClientQ, configuredDownloadDir())
	canaryH := handlers.NewCanaryHandler(ss.CanaryQ, ss.CanarySvc)
	proxyH := handlers.NewProxyHandler(ss.ServiceQ, ss.APIKeyQ, ss.Resolver, ss.RequestLogQ, ss.ApprovalQ, ss.SettingsQ, ss.Notifier).
		WithCrypto(ss.Crypto).
		WithConversationUsage(ss.ConvUsageQ)
	internalH := handlers.NewInternalHandler(ss.Resolver, s.config.DataDir)

	// Client routes (require client auth)
	clientMux := http.NewServeMux()
	clientMux.HandleFunc("GET /client/keys", clientH.GetKeys)
	clientMux.HandleFunc("GET /client/canaries", canaryH.ClientGetCanaries)
	clientMux.HandleFunc("POST /client/control/heartbeat", clientUpdateH.Heartbeat)
	clientMux.HandleFunc("POST /client/control/jobs/{id}/status", clientUpdateH.ReportStatus)

	// Agent statusline script — global content set in /admin/settings,
	// downloaded by `duckway sync` and dropped at ~/.duckway/statusline.sh.
	// Empty body when nothing is configured; the sync command then skips
	// the local write rather than blanking out a previously-installed
	// script.
	clientMux.HandleFunc("GET /client/statusline", func(w http.ResponseWriter, r *http.Request) {
		script := settingsQ.Get(queries.SettingAgentStatuslineScript)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(script))
	})

	// Supply-chain hardening: rc-file settings for the agent's package
	// managers, resolved server-side so rolling-date cutoffs stay fresh.
	supplyChainH := handlers.NewSupplyChainHandler(settingsQ)
	clientMux.HandleFunc("GET /client/supply-chain-rc", supplyChainH.ClientRC)

	// CA cert + key
	ca, caErr := services.LoadOrCreateCA(s.config.DataDir)
	if caErr != nil {
		log.Printf("Warning: CA cert generation failed: %v", caErr)
	}
	if ca != nil {
		clientMux.HandleFunc("GET /client/ca-key", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.Write(ca.KeyPEM)
		})
	}

	// Claude/OAuth credentials endpoint (client auth required)
	oauthClientH := handlers.NewOAuthHandler(ss.APIKeyQ, ss.PlaceholderQ, ss.ServiceQ, ss.Crypto)
	clientMux.HandleFunc("GET /client/claude-credentials", oauthClientH.ClientGetCredentials)
	clientMux.HandleFunc("GET /client/codex-credentials", oauthClientH.ClientGetCodexCredentials)

	// Token-loan endpoint for loan_proxy delivery mode (client auth required).
	// Sidecar fetches the real token here once per TTL, caches it, and forwards
	// requests directly to upstream. Audit endpoint receives the per-request
	// log entries the sidecar collects.
	loanH := handlers.NewLoanHandler(ss.Resolver, ss.ServiceQ, ss.ApprovalQ, ss.RequestLogQ, s.notifier).
		WithCrypto(ss.Crypto).
		WithDB(s.db)
	clientMux.HandleFunc("GET /client/loan", loanH.Issue)
	clientMux.HandleFunc("POST /client/audit", loanH.Audit)
	clientMux.HandleFunc("POST /client/loan/exhaust", loanH.MarkExhausted)

	// Usage snapshot reporting for group-based key management.
	keyGroupH := handlers.NewKeyGroupHandler(s.db)
	clientMux.HandleFunc("POST /client/usage", keyGroupH.ReportUsage)

	// Control Channel client API (client auth required). Real Discord IDs
	// stay server-side — agents see only opaque handles.
	ccQ := queries.NewControlChannelQueries(s.db)
	ccClientH := handlers.NewCCClientHandler(ccQ, ss.APIKeyQ, ss.Crypto, services.NewDiscordBot(), ss.CCHub, ss.CCApprovals)
	// CC v2 client API — cc_id is implicit (1:1 client↔CC).
	clientMux.HandleFunc("GET /client/cc", ccClientH.GetMyCC)
	clientMux.HandleFunc("GET /client/cc/events", ccClientH.Events)
	clientMux.HandleFunc("GET /client/cc/channels", ccClientH.ListChannels)
	clientMux.HandleFunc("POST /client/cc/channels", ccClientH.CreateChannel)
	clientMux.HandleFunc("POST /client/cc/channels/{handle}/archive", ccClientH.ArchiveChannel)
	clientMux.HandleFunc("GET /client/cc/channels/{handle}/messages", ccClientH.GetMessages)
	clientMux.HandleFunc("POST /client/cc/channels/{handle}/messages", ccClientH.PostMessage)
	clientMux.HandleFunc("POST /client/cc/channels/{handle}/attachments", ccClientH.PostAttachment)
	clientMux.HandleFunc("POST /client/cc/channels/{handle}/messages/{message_id}/reactions", ccClientH.ReactMessage)
	clientMux.HandleFunc("PATCH /client/cc/channels/{handle}/messages/{message_id}", ccClientH.EditMessage)
	clientMux.HandleFunc("DELETE /client/cc/channels/{handle}/messages/{message_id}", ccClientH.DeleteMessage)
	clientMux.HandleFunc("POST /client/cc/channels/{handle}/approval", ccClientH.RequestApproval)
	clientMux.HandleFunc("GET /client/cc/inbox", ccClientH.PullInbox)
	clientMux.HandleFunc("POST /client/cc/agent-tests/{test_id}", ccClientH.ReportAgentTest)

	// Client config (no auth — needed during duckway init before token is verified)
	s.mux.HandleFunc("GET /client/config", func(w http.ResponseWriter, r *http.Request) {
		gwURL := settingsQ.Get(queries.SettingGatewayURL)
		proxyPort := settingsQ.Get(queries.SettingProxyPort)
		if proxyPort == "" {
			proxyPort = "18080"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"gateway_url": gwURL,
			"proxy_port":  proxyPort,
		})
	})

	// Service host map (for HTTPS proxy client). Requires client auth.
	// Registered in clientMux so it is protected by the /client/ middleware.
	clientMux.HandleFunc("GET /client/services", func(w http.ResponseWriter, r *http.Request) {
		svcs, err := ss.ServiceQ.List()
		if err != nil {
			http.Error(w, `{"error":"list services failed"}`, http.StatusInternalServerError)
			return
		}
		type svcInfo struct {
			Name         string `json:"name"`
			HostPattern  string `json:"host_pattern"`
			UpstreamURL  string `json:"upstream_url"`
			DeliveryMode string `json:"delivery_mode"`
		}
		var result []svcInfo
		for _, svc := range svcs {
			if svc.IsActive && !strings.HasPrefix(svc.UpstreamURL, "internal://") {
				result = append(result, svcInfo{
					Name: svc.Name, HostPattern: svc.HostPattern,
					UpstreamURL: svc.UpstreamURL, DeliveryMode: svc.DeliveryMode,
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	s.mux.Handle("/client/", ss.ClientAuth.Middleware(clientMux))

	// Proxy routes (require client auth)
	proxyMux := http.NewServeMux()
	proxyMux.HandleFunc("/", proxyH.Handle)
	s.mux.Handle("/proxy/", ss.ClientAuth.Middleware(proxyMux))

	// Internal API (mitmproxy)
	s.mux.HandleFunc("POST /internal/resolve", internalH.Resolve)

	// Public endpoints (no auth)
	s.mux.HandleFunc("GET /skill/duckway-agent.md", func(w http.ResponseWriter, r *http.Request) {
		data, err := skill.Content.ReadFile("duckway-agent.md")
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(data)
	})

	if ca != nil {
		s.mux.HandleFunc("GET /skill/ca.pem", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.Header().Set("Content-Disposition", "attachment; filename=duckway-ca.pem")
			w.Write(ca.CertPEM)
		})
		log.Printf("CA certificate available at /skill/ca.pem")
	}

	// Client binary downloads
	downloadDir := configuredDownloadDir()
	if info, err := os.Stat(downloadDir); err == nil && info.IsDir() {
		s.mux.HandleFunc("GET /download/{binary}", func(w http.ResponseWriter, r *http.Request) {
			serveClientDownload(w, r, downloadDir)
		})
	}

	// Public version endpoint — used by `duckway update` to detect drift
	// before downloading a new binary. No auth: it leaks only the build
	// commit, which is also visible in /install.sh and the binaries.
	s.mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": version.Get()})
	})
	s.mux.HandleFunc("GET /client/update-info", func(w http.ResponseWriter, r *http.Request) {
		handleClientUpdateInfo(w, r, downloadDir)
	})

	// Install script
	s.mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		serverURL := r.Host
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
			scheme = fwd
		}
		baseURL := scheme + "://" + serverURL
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, installScript, baseURL, baseURL, baseURL)
	})
}

func configuredDownloadDir() string {
	if dir := os.Getenv("DUCKWAY_DOWNLOAD_DIR"); dir != "" {
		return dir
	}
	return "/srv/downloads"
}

func handleClientUpdateInfo(w http.ResponseWriter, r *http.Request, downloadDir string) {
	osName := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	current := r.URL.Query().Get("version")
	if osName == "" {
		osName = "linux"
	}
	if arch == "" {
		arch = "amd64"
	}
	switch osName {
	case "linux", "darwin":
	default:
		handlers.JsonErrorPublic(w, "unsupported os", http.StatusBadRequest)
		return
	}
	switch arch {
	case "amd64", "arm64":
	default:
		handlers.JsonErrorPublic(w, "unsupported arch", http.StatusBadRequest)
		return
	}

	binary := fmt.Sprintf("duckway-client-%s-%s", osName, arch)
	path := filepath.Join(downloadDir, binary)
	sum, err := fileSHA256(path)
	if err != nil {
		handlers.JsonErrorPublic(w, "client binary not available", http.StatusNotFound)
		return
	}
	ducklionBinary := fmt.Sprintf("ducklion-%s-%s", osName, arch)
	ducklionPath := filepath.Join(downloadDir, ducklionBinary)
	ducklionSum, ducklionErr := fileSHA256(ducklionPath)

	recommended := os.Getenv("DUCKWAY_CLIENT_RECOMMENDED_VERSION")
	if recommended == "" {
		recommended = version.Get()
	}
	minVersion := os.Getenv("DUCKWAY_CLIENT_MIN_VERSION")
	required := os.Getenv("DUCKWAY_CLIENT_UPDATE_REQUIRED") == "1" && current != "" && current != recommended
	recommendedUpdate := current != "" && current != recommended
	reason := os.Getenv("DUCKWAY_CLIENT_UPDATE_REASON")
	if reason == "" && required {
		reason = "client update required by server policy"
	} else if reason == "" && recommendedUpdate {
		reason = "new client version available"
	}

	payload := map[string]interface{}{
		"server_version":             version.Get(),
		"client_current_version":     current,
		"client_recommended_version": recommended,
		"client_min_version":         minVersion,
		"update_required":            required,
		"update_recommended":         recommendedUpdate,
		"restart_required":           false,
		"reason":                     reason,
		"os":                         osName,
		"arch":                       arch,
		"binary":                     binary,
		"download_url":               "/download/" + binary,
		"sha256":                     sum,
		"size": func() int64 {
			info, _ := os.Stat(path)
			if info != nil {
				return info.Size()
			}
			return 0
		}(),
	}
	if ducklionErr == nil {
		payload["ducklion_binary"] = ducklionBinary
		payload["ducklion_download_url"] = "/download/" + ducklionBinary
		payload["ducklion_sha256"] = ducklionSum
		payload["ducklion_size"] = func() int64 {
			info, _ := os.Stat(ducklionPath)
			if info != nil {
				return info.Size()
			}
			return 0
		}()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func serveClientDownload(w http.ResponseWriter, r *http.Request, downloadDir string) {
	binary := r.PathValue("binary")
	if !isAllowedClientBinary(binary) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(downloadDir, binary))
}

func isAllowedClientBinary(binary string) bool {
	switch binary {
	case "duckway-client-linux-amd64",
		"duckway-client-linux-arm64",
		"duckway-client-darwin-amd64",
		"duckway-client-darwin-arm64",
		"ducklion-linux-amd64",
		"ducklion-linux-arm64",
		"ducklion-darwin-amd64",
		"ducklion-darwin-arm64",
		"ducklord-linux-amd64",
		"ducklord-linux-arm64",
		"ducklord-darwin-amd64",
		"ducklord-darwin-arm64":
		return true
	default:
		return false
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const installScript = `#!/bin/sh
set -e
DUCKWAY_SERVER="%s"
echo "Duckway client installer"
echo "Server: $DUCKWAY_SERVER"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) ARCH="amd64" ;; aarch64|arm64) ARCH="arm64" ;; *) echo "Unsupported: $ARCH"; exit 1 ;; esac
BINARY="duckway-client-${OS}-${ARCH}"
DUCKLION_BINARY="ducklion-${OS}-${ARCH}"
DUCKLORD_BINARY="ducklord-${OS}-${ARCH}"
INSTALL_COMPONENT="${DUCKWAY_INSTALL_COMPONENT:-}"
INSTALL_MODE="${DUCKWAY_INSTALL:-}"
if [ -z "$INSTALL_COMPONENT" ] && [ -r /dev/tty ] && [ -w /dev/tty ]; then
  echo ""
  echo "Install components:" > /dev/tty
  echo "  1) Duckway client + Ducklion  (remote host / CC / agent PTY)" > /dev/tty
  echo "  2) Ducklord only              (developer laptop SSH TUI)" > /dev/tty
  echo "  3) All tools                  (Duckway client + Ducklion + Ducklord)" > /dev/tty
  printf "Choose [1]: " > /dev/tty
  read component_choice < /dev/tty
  case "${component_choice:-1}" in
    1) INSTALL_COMPONENT="client" ;;
    2) INSTALL_COMPONENT="ducklord" ;;
    3) INSTALL_COMPONENT="all" ;;
    *)
      echo "Unsupported choice: $component_choice"
      exit 1
      ;;
  esac
fi
if [ -z "$INSTALL_COMPONENT" ]; then
  INSTALL_COMPONENT="client"
fi
case "$INSTALL_COMPONENT" in
  client|duckway|duckway-client) INSTALL_COMPONENT="client"; PRIMARY_NAME="duckway" ;;
  ducklord) PRIMARY_NAME="ducklord" ;;
  all) PRIMARY_NAME="duckway" ;;
  *)
    echo "Unsupported DUCKWAY_INSTALL_COMPONENT=$INSTALL_COMPONENT (use client, ducklord, or all)"
    exit 1
    ;;
esac
if [ -z "$INSTALL_MODE" ] && [ -z "${DUCKWAY_INSTALL_PATH:-}" ] && [ -r /dev/tty ] && [ -w /dev/tty ]; then
  echo ""
  echo "Install location:" > /dev/tty
  echo "  1) System-wide  /usr/local/bin/$PRIMARY_NAME  (uses sudo if needed)" > /dev/tty
  echo "  2) User-local   $HOME/.local/bin/$PRIMARY_NAME  (no sudo)" > /dev/tty
  echo "  3) Custom path" > /dev/tty
  printf "Choose [1]: " > /dev/tty
  read choice < /dev/tty
  case "${choice:-1}" in
    1) INSTALL_MODE="system" ;;
    2) INSTALL_MODE="user" ;;
    3)
      printf "Install path: " > /dev/tty
      read custom_path < /dev/tty
      if [ -z "$custom_path" ]; then
        echo "Error: custom path is required"
        exit 1
      fi
      DUCKWAY_INSTALL_PATH="$custom_path"
      INSTALL_MODE="custom"
      ;;
    *)
      echo "Unsupported choice: $choice"
      exit 1
      ;;
  esac
fi
if [ -z "$INSTALL_MODE" ]; then
  INSTALL_MODE="system"
fi
case "$INSTALL_MODE" in
  system)
    DEST="${DUCKWAY_INSTALL_PATH:-/usr/local/bin/$PRIMARY_NAME}"
    ;;
  user|local|user-local)
    DEST="${DUCKWAY_INSTALL_PATH:-$HOME/.local/bin/$PRIMARY_NAME}"
    ;;
  custom)
    if [ -z "${DUCKWAY_INSTALL_PATH:-}" ]; then
      echo "Error: DUCKWAY_INSTALL_PATH is required for custom install"
      exit 1
    fi
    DEST="$DUCKWAY_INSTALL_PATH"
    ;;
  *)
    echo "Unsupported DUCKWAY_INSTALL=$INSTALL_MODE (use system, user, or custom)"
    exit 1
    ;;
esac
DEST_DIR="$(dirname "$DEST")"
TMP_DIR="${TMPDIR:-/tmp}/duckway-install.$$"
mkdir -p "$TMP_DIR"
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM
download_tool() {
  binary="$1"
  out="$2"
  echo "Downloading: $DUCKWAY_SERVER/download/$binary"
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$DUCKWAY_SERVER/download/$binary" -o "$out"
  elif command -v wget >/dev/null 2>&1; then wget -q "$DUCKWAY_SERVER/download/$binary" -O "$out"
  else echo "Error: curl or wget required"; exit 1; fi
  chmod +x "$out"
}
if [ "$INSTALL_COMPONENT" = "client" ] || [ "$INSTALL_COMPONENT" = "all" ]; then
  download_tool "$BINARY" "$TMP_DIR/duckway"
  download_tool "$DUCKLION_BINARY" "$TMP_DIR/ducklion"
fi
if [ "$INSTALL_COMPONENT" = "ducklord" ] || [ "$INSTALL_COMPONENT" = "all" ]; then
  download_tool "$DUCKLORD_BINARY" "$TMP_DIR/ducklord"
fi
DUCKLION_DEST="$DEST_DIR/ducklion"
DUCKLORD_DEST="$DEST_DIR/ducklord"
if [ "$INSTALL_COMPONENT" = "ducklord" ]; then
  DUCKLORD_DEST="$DEST"
fi
if [ "$INSTALL_MODE" = "system" ] && [ ! -w "$DEST_DIR" ]; then
  sudo mkdir -p "$DEST_DIR"
  if [ -f "$TMP_DIR/duckway" ]; then sudo mv "$TMP_DIR/duckway" "$DEST"; fi
  if [ -f "$TMP_DIR/ducklion" ]; then sudo mv "$TMP_DIR/ducklion" "$DUCKLION_DEST"; fi
  if [ -f "$TMP_DIR/ducklord" ]; then sudo mv "$TMP_DIR/ducklord" "$DUCKLORD_DEST"; fi
else
  mkdir -p "$DEST_DIR"
  if [ -f "$TMP_DIR/duckway" ]; then mv "$TMP_DIR/duckway" "$DEST"; fi
  if [ -f "$TMP_DIR/ducklion" ]; then mv "$TMP_DIR/ducklion" "$DUCKLION_DEST"; fi
  if [ -f "$TMP_DIR/ducklord" ]; then mv "$TMP_DIR/ducklord" "$DUCKLORD_DEST"; fi
fi
if [ "$INSTALL_COMPONENT" = "client" ] || [ "$INSTALL_COMPONENT" = "all" ]; then
  echo "Installed: $DEST"
  echo "Installed: $DUCKLION_DEST"
fi
if [ "$INSTALL_COMPONENT" = "ducklord" ] || [ "$INSTALL_COMPONENT" = "all" ]; then
  echo "Installed: $DUCKLORD_DEST"
fi
case ":$PATH:" in
  *":$DEST_DIR:"*) ;;
  *)
    echo "Note: $DEST_DIR is not in PATH for this shell."
    echo "      Run this binary as: $DEST"
    ;;
esac
if [ "$INSTALL_COMPONENT" = "client" ] || [ "$INSTALL_COMPONENT" = "all" ]; then
  mkdir -p ~/.duckway
  if command -v curl >/dev/null 2>&1; then curl -fsSL "%s/skill/ca.pem" -o ~/.duckway/ca.pem
  else wget -q "%s/skill/ca.pem" -O ~/.duckway/ca.pem; fi
fi
echo "======================================"
echo "  Duckway tools installed!"
if [ "$INSTALL_COMPONENT" = "client" ] || [ "$INSTALL_COMPONENT" = "all" ]; then
  echo "  Next: duckway init"
fi
if [ "$INSTALL_COMPONENT" = "ducklord" ] || [ "$INSTALL_COMPONENT" = "all" ]; then
  echo "  Ducklord: ducklord tui"
fi
echo "  Server URL: $DUCKWAY_SERVER"
echo "======================================"
`
