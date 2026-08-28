package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
	"golang.org/x/crypto/bcrypt"
)

type stopper interface{ Stop() }

type Server struct {
	config   *Config
	db       *sql.DB
	mux      *http.ServeMux
	notifier *services.Notifier

	stopOnce sync.Once
	stopCh   chan struct{} // closed to stop anonymous sweeper goroutines
	stoppers []stopper     // services with their own Stop()
}

// Shutdown stops all background goroutines and services. Call after
// http.Server.Shutdown() so the DB is not closed while goroutines are
// still issuing queries against it.
func (s *Server) Shutdown() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		for _, svc := range s.stoppers {
			svc.Stop()
		}
	})
}

func (s *Server) register(svc stopper) stopper {
	s.stoppers = append(s.stoppers, svc)
	return svc
}

// New creates a combined server (admin + gateway on one port).
func New(config *Config, db *sql.DB, contentFS fs.FS) (*Server, error) {
	s := &Server{config: config, db: db, mux: http.NewServeMux(), stopCh: make(chan struct{})}
	s.setupHealthRoute()

	if err := s.ensureAdminUser(); err != nil {
		return nil, fmt.Errorf("ensure admin user: %w", err)
	}
	if err := s.seedDefaultServices(); err != nil {
		return nil, fmt.Errorf("seed services: %w", err)
	}
	services.SeedDefaultStatusline(queries.NewSettingsQueries(s.db))

	ss := s.initShared()
	// Refresher must be on ss before SetupAdminRoutes so the OAuth handler
	// can wire its manual-refresh endpoint to it.
	s.startOAuthRefresher(ss)
	s.SetupAdminRoutes(contentFS, ss)
	s.SetupGatewayRoutes(ss)
	s.startApprovalListeners()
	s.startApprovalSweeper(ss)
	s.startCCBackground(ss)
	s.startKeyGroupSweeper()
	s.startUsageRetentionSweeper()

	return s, nil
}

// NewAdmin creates an admin-only server (no proxy/client routes).
//
// Important: the Discord WSS gateway does NOT run here — it lives in the
// gateway process (NewGateway) because CCEventHub is in-process pub/sub
// and the SSE consumer (`duckway cc watch`) connects to the gateway's
// /client/cc/events. Running the WSS gateway in admin would publish
// events to a hub the SSE subscriber can't see.
func NewAdmin(config *Config, db *sql.DB, contentFS fs.FS) (*Server, error) {
	s := &Server{config: config, db: db, mux: http.NewServeMux(), stopCh: make(chan struct{})}
	s.setupHealthRoute()

	if err := s.ensureAdminUser(); err != nil {
		return nil, fmt.Errorf("ensure admin user: %w", err)
	}
	if err := s.seedDefaultServices(); err != nil {
		return nil, fmt.Errorf("seed services: %w", err)
	}
	services.SeedDefaultStatusline(queries.NewSettingsQueries(s.db))

	ss := s.initShared()
	s.startOAuthRefresher(ss)
	s.SetupAdminRoutes(contentFS, ss)
	s.startApprovalListeners()
	s.startApprovalSweeper(ss)

	return s, nil
}

// NewGateway creates a gateway-only server (proxy + client API, no admin).
//
// The Discord WSS gateway + CC command parser + inbox cleanup all run
// here so the in-process CCEventHub can fan out to the SSE subscriber
// (the daemon) that connects on /client/cc/events.
func NewGateway(config *Config, db *sql.DB) (*Server, error) {
	s := &Server{config: config, db: db, mux: http.NewServeMux(), stopCh: make(chan struct{})}
	s.setupHealthRoute()

	// Seed the default statusline here too: in split deployments the
	// gateway sometimes boots before the admin process, and the
	// `agent_statusline_script` setting needs to be populated before
	// the first /client/statusline fetch lands.
	services.SeedDefaultStatusline(queries.NewSettingsQueries(s.db))

	ss := s.initShared()
	s.SetupGatewayRoutes(ss)
	s.startCCBackground(ss)
	s.startUsageRetentionSweeper()

	return s, nil
}

