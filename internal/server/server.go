package server

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/skill"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	config *Config
	db     *sql.DB
	mux    *http.ServeMux
}

func New(config *Config, db *sql.DB, contentFS embed.FS) (*Server, error) {
	s := &Server{
		config: config,
		db:     db,
		mux:    http.NewServeMux(),
	}

	if err := s.ensureAdminUser(); err != nil {
		return nil, fmt.Errorf("ensure admin user: %w", err)
	}

	if err := s.seedDefaultServices(); err != nil {
		return nil, fmt.Errorf("seed services: %w", err)
	}

	s.setupRoutes(contentFS)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Duckway-Token, X-Duckway-Key")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.mux.ServeHTTP(w, r)
}

func (s *Server) setupRoutes(contentFS embed.FS) {
	// Query objects
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

	// Services
	crypto := services.NewCrypto(s.config.EncryptionKey)
	resolver := services.NewKeyResolver(crypto, apiKeyQ, placeholderQ, groupQ, approvalQ)
	notifier := services.NewNotifier(notifQ)
	canarySvc := services.NewCanaryService(canaryQ)

	// Middleware
	adminAuth := middleware.NewAdminAuth(s.config.SessionSecret)
	clientAuth := middleware.NewClientAuth(clientQ)

	// Handlers
	authH := handlers.NewAuthHandler(userQ, adminAuth)
	serviceH := handlers.NewServiceHandler(serviceQ)
	apiKeyH := handlers.NewAPIKeyHandler(apiKeyQ, serviceQ, crypto)
	placeholderH := handlers.NewPlaceholderHandler(placeholderQ, serviceQ, clientQ)
	clientH := handlers.NewClientHandler(clientQ, placeholderQ, serviceQ, canarySvc)
	groupH := handlers.NewGroupHandler(groupQ, serviceQ)
	approvalH := handlers.NewApprovalHandler(approvalQ, placeholderQ)
	notifH := handlers.NewNotificationHandler(notifQ)
	canaryH := handlers.NewCanaryHandler(canaryQ, canarySvc)
	proxyH := handlers.NewProxyHandler(serviceQ, resolver, requestLogQ, approvalQ, notifier)
	adminPageH := handlers.NewAdminHandler(contentFS, userQ, serviceQ, apiKeyQ, placeholderQ, clientQ, groupQ, approvalQ, requestLogQ, notifQ, canaryQ, adminAuth)

	// Static files
	staticFS, err := fs.Sub(contentFS, "static")
	if err != nil {
		log.Fatalf("Failed to get static FS: %v", err)
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Admin pages (HTML)
	s.mux.HandleFunc("GET /admin/login", adminPageH.LoginPage)
	s.mux.HandleFunc("POST /admin/login", adminPageH.LoginSubmit)

	// Admin pages behind auth
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
	adminPageMux.HandleFunc("GET /admin/docs", adminPageH.DocsPage)
	adminPageMux.HandleFunc("POST /admin/approvals/{id}/approve", adminPageH.ApproveAction)
	adminPageMux.HandleFunc("POST /admin/approvals/{id}/reject", adminPageH.RejectAction)
	s.mux.Handle("/admin/", adminAuth.Middleware(adminPageMux))

	// Public API routes
	s.mux.HandleFunc("POST /api/auth/login", authH.Login)
	s.mux.HandleFunc("POST /api/auth/logout", authH.Logout)

	// Admin API routes (require admin auth)
	adminAPIMux := http.NewServeMux()
	adminAPIMux.HandleFunc("GET /api/services", serviceH.List)
	adminAPIMux.HandleFunc("POST /api/services", serviceH.Create)
	adminAPIMux.HandleFunc("GET /api/services/{id}", serviceH.Get)
	adminAPIMux.HandleFunc("PUT /api/services/{id}", serviceH.Update)
	adminAPIMux.HandleFunc("DELETE /api/services/{id}", serviceH.Delete)

	adminAPIMux.HandleFunc("GET /api/keys", apiKeyH.List)
	adminAPIMux.HandleFunc("POST /api/keys", apiKeyH.Create)
	adminAPIMux.HandleFunc("DELETE /api/keys/{id}", apiKeyH.Delete)

	adminAPIMux.HandleFunc("GET /api/placeholders", placeholderH.List)
	adminAPIMux.HandleFunc("POST /api/placeholders", placeholderH.Create)
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
	adminAPIMux.HandleFunc("DELETE /api/notifications/{id}", notifH.Delete)

	adminAPIMux.HandleFunc("GET /api/canary/settings", canaryH.GetSettings)
	adminAPIMux.HandleFunc("POST /api/canary/settings", canaryH.SaveSettings)
	adminAPIMux.HandleFunc("GET /api/canary/clients/{clientId}", canaryH.ListByClient)
	adminAPIMux.HandleFunc("POST /api/canary/clients/{clientId}/generate", canaryH.GenerateForClient)

	s.mux.Handle("/api/", adminAuth.Middleware(adminAPIMux))

	// Client routes (require client auth)
	clientMux := http.NewServeMux()
	clientMux.HandleFunc("GET /client/keys", clientH.GetKeys)
	clientMux.HandleFunc("GET /client/canaries", canaryH.ClientGetCanaries)
	s.mux.Handle("/client/", clientAuth.Middleware(clientMux))

	// Proxy routes (require client auth)
	proxyMux := http.NewServeMux()
	proxyMux.HandleFunc("/", proxyH.Handle)
	s.mux.Handle("/proxy/", clientAuth.Middleware(proxyMux))

	// Internal API (for mitmproxy addon, secret-authenticated)
	internalH := handlers.NewInternalHandler(resolver)
	s.mux.HandleFunc("POST /internal/resolve", internalH.Resolve)

	// Skill file (public, no auth)
	s.mux.HandleFunc("GET /skill/duckway-agent.md", func(w http.ResponseWriter, r *http.Request) {
		data, err := skill.Content.ReadFile("duckway-agent.md")
		if err != nil {
			http.Error(w, "skill file not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(data)
	})

	// Root redirect to admin
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	})
}

func (s *Server) ensureAdminUser() error {
	userQ := queries.NewAdminUserQueries(s.db)
	count, err := userQ.Count()
	if err != nil {
		return err
	}

	if count > 0 {
		return nil
	}

	password, err := services.GeneratePassword(16)
	if err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	id, _ := services.GenerateToken(16)
	if err := userQ.Create(id, "duckway", string(hash)); err != nil {
		return err
	}

	log.Println("========================================")
	log.Println("  First-run admin credentials:")
	log.Printf("  Username: duckway")
	log.Printf("  Password: %s", password)
	log.Println("  (save this — shown only once)")
	log.Println("========================================")

	return nil
}

func (s *Server) seedDefaultServices() error {
	svcQ := queries.NewServiceQueries(s.db)
	count, err := svcQ.Count()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	defaults := []models.Service{
		{
			Name:         "openai",
			DisplayName:  "OpenAI API",
			UpstreamURL:  "https://api.openai.com",
			HostPattern:  "api.openai.com",
			AuthType:     "bearer",
			AuthHeader:   "Authorization",
			AuthPrefix:   "Bearer ",
			KeyPrefix:    "sk-",
			KeyLength:    164,
			KeyDirectory: ".config/openai/credentials",
			IsActive:     true,
		},
		{
			Name:         "anthropic",
			DisplayName:  "Anthropic API",
			UpstreamURL:  "https://api.anthropic.com",
			HostPattern:  "api.anthropic.com",
			AuthType:     "header",
			AuthHeader:   "x-api-key",
			AuthPrefix:   "",
			KeyPrefix:    "sk-ant-",
			KeyLength:    108,
			KeyDirectory: ".config/anthropic/credentials",
			IsActive:     true,
		},
		{
			Name:         "github",
			DisplayName:  "GitHub API",
			UpstreamURL:  "https://api.github.com",
			HostPattern:  "api.github.com",
			AuthType:     "bearer",
			AuthHeader:   "Authorization",
			AuthPrefix:   "Bearer ",
			KeyPrefix:    "ghp_",
			KeyLength:    40,
			KeyDirectory: ".config/gh/credentials",
			IsActive:     true,
		},
		{
			Name:         "discord",
			DisplayName:  "Discord API",
			UpstreamURL:  "https://discord.com/api",
			HostPattern:  "discord.com",
			AuthType:     "header",
			AuthHeader:   "Authorization",
			AuthPrefix:   "Bot ",
			KeyPrefix:    "",
			KeyLength:    72,
			KeyDirectory: ".config/discord/credentials",
			IsActive:     true,
		},
		{
			Name:         "telegram",
			DisplayName:  "Telegram Bot API",
			UpstreamURL:  "https://api.telegram.org",
			HostPattern:  "api.telegram.org",
			AuthType:     "bearer",
			AuthHeader:   "Authorization",
			AuthPrefix:   "Bearer ",
			KeyPrefix:    "",
			KeyLength:    46,
			KeyDirectory: ".config/telegram/credentials",
			IsActive:     true,
		},
	}

	for _, svc := range defaults {
		id, _ := services.GenerateToken(16)
		svc.ID = id
		if err := svcQ.Create(&svc); err != nil {
			log.Printf("Warning: failed to seed service %s: %v", svc.Name, err)
		}
	}

	log.Printf("Seeded %d default services (openai, anthropic, github, discord, telegram)", len(defaults))
	return nil
}
