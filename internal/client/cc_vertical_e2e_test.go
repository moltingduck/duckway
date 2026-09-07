package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	duckliondaemon "github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestDiscordOwnershipRejectionVerticalE2E(t *testing.T) {
	configDir := t.TempDir()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	ducklion, err := duckliondaemon.Open(context.Background(), duckliondaemon.Options{Root: filepath.Join(configDir, "ducklion"), RuntimeLauncher: func(specPath string) error {
		go func() { _ = duckliondaemon.RunManagedSupervisor(runtimeCtx, specPath) }()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ducklionDone := make(chan error, 1)
	go func() { ducklionDone <- ducklion.Serve() }()
	defer func() { _ = ducklion.Close(); <-ducklionDone }()
	terminal, err := duckliondaemon.Dial(ducklion.SocketPath(), "vertical-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	session, err := terminal.CreateSession(context.Background(), protocol.SessionCreate{Handle: "vertical", Kind: model.KindAgent, AgentType: "fixture", CWD: configDir,
		Command: []string{"sh", "-c", "while IFS= read -r line; do printf 'unexpected:%s\\n' \"$line\"; done"}})
	if err != nil {
		t.Fatal(err)
	}
	management, err := duckliondaemon.DialCC(ducklion.SocketPath(), "dwch_vertical_management")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := management.BindDiscordSession(context.Background(), "vertical-bind", session.SessionID, "dwch_vertical_task"); err != nil {
		t.Fatal(err)
	}
	_ = management.Close()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	serviceQueries := queries.NewServiceQueries(db)
	if err := serviceQueries.Create(&models.Service{ID: "discord-service", Name: "discord", DisplayName: "Discord", UpstreamURL: "https://discord.invalid",
		HostPattern: "discord.invalid", AuthType: "header", AuthHeader: "Authorization", AuthPrefix: "Bot ", DeliveryMode: "proxy"}); err != nil {
		t.Fatal(err)
	}
	clientModel := &models.Client{ID: "vertical-client", ShortID: "VERT01", Name: "vertical client", TokenHash: "unused", IsActive: true}
	if err := queries.NewClientQueries(db).Create(clientModel); err != nil {
		t.Fatal(err)
	}
	crypto := services.NewCrypto(make([]byte, 32))
	encryptedToken, err := crypto.Encrypt("fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	apiKeys := queries.NewAPIKeyQueries(db)
	if err := apiKeys.Create(&models.APIKey{ID: "vertical-key", ServiceID: "discord-service", Name: "fixture bot", KeyEncrypted: encryptedToken}); err != nil {
		t.Fatal(err)
	}
	ccQueries := queries.NewControlChannelQueries(db)
	policy := `{"guild_id":"vertical-guild","category_id":"vertical-category","enabled":true}`
	if err := ccQueries.Create(&models.ControlChannel{ID: "vertical-cc", Name: "vertical", ServiceID: "discord-service", APIKeyID: "vertical-key",
		ClientID: clientModel.ID, AgentType: "fixture", PlaceholderID: "unused", Config: policy, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if err := ccQueries.CreateChannel(&models.CCChannel{Handle: "dwch_vertical_task", CCID: "vertical-cc", ClientID: &clientModel.ID,
		ChannelID: "vertical-discord-channel", Name: "vertical-task", Kind: "task", SessionID: session.SessionID, Cwd: configDir}); err != nil {
		t.Fatal(err)
	}

	var discordMu sync.Mutex
	var discordMessages []map[string]any
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot fixture-token" {
			t.Errorf("Discord authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, "[]")
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			discordMu.Lock()
			discordMessages = append(discordMessages, body)
			discordMu.Unlock()
			_, _ = io.WriteString(w, `{"id":"1888888888888888888"}`)
		default:
			http.Error(w, "unexpected Discord method", http.StatusMethodNotAllowed)
		}
	}))
	defer discord.Close()
	bot := &services.DiscordBot{BaseURL: discord.URL, HTTP: discord.Client()}
	clientHandler := handlers.NewCCClientHandler(ccQueries, apiKeys, crypto, bot, services.NewCCEventHub(), services.NewCCApprovalRegistry())
	withClient := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			next(w, r.WithContext(context.WithValue(r.Context(), middleware.ClientKey, clientModel)))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /client/cc/inbox/claim", withClient(clientHandler.ClaimInbox))
	mux.HandleFunc("POST /client/cc/inbox/{inbox_id}/finish", withClient(clientHandler.FinishInbox))
	mux.HandleFunc("POST /client/cc/channels/{handle}/messages", withClient(clientHandler.PostMessage))
	apiServer := httptest.NewServer(mux)
	defer apiServer.Close()

	messageID := "1777777777777777777"
	payload, _ := json.Marshal(map[string]any{"id": messageID, "guild_id": "vertical-guild", "channel_id": "vertical-discord-channel",
		"content": "must remain rejected", "author": map[string]any{"id": "vertical-user", "bot": false}})
	services.RouteDiscordGatewayEvent("vertical-key", "fixture-token", "vertical-bot", ccQueries, services.NewCCEventHub(), nil, nil, "MESSAGE_CREATE", payload)
	// A replayed Gateway dispatch with the same Discord snowflake must resolve
	// to the existing row before the client claims it.
	services.RouteDiscordGatewayEvent("vertical-key", "fixture-token", "vertical-bot", ccQueries, services.NewCCEventHub(), nil, nil, "MESSAGE_CREATE", payload)

	watch, err := NewCCWatch(configDir, &Config{ServerURL: apiServer.URL, Token: "fixture-client-token"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := watch.api.ClaimCCInbox(context.Background(), 120)
	if err != nil || claimed == nil || claimed.EventKey != "MESSAGE_CREATE:"+messageID || claimed.ClaimToken == "" || claimed.AttemptCount != 1 {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	handle := ""
	if claimed.ChannelHandle != nil {
		handle = *claimed.ChannelHandle
	}
	envelope, _ := json.Marshal(sseEnvelope{Type: "message_create", CCID: claimed.CCID, Handle: handle, Kind: "task", Payload: json.RawMessage(claimed.Payload),
		InboxID: claimed.ID, SessionID: claimed.SessionID, ClaimToken: claimed.ClaimToken, AttemptCount: claimed.AttemptCount})
	watch.handleMessageCreate(envelope)

	rows, err := ccQueries.PullInbox("vertical-cc", 0, []string{"dwch_vertical_task"}, 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "completed" || rows[0].LastError != "not owner" {
		t.Fatalf("persisted inbox rows=%+v err=%v", rows, err)
	}
	if next, err := watch.api.ClaimCCInbox(context.Background(), 120); err != nil || next != nil {
		t.Fatalf("completed inbox reclaimed: next=%+v err=%v", next, err)
	}
	discordMu.Lock()
	posted := append([]map[string]any(nil), discordMessages...)
	discordMu.Unlock()
	if len(posted) != 1 || !strings.Contains(posted[0]["content"].(string), "controlled by `terminal:vertical-terminal`") {
		t.Fatalf("Discord rejection=%+v", posted)
	}
	reference, _ := posted[0]["message_reference"].(map[string]any)
	if reference["message_id"] != messageID {
		t.Fatalf("Discord origin reference=%+v", reference)
	}
	cc, err := duckliondaemon.DialCC(ducklion.SocketPath(), "dwch_vertical_task")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := cc.AgentTaskEvents(context.Background(), session.SessionID, messageID, 0); err == nil {
		t.Fatal("ownership-rejected vertical prompt reached Ducklion")
	}
}
