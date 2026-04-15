package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

type Notifier struct {
	channels *queries.NotificationQueries
	client   *http.Client
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
		"🔑 *Duckway Approval Required*\n\nClient: `%s`\nService: `%s`\nRequest: `%s %s`\n\n[Approve in Admin Panel](%s)",
		notif.ClientName, notif.ServiceName, notif.Method, notif.Path, notif.AdminURL,
	)

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.BotToken)
	body, _ := json.Marshal(map[string]string{
		"chat_id":    cfg.ChatID,
		"text":       text,
		"parse_mode": "Markdown",
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
		"title":       "Duckway Approval Required",
		"color":       16750848, // Orange
		"description": fmt.Sprintf("**Client:** `%s`\n**Service:** `%s`\n**Request:** `%s %s`", notif.ClientName, notif.ServiceName, notif.Method, notif.Path),
		"footer":      map[string]string{"text": "Approve at " + notif.AdminURL},
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
