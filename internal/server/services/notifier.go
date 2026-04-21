package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"net/url"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

type Notifier struct {
	channels *queries.NotificationQueries
	client   *http.Client
	// Discord gateways by channel ID — used for reaction-based approvals
	Gateways sync.Map // channelID → *DiscordGateway
}

func NewNotifier(channels *queries.NotificationQueries) *Notifier {
	return &Notifier{
		channels: channels,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

type ApprovalNotification struct {
	ApprovalID    string `json:"approval_id"`
	PlaceholderID string `json:"placeholder_id"`
	ClientName    string `json:"client_name"`
	ServiceName   string `json:"service_name"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	AdminURL      string `json:"admin_url"`
}

// NotifyApprovalNeeded sends a notification to all active channels.
func (n *Notifier) NotifyApprovalNeeded(notif ApprovalNotification) {
	channels, err := n.channels.ListActive()
	if err != nil {
		log.Printf("Failed to list notification channels: %v", err)
		return
	}

	for _, ch := range channels {
		go func(ch queries.NotificationChannel) {
			var err error
			switch ch.ChannelType {
			case "telegram":
				err = n.sendTelegram(ch.Config, notif)
			case "discord":
				err = n.sendDiscord(ch.Config, notif)
			case "discord_bot":
				err = n.sendDiscordBot(ch.Config, notif)
			case "webhook":
				err = n.sendWebhook(ch.Config, notif)
			default:
				log.Printf("Unknown channel type: %s", ch.ChannelType)
				return
			}
			if err != nil {
				log.Printf("Notification error (%s/%s): %v", ch.ChannelType, ch.Name, err)
			}
		}(ch)
	}
}

// Telegram: config = {"bot_token": "...", "chat_id": "..."}
func (n *Notifier) sendTelegram(configJSON string, notif ApprovalNotification) error {
	var cfg struct {
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse telegram config: %w", err)
	}

	text := fmt.Sprintf(
		"🔑 *Duckway Approval Required*\n\nClient: `%s`\nService: `%s`\nRequest: `%s %s`\nApproval ID: `%s`",
		notif.ClientName, notif.ServiceName, notif.Method, notif.Path, notif.ApprovalID,
	)

	// Inline keyboard with Approve/Reject buttons
	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ Approve (24h)", "callback_data": "approve:" + notif.ApprovalID},
				{"text": "❌ Reject", "callback_data": "reject:" + notif.ApprovalID},
			},
		},
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.BotToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":      cfg.ChatID,
		"text":         text,
		"parse_mode":   "Markdown",
		"reply_markup": keyboard,
	})

	resp, err := n.client.Post(apiURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("telegram API returned %d", resp.StatusCode)
	}
	return nil
}

// Discord: config = {"webhook_url": "..."}
func (n *Notifier) sendDiscord(configJSON string, notif ApprovalNotification) error {
	var cfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse discord config: %w", err)
	}

	embed := map[string]interface{}{
		"title":       "🔑 Duckway Approval Required",
		"color":       16750848,
		"description": fmt.Sprintf("**Client:** `%s`\n**Service:** `%s`\n**Request:** `%s %s`\n**Approval ID:** `%s`", notif.ClientName, notif.ServiceName, notif.Method, notif.Path, notif.ApprovalID),
		"footer":      map[string]string{"text": "Reply: !approve " + notif.ApprovalID},
	}

	body, _ := json.Marshal(map[string]interface{}{
		"embeds": []interface{}{embed},
	})

	resp, err := n.client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord webhook returned %d", resp.StatusCode)
	}
	return nil
}

// Discord Bot: config = {"bot_token": "...", "channel_id": "..."}
// Uses the Gateway's SendApprovalMessage for reaction-based approval if available.
func (n *Notifier) sendDiscordBot(configJSON string, notif ApprovalNotification) error {
	var cfg struct {
		BotToken  string `json:"bot_token"`
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse discord bot config: %w", err)
	}

	// Use Gateway if running (reaction-based approval)
	if gw, ok := n.Gateways.Load(cfg.ChannelID); ok {
		gateway := gw.(*DiscordGateway)
		gateway.SendApprovalMessage(notif)
		return nil
	}

	// Fallback: send via REST API without reaction tracking
	embed := map[string]interface{}{
		"title":       "🔑 Duckway Approval Required",
		"color":       16750848,
		"description": fmt.Sprintf("**Client:** `%s`\n**Service:** `%s`\n**Request:** `%s %s`\n**Approval ID:** `%s`", notif.ClientName, notif.ServiceName, notif.Method, notif.Path, notif.ApprovalID),
		"footer":      map[string]string{"text": "Reply: !approve " + notif.ApprovalID + "  or  !reject " + notif.ApprovalID},
	}

	body, _ := json.Marshal(map[string]interface{}{
		"embeds": []interface{}{embed},
	})

	apiURL := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", cfg.ChannelID)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+cfg.BotToken)

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord bot API returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// TestChannel sends a test message to a specific channel and optionally
// verifies the round-trip (user can interact within a timeout).
// Returns: send_ok, receive_ok, error message
type TestResult struct {
	SendOK    bool   `json:"send_ok"`
	ReceiveOK bool   `json:"receive_ok"`
	Message   string `json:"message"`
}

func (n *Notifier) TestChannel(ch queries.NotificationChannel) TestResult {
	testCode := fmt.Sprintf("test-%d", time.Now().UnixMilli()%100000)

	switch ch.ChannelType {
	case "telegram":
		return n.testTelegram(ch.Config, testCode)
	case "discord_bot":
		return n.testDiscordBot(ch.Config, testCode)
	case "discord":
		return n.testDiscordWebhook(ch.Config)
	case "webhook":
		return n.testWebhook(ch.Config)
	default:
		return TestResult{Message: "unknown channel type: " + ch.ChannelType}
	}
}

func (n *Notifier) testTelegram(configJSON, testCode string) TestResult {
	var cfg struct {
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return TestResult{Message: "invalid config: " + err.Error()}
	}

	// Send test message with confirm button
	text := fmt.Sprintf("🧪 *Duckway Test*\nClick the button to verify this channel works.\nTest code: `%s`", testCode)
	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{{"text": "✅ Confirm", "callback_data": "test_confirm:" + testCode}},
		},
	}
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id": cfg.ChatID, "text": text, "parse_mode": "Markdown", "reply_markup": keyboard,
	})
	resp, err := n.client.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.BotToken),
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		return TestResult{Message: "send failed: " + err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return TestResult{Message: fmt.Sprintf("send failed: HTTP %d", resp.StatusCode)}
	}

	// Poll for button press (15s timeout)
	deadline := time.Now().Add(15 * time.Second)
	pollClient := &http.Client{Timeout: 10 * time.Second}
	offset := 0

	for time.Now().Before(deadline) {
		pollURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=5&allowed_updates=[\"callback_query\"]",
			cfg.BotToken, offset)
		pollResp, err := pollClient.Get(pollURL)
		if err != nil {
			continue
		}
		pollBody, _ := io.ReadAll(pollResp.Body)
		pollResp.Body.Close()

		var result struct {
			Result []struct {
				UpdateID      int `json:"update_id"`
				CallbackQuery *struct {
					ID   string `json:"id"`
					Data string `json:"data"`
				} `json:"callback_query"`
			} `json:"result"`
		}
		json.Unmarshal(pollBody, &result)

		for _, u := range result.Result {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil && u.CallbackQuery.Data == "test_confirm:"+testCode {
				// Answer callback
				answerBody, _ := json.Marshal(map[string]string{
					"callback_query_id": u.CallbackQuery.ID, "text": "Test confirmed!",
				})
				n.client.Post(
					fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", cfg.BotToken),
					"application/json", bytes.NewReader(answerBody),
				)
				return TestResult{SendOK: true, ReceiveOK: true, Message: "Send + receive confirmed"}
			}
		}
	}

	return TestResult{SendOK: true, ReceiveOK: false, Message: "Message sent, but no button press received within 15s. Click the button in the chat to verify."}
}

func (n *Notifier) testDiscordBot(configJSON, testCode string) TestResult {
	var cfg struct {
		BotToken  string `json:"bot_token"`
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return TestResult{Message: "invalid config: " + err.Error()}
	}

	// Send test message
	text := fmt.Sprintf("🧪 **Duckway Test**\nReply with `!confirm %s` to verify this channel works.", testCode)
	msgBody, _ := json.Marshal(map[string]string{"content": text})
	apiURL := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", cfg.ChannelID)
	req, _ := http.NewRequest("POST", apiURL, bytes.NewReader(msgBody))
	req.Header.Set("Authorization", "Bot "+cfg.BotToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return TestResult{Message: "send failed: " + err.Error()}
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return TestResult{Message: fmt.Sprintf("send failed: HTTP %d: %s", resp.StatusCode, string(respBody))}
	}

	// Poll channel messages for !confirm reply (15s timeout)
	deadline := time.Now().Add(15 * time.Second)
	var lastMsgID string
	// Get the sent message ID to only look at messages after it
	var sentMsg struct{ ID string `json:"id"` }
	json.Unmarshal(respBody, &sentMsg)
	lastMsgID = sentMsg.ID

	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		getURL := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages?after=%s&limit=10", cfg.ChannelID, lastMsgID)
		getReq, _ := http.NewRequest("GET", getURL, nil)
		getReq.Header.Set("Authorization", "Bot "+cfg.BotToken)
		getResp, err := n.client.Do(getReq)
		if err != nil {
			continue
		}
		getBody, _ := io.ReadAll(getResp.Body)
		getResp.Body.Close()

		var messages []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			Author  struct{ Bot bool `json:"bot"` } `json:"author"`
		}
		json.Unmarshal(getBody, &messages)

		for _, m := range messages {
			if m.Author.Bot {
				continue
			}
			if strings.TrimSpace(m.Content) == "!confirm "+testCode {
				// React to confirm
				reactURL := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages/%s/reactions/✅/@me", cfg.ChannelID, m.ID)
				reactReq, _ := http.NewRequest("PUT", reactURL, nil)
				reactReq.Header.Set("Authorization", "Bot "+cfg.BotToken)
				n.client.Do(reactReq)
				return TestResult{SendOK: true, ReceiveOK: true, Message: "Send + receive confirmed"}
			}
		}
	}

	return TestResult{SendOK: true, ReceiveOK: false, Message: "Message sent, but no !confirm reply received within 15s. Reply in the channel to verify."}
}

func (n *Notifier) testDiscordWebhook(configJSON string) TestResult {
	var cfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return TestResult{Message: "invalid config"}
	}
	body, _ := json.Marshal(map[string]string{"content": "🧪 Duckway test notification"})
	resp, err := n.client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return TestResult{Message: "send failed: " + err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return TestResult{Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return TestResult{SendOK: true, ReceiveOK: true, Message: "Webhook sent OK (no receive test for webhooks)"}
}

func (n *Notifier) testWebhook(configJSON string) TestResult {
	var cfg struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return TestResult{Message: "invalid config"}
	}
	body, _ := json.Marshal(map[string]string{"test": "duckway"})
	resp, err := n.client.Post(cfg.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return TestResult{Message: "send failed: " + err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return TestResult{Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return TestResult{SendOK: true, ReceiveOK: true, Message: "Webhook OK"}
}

// Webhook: config = {"url": "...", "secret": "..."}
func (n *Notifier) sendWebhook(configJSON string, notif ApprovalNotification) error {
	var cfg struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse webhook config: %w", err)
	}

	body, _ := json.Marshal(notif)

	parsedURL, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("invalid webhook URL: %w", err)
	}

	req, err := http.NewRequest("POST", parsedURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Secret != "" {
		req.Header.Set("X-Duckway-Secret", cfg.Secret)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}
