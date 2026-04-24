package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
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

	Crypto    *services.Crypto
	Resolver  *services.KeyResolver
	Notifier  *services.Notifier
	CanarySvc *services.CanaryService

	AdminAuth  *middleware.AdminAuth
	ClientAuth *middleware.ClientAuth
}

func NewSharedServices(config *Config, db interface{ QueryRow(string, ...interface{}) interface{} }) *SharedServices {
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
		NotifQ: notifQ, CanaryQ: canaryQ,
		Crypto: crypto, Resolver: resolver, Notifier: notifier, CanarySvc: canarySvc,
		AdminAuth: adminAuth, ClientAuth: clientAuth,
	}
}

// SetupAdminRoutes adds admin panel + management API routes.
func (s *Server) SetupAdminRoutes(contentFS fs.FS, ss *SharedServices) {
	settingsQ := queries.NewSettingsQueries(s.db)
	authH := handlers.NewAuthHandler(ss.UserQ, ss.AdminAuth)
	serviceH := handlers.NewServiceHandler(ss.ServiceQ)
	apiKeyH := handlers.NewAPIKeyHandler(ss.APIKeyQ, ss.ServiceQ, ss.Crypto)
	placeholderH := handlers.NewPlaceholderHandler(ss.PlaceholderQ, ss.ServiceQ, ss.ClientQ)
	clientH := handlers.NewClientHandler(ss.ClientQ, ss.PlaceholderQ, ss.ServiceQ, ss.APIKeyQ, ss.CanarySvc)
	groupH := handlers.NewGroupHandler(ss.GroupQ, ss.ServiceQ)
	approvalH := handlers.NewApprovalHandler(ss.ApprovalQ, ss.PlaceholderQ)
	notifH := handlers.NewNotificationHandler(ss.NotifQ, ss.Notifier)
	canaryH := handlers.NewCanaryHandler(ss.CanaryQ, ss.CanarySvc)
	adminPageH := handlers.NewAdminHandler(contentFS, ss.UserQ, ss.ServiceQ, ss.APIKeyQ, ss.PlaceholderQ, ss.ClientQ, ss.GroupQ, ss.ApprovalQ, ss.RequestLogQ, ss.NotifQ, ss.CanaryQ, ss.AdminAuth)

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
	adminPageMux.HandleFunc("GET /admin/placeholders", adminPageH.PlaceholdersPage)
	adminPageMux.HandleFunc("GET /admin/clients", adminPageH.ClientsPage)
	adminPageMux.HandleFunc("GET /admin/groups", adminPageH.GroupsPage)
	adminPageMux.HandleFunc("GET /admin/approvals", adminPageH.ApprovalsPage)
	adminPageMux.HandleFunc("GET /admin/logs", adminPageH.LogsPage)
	adminPageMux.HandleFunc("GET /admin/notifications", adminPageH.NotificationsPage)
	adminPageMux.HandleFunc("GET /admin/canary", adminPageH.CanaryPage)
	adminPageMux.HandleFunc("GET /admin/oauth", adminPageH.OAuthPage)
	adminPageMux.HandleFunc("GET /admin/settings", adminPageH.SettingsPage)
	adminPageMux.HandleFunc("GET /admin/docs", adminPageH.DocsPage)
	adminPageMux.HandleFunc("POST /admin/approvals/{id}/approve", adminPageH.ApproveAction)
	adminPageMux.HandleFunc("POST /admin/approvals/{id}/reject", adminPageH.RejectAction)
	s.mux.Handle("/admin/", ss.AdminAuth.Middleware(adminPageMux))

	// Public API routes
	s.mux.HandleFunc("POST /api/auth/login", authH.Login)
	s.mux.HandleFunc("POST /api/auth/logout", authH.Logout)

	// Admin API
	adminAPIMux := http.NewServeMux()
	adminAPIMux.HandleFunc("GET /api/services", serviceH.List)
	adminAPIMux.HandleFunc("POST /api/services", serviceH.Create)
	adminAPIMux.HandleFunc("GET /api/services/{id}", serviceH.Get)
	adminAPIMux.HandleFunc("PUT /api/services/{id}", serviceH.Update)
	adminAPIMux.HandleFunc("DELETE /api/services/{id}", serviceH.Delete)
	adminAPIMux.HandleFunc("GET /api/services/{id}/acl-templates", serviceH.ListACLTemplates)
	adminAPIMux.HandleFunc("POST /api/services/{id}/acl-templates", serviceH.ApplyACLTemplate)

	adminAPIMux.HandleFunc("GET /api/keys", apiKeyH.List)
	adminAPIMux.HandleFunc("POST /api/keys", apiKeyH.Create)
	adminAPIMux.HandleFunc("PUT /api/keys/{id}", apiKeyH.Update)
	adminAPIMux.HandleFunc("DELETE /api/keys/{id}", apiKeyH.Delete)
	adminAPIMux.HandleFunc("GET /api/keys/{id}/acl-templates", apiKeyH.ListACLTemplates)
	adminAPIMux.HandleFunc("POST /api/keys/{id}/acl-templates", apiKeyH.ApplyACLTemplate)
	adminAPIMux.HandleFunc("POST /api/keys/{id}/acl", apiKeyH.SetACL)

	adminAPIMux.HandleFunc("GET /api/placeholders", placeholderH.List)
	adminAPIMux.HandleFunc("GET /api/placeholders/with-approvals", func(w http.ResponseWriter, r *http.Request) {
		placeholderH.ListWithApprovals(w, r, ss.ApprovalQ)
	})
	adminAPIMux.HandleFunc("POST /api/placeholders", placeholderH.Create)
	adminAPIMux.HandleFunc("PUT /api/placeholders/{id}", placeholderH.Update)
	adminAPIMux.HandleFunc("DELETE /api/placeholders/{id}", placeholderH.Delete)

	adminAPIMux.HandleFunc("GET /api/clients", clientH.List)
	adminAPIMux.HandleFunc("POST /api/clients", clientH.Create)
	adminAPIMux.HandleFunc("DELETE /api/clients/{id}", clientH.Delete)
	adminAPIMux.HandleFunc("POST /api/clients/{id}/canary", clientH.ToggleCanary)

	adminAPIMux.HandleFunc("GET /api/groups", groupH.List)
	adminAPIMux.HandleFunc("POST /api/groups", groupH.Create)
	adminAPIMux.HandleFunc("DELETE /api/groups/{id}", groupH.Delete)
	adminAPIMux.HandleFunc("POST /api/groups/{id}/members", groupH.AddMember)
	adminAPIMux.HandleFunc("DELETE /api/groups/{id}/members/{keyId}", groupH.RemoveMember)

	adminAPIMux.HandleFunc("GET /api/approvals", approvalH.ListPending)
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

	oauthH := handlers.NewOAuthHandler(queries.NewOAuthQueries(s.db), ss.PlaceholderQ, ss.ServiceQ, ss.Crypto)
	adminAPIMux.HandleFunc("GET /api/oauth", oauthH.List)
	adminAPIMux.HandleFunc("POST /api/oauth", oauthH.Create)
	adminAPIMux.HandleFunc("DELETE /api/oauth/{id}", oauthH.Delete)

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
	canaryH := handlers.NewCanaryHandler(ss.CanaryQ, ss.CanarySvc)
	proxyH := handlers.NewProxyHandler(ss.ServiceQ, ss.Resolver, ss.RequestLogQ, ss.ApprovalQ, ss.Notifier)
	internalH := handlers.NewInternalHandler(ss.Resolver)

	// Client routes (require client auth)
	clientMux := http.NewServeMux()
	clientMux.HandleFunc("GET /client/keys", clientH.GetKeys)
	clientMux.HandleFunc("GET /client/canaries", canaryH.ClientGetCanaries)

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

	// Claude credentials endpoint (client auth required)
	oauthClientH := handlers.NewOAuthHandler(queries.NewOAuthQueries(s.db), ss.PlaceholderQ, ss.ServiceQ, ss.Crypto)
	clientMux.HandleFunc("GET /client/claude-credentials", oauthClientH.ClientGetCredentials)

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

	s.mux.Handle("/client/", ss.ClientAuth.Middleware(clientMux))

	// Service host map (for HTTPS proxy client)
	s.mux.HandleFunc("GET /client/services", func(w http.ResponseWriter, r *http.Request) {
		svcs, _ := ss.ServiceQ.List()
		type svcInfo struct {
			Name        string `json:"name"`
			HostPattern string `json:"host_pattern"`
			UpstreamURL string `json:"upstream_url"`
		}
		var result []svcInfo
		for _, svc := range svcs {
			if svc.IsActive && !strings.HasPrefix(svc.UpstreamURL, "internal://") {
				result = append(result, svcInfo{Name: svc.Name, HostPattern: svc.HostPattern, UpstreamURL: svc.UpstreamURL})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

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
	downloadDir := os.Getenv("DUCKWAY_DOWNLOAD_DIR")
	if downloadDir == "" {
		downloadDir = "/srv/downloads"
	}
	if info, err := os.Stat(downloadDir); err == nil && info.IsDir() {
		s.mux.Handle("GET /download/", http.StripPrefix("/download/", http.FileServer(http.Dir(downloadDir))))
	}

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

const installScript = `#!/bin/sh
set -e
DUCKWAY_SERVER="%s"
echo "Duckway client installer"
echo "Server: $DUCKWAY_SERVER"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) ARCH="amd64" ;; aarch64|arm64) ARCH="arm64" ;; *) echo "Unsupported: $ARCH"; exit 1 ;; esac
BINARY="duckway-client-${OS}-${ARCH}"
DEST="/usr/local/bin/duckway"
echo "Downloading: $DUCKWAY_SERVER/download/$BINARY"
if command -v curl >/dev/null 2>&1; then curl -fsSL "$DUCKWAY_SERVER/download/$BINARY" -o /tmp/duckway
elif command -v wget >/dev/null 2>&1; then wget -q "$DUCKWAY_SERVER/download/$BINARY" -O /tmp/duckway
else echo "Error: curl or wget required"; exit 1; fi
chmod +x /tmp/duckway
if [ -w /usr/local/bin ]; then mv /tmp/duckway "$DEST"; else sudo mv /tmp/duckway "$DEST"; fi
echo "Installed: $DEST"
mkdir -p ~/.duckway
if command -v curl >/dev/null 2>&1; then curl -fsSL "%s/skill/ca.pem" -o ~/.duckway/ca.pem
else wget -q "%s/skill/ca.pem" -O ~/.duckway/ca.pem; fi
echo "======================================"
echo "  Duckway client installed!"
echo "  Next: duckway init"
echo "  Server URL: $DUCKWAY_SERVER"
echo "======================================"
`