func (s *Server) setupHealthRoute() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.PingContext(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
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

// ResetAdminPassword sets the admin user's password to a fresh random value.
// Returns the new password (printed once) and any error.
// If the user doesn't exist, it is created.
func ResetAdminPassword(db *sql.DB, username string) (string, error) {
	if username == "" {
		username = "duckway"
	}

	password, err := services.GeneratePassword(16)
	if err != nil {
		return "", err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}

	userQ := queries.NewAdminUserQueries(db)
	if err := userQ.UpdatePassword(username, string(hash)); err != nil {
		// User doesn't exist yet — create them
		id, _ := services.GenerateToken(16)
		if cerr := userQ.Create(id, username, string(hash)); cerr != nil {
			return "", fmt.Errorf("update failed: %w; create failed: %v", err, cerr)
		}
	}
	return password, nil
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
	// Use heartbeat as the canonical marker for "defaults already seeded".
	// Don't use total count — migrations may pre-seed individual rows
	// (e.g. discord_api for the Control Channel feature).
	if _, err := svcQ.GetByName("heartbeat"); err == nil {
		return nil
	}

	defaults := []models.Service{
		{Name: "heartbeat", DisplayName: "Duckway Heartbeat", UpstreamURL: "internal://heartbeat", HostPattern: "heartbeat", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "dw-hb-", KeyLength: 32, DeliveryMode: "proxy", IsActive: true},
		{Name: "openai", DisplayName: "OpenAI API", UpstreamURL: "https://api.openai.com", HostPattern: "api.openai.com", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "sk-", KeyLength: 164, KeyDirectory: ".config/openai/credentials", DeliveryMode: "proxy", IsActive: true},
		{Name: "anthropic", DisplayName: "Anthropic API", UpstreamURL: "https://api.anthropic.com", HostPattern: "api.anthropic.com", AuthType: "header", AuthHeader: "x-api-key", KeyPrefix: "sk-ant-", KeyLength: 108, KeyDirectory: ".config/anthropic/credentials", DeliveryMode: "proxy", IsActive: true},
		{Name: "xai", DisplayName: "xAI / Grok", UpstreamURL: "https://api.x.ai", HostPattern: "api.x.ai,cli-chat-proxy.grok.com", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "xai-", KeyLength: 80, KeyDirectory: ".config/xai/credentials", DeliveryMode: "proxy", IsActive: true},
		// GitHub: default to simple phantom-token proxy mode. Fine-grained
		// PATs use github_pat_*; loan_proxy can still be enabled later for
		// high-bandwidth git traffic.
		{Name: "github", DisplayName: "GitHub API + Git", UpstreamURL: "https://api.github.com", HostPattern: "api.github.com,github.com", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "github_pat_", KeyLength: 93, KeyDirectory: ".config/gh/credentials", DeliveryMode: "proxy", IsActive: true},
		{Name: "discord", DisplayName: "Discord API", UpstreamURL: "https://discord.com/api/v10", HostPattern: "discord.com,api.discord.com,gateway.discord.gg,*.discordapp.net", AuthType: "header", AuthHeader: "Authorization", AuthPrefix: "Bot ", KeyLength: 72, KeyDirectory: ".config/discord/credentials", DeliveryMode: "proxy", IsActive: true},
		{Name: "telegram", DisplayName: "Telegram Bot API", UpstreamURL: "https://api.telegram.org", HostPattern: "api.telegram.org", AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyLength: 46, KeyDirectory: ".config/telegram/credentials", DeliveryMode: "proxy", IsActive: true},
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
	refresher := services.NewTokenRefresher(ss.APIKeyQ, ss.Crypto)
	refresher.Start()
	ss.Refresher = refresher
	s.register(refresher)
}

func (s *Server) startApprovalSweeper(ss *SharedServices) {
	sweeper := services.NewApprovalSweeper(ss.ApprovalQ, ss.SettingsQ)
	sweeper.Start()
	s.register(sweeper)
}

// startCCBackground spins up the multi-bot Discord gateway connections used
// by Control Channels and the inbox cleanup goroutine. Both are best-effort:
// gateway errors retry forever, cleanup errors are logged.
//
// In test/dev environments without a real Discord, set
// DUCKWAY_CC_DISABLE_GATEWAY=1 to skip the WSS dial — the inbox cleanup
// loop still runs and the REST/CRUD paths work normally.
func (s *Server) startCCBackground(ss *SharedServices) {
	ccQ := queries.NewControlChannelQueries(s.db)
	if os.Getenv("DUCKWAY_CC_DISABLE_GATEWAY") != "1" {
		mgr := services.NewCCGatewayManager(ccQ, ss.APIKeyQ, ss.Crypto, ss.CCHub, ss.CCApprovals)
		mgr.Start()
		s.register(mgr)
	}
	services.StartInboxCleanup(ccQ, ss.SettingsQ, s.stopCh)
}

// startKeyGroupSweeper runs ClearExpiredExhausted every 60 seconds.
func (s *Server) startKeyGroupSweeper() {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				if err := queries.ClearExpiredExhausted(s.db); err != nil {
					log.Printf("key group sweeper: %v", err)
				}
			}
		}
	}()
}

// conversationUsageRetentionDays bounds how long per-request token usage
// rows are kept. The usage detail API exposes a 90-day heatmap window, so raw
// events must remain available for at least that full period.
const conversationUsageRetentionDays = 90

// startUsageRetentionSweeper prunes conversation_usage rows older than
// the retention window. Runs hourly (the table is append-only and grows
// slowly, so frequent sweeps aren't needed).
func (s *Server) startUsageRetentionSweeper() {
	convQ := queries.NewConversationUsageQueries(s.db)
	prune := func() {
		if n, err := convQ.PruneOlderThan(conversationUsageRetentionDays); err != nil {
			log.Printf("usage retention sweeper: %v", err)
		} else if n > 0 {
			log.Printf("usage retention: pruned %d conversation_usage rows older than %dd", n, conversationUsageRetentionDays)
		}
	}
	go func() {
		prune() // once at startup
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				prune()
			}
		}
	}()
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
			s.register(gw)
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
			s.register(poller)
			log.Printf("Started Telegram poller for chat %s (%s)", cfg.ChatID, ch.Name)
		}
	}
}
