package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	config   *Config
	db       *sql.DB
	mux      *http.ServeMux
	notifier *services.Notifier
}

// New creates a combined server (admin + gateway on one port).
func New(config *Config, db *sql.DB, contentFS fs.FS) (*Server, error) {
	s := &Server{config: config, db: db, mux: http.NewServeMux()}

	if err := s.ensureAdminUser(); err != nil {
		return nil, fmt.Errorf("ensure admin user: %w", err)
	}
	if err := s.seedDefaultServices(); err != nil {
		return nil, fmt.Errorf("seed services: %w", err)
	}

	ss := s.initShared()
	s.SetupAdminRoutes(contentFS, ss)
	s.SetupGatewayRoutes(ss)
	s.startApprovalListeners()
	s.startOAuthRefresher(ss)

	return s, nil
}

// NewAdmin creates an admin-only server (no proxy/client routes).
func NewAdmin(config *Config, db *sql.DB, contentFS fs.FS) (*Server, error) {
	s := &Server{config: config, db: db, mux: http.NewServeMux()}

	if err := s.ensureAdminUser(); err != nil {
		return nil, fmt.Errorf("ensure admin user: %w", err)
	}
	if err := s.seedDefaultServices(); err != nil {
		return nil, fmt.Errorf("seed services: %w", err)
	}

	ss := s.initShared()
	s.SetupAdminRoutes(contentFS, ss)
	s.startApprovalListeners()
	s.startOAuthRefresher(ss)

	return s, nil
}

// NewGateway creates a gateway-only server (proxy + client API, no admin).
func NewGateway(config *Config, db *sql.DB) (*Server, error) {
	s := &Server{config: config, db: db, mux: http.NewServeMux()}

	ss := s.initShared()
	s.SetupGatewayRoutes(ss)

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

func (s *Server) ensureAdminUser() error {
	userQ := queries.NewAdminUserQueries(s.db)
	count, err := userQ.Count()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	var password string
	if os.Getenv("DUCKWAY_DEV") == "1" {
		password = "duckway"
	} else {
		var err error
		password, err = services.GeneratePassword(16)
		if err != nil {
			return err
		}
	}

	hash, hashErr := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if hashErr != nil {
		return hashErr
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
		{Name: "heartbeat", DisplayName: "Duckway Heartbeat", UpstreamURL: "internal://heartbeat", HostPattern: "heartbeat", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "dw-hb-", KeyLength: 32, IsActive: true},
		{Name: "openai", DisplayName: "OpenAI API", UpstreamURL: "https://api.openai.com", HostPattern: "api.openai.com", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "sk-", KeyLength: 164, KeyDirectory: ".config/openai/credentials", IsActive: true},
		{Name: "anthropic", DisplayName: "Anthropic API", UpstreamURL: "https://api.anthropic.com", HostPattern: "api.anthropic.com", AuthType: "header", AuthHeader: "x-api-key", KeyPrefix: "sk-ant-", KeyLength: 108, KeyDirectory: ".config/anthropic/credentials", IsActive: true},
		{Name: "github", DisplayName: "GitHub API", UpstreamURL: "https://api.github.com", HostPattern: "api.github.com", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "ghp_", KeyLength: 40, KeyDirectory: ".config/gh/credentials", IsActive: true},
		{Name: "discord", DisplayName: "Discord API", UpstreamURL: "https://discord.com/api", HostPattern: "discord.com", AuthType: "header", AuthHeader: "Authorization", AuthPrefix: "Bot ", KeyLength: 72, KeyDirectory: ".config/discord/credentials", IsActive: true},
		{Name: "telegram", DisplayName: "Telegram Bot API", UpstreamURL: "https://api.telegram.org", HostPattern: "api.telegram.org", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyLength: 46, KeyDirectory: ".config/telegram/credentials", IsActive: true},
	}

	for _, svc := range defaults {
		id, _ := services.GenerateToken(16)
		svc.ID = id
		if err := svcQ.Create(&svc); err != nil {
			log.Printf("Warning: failed to seed service %s: %v", svc.Name, err)
		}
	}

	// Create heartbeat API key
	hbSvc, err := svcQ.GetByName("heartbeat")
	if err == nil {
		crypto := services.NewCrypto(s.config.EncryptionKey)
		enc, _ := crypto.Encrypt("internal-heartbeat-key")
		apiKeyQ := queries.NewAPIKeyQueries(s.db)
		keyID, _ := services.GenerateToken(16)
		apiKeyQ.Create(&models.APIKey{ID: keyID, ServiceID: hbSvc.ID, Name: "Heartbeat Internal", KeyEncrypted: enc, IsActive: true})
	}

	log.Printf("Seeded %d default services", len(defaults))
	return nil
}

func (s *Server) startOAuthRefresher(ss *SharedServices) {
	oauthQ := queries.NewOAuthQueries(s.db)
	refresher := services.NewOAuthRefresher(oauthQ, ss.Crypto)
	refresher.Start()
}

func (s *Server) startApprovalListeners() {
	notifQ := queries.NewNotificationQueries(s.db)
	approvalQ := queries.NewApprovalQueries(s.db)
	placeholderQ := queries.NewPlaceholderQueries(s.db)
	notifier := s.notifier

	channels, err := notifQ.ListActive()
	if err != nil {
		return
	}

	approveFunc := func(approvalID string) error {
		approval, err := approvalQ.GetByID(approvalID)
		ttl := 1440
		if err == nil && approval != nil {
			ph, phErr := placeholderQ.GetByID(approval.PlaceholderID)
			if phErr == nil && ph.ApprovalTTLMinutes > 0 {
				ttl = ph.ApprovalTTLMinutes
			}
		}
		return approvalQ.Approve(approvalID, fmt.Sprintf("datetime('now', '+%d minutes')", ttl))
	}
	rejectFunc := func(approvalID string) error {
		return approvalQ.Reject(approvalID)
	}

	for _, ch := range channels {
		switch ch.ChannelType {
		case "discord_bot":
			var cfg struct {
				BotToken  string `json:"bot_token"`
				ChannelID string `json:"channel_id"`
			}
			if err := json.Unmarshal([]byte(ch.Config), &cfg); err != nil || cfg.BotToken == "" {
				continue
			}
			gw := services.NewDiscordGateway(cfg.BotToken, cfg.ChannelID, approveFunc, rejectFunc)
			gw.Start()
			if notifier != nil {
				notifier.Gateways.Store(cfg.ChannelID, gw)
			}
			log.Printf("Started Discord Gateway for channel %s (%s)", cfg.ChannelID, ch.Name)

		case "telegram":
			var cfg struct {
				BotToken string `json:"bot_token"`
				ChatID   string `json:"chat_id"`
			}
			if err := json.Unmarshal([]byte(ch.Config), &cfg); err != nil || cfg.BotToken == "" {
				continue
			}
			poller := services.NewTelegramPoller(cfg.BotToken, cfg.ChatID, approveFunc, rejectFunc)
			poller.Start()
			log.Printf("Started Telegram poller for chat %s (%s)", cfg.ChatID, ch.Name)
		}
	}
}
