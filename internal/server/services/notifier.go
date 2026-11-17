package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

type Notifier struct {
	channels *queries.NotificationQueries
	client   *http.Client
	// Discord gateways by channel ID — used for reaction-based approvals
	Gateways sync.Map // channelID → *DiscordGateway
	// Telegram held-request messages by approval ID, so a decision made
	// elsewhere can still update them (best effort, in-memory, this process only,
	// entries expire after 48 h).
	tgMessages ttlMap[[]tgMessageRef]

	// lookup reads an approval's current decision (decision-before-store check).
	lookup DecisionLookup

	// DecisionMade work runs on a small bounded worker pool, not one goroutine per decision.
	decisionQ   chan decisionJob
	workersOnce sync.Once
	stopCh      chan struct{}
	stopOnce    sync.Once
}

// decisionJob is one queued chat-message update.
type decisionJob struct {
	approvalID, decidedBy, via, status string
	notBefore                          time.Time
}

const (
	decisionQueueSize = 256
	decisionWorkers   = 2
	// chatDecisionGrace lets the chat surface that made the decision finish
	// editing its own message (and drop its ref) before the observer runs, so
	// only the other messages are touched.
	chatDecisionGrace = 500 * time.Millisecond
)

// WithDecisionLookup enables the decision-before-store check for Telegram messages.
func (n *Notifier) WithDecisionLookup(fn DecisionLookup) *Notifier {
	n.lookup = fn
	return n
}

// Stop ends the decision workers.
func (n *Notifier) Stop() { n.stopOnce.Do(func() { close(n.stopCh) }) }

type tgMessageRef struct {
	botToken string
	chatID   string
	msgID    int64
	at       time.Time
}

func NewNotifier(channels *queries.NotificationQueries) *Notifier {
	return &Notifier{
		channels:  channels,
		client:    &http.Client{Timeout: 10 * time.Second},
		decisionQ: make(chan decisionJob, decisionQueueSize),
		stopCh:    make(chan struct{}),
	}
}

type ApprovalNotification struct {
	ApprovalID    string `json:"approval_id"`
	PlaceholderID string `json:"placeholder_id"`
	ClientName    string `json:"client_name"`
	ServiceName   string `json:"service_name"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Rule          string `json:"rule,omitempty"`      // approve rule (and layer) that held the request; empty for token-wide approval
	Query         string `json:"query,omitempty"`     // raw query string of the held request
	BodyHash      string `json:"body_hash,omitempty"` // first 12 hex characters of the body's SHA-256
	AdminURL      string `json:"admin_url"`
}

// requestTarget is the path with its query string: what exactly is being approved.
func (n ApprovalNotification) requestTarget() string {
	t := n.Path
	if n.Query != "" {
		t += "?" + n.Query
	}
	// The target is placed inside markdown code spans: a backtick would close
	// the span and let request-controlled text inject formatting.
	return strings.ReplaceAll(t, "`", "'")
}

// mdSafe returns a copy whose plain fields are safe inside markdown code spans
// (a backtick would close the span). Used for the Telegram and Discord texts
// only; the webhook payload keeps the raw values.
func (n ApprovalNotification) mdSafe() ApprovalNotification {
	r := func(s string) string { return strings.ReplaceAll(s, "`", "'") }
	n.ClientName, n.ServiceName, n.Method, n.ApprovalID = r(n.ClientName), r(n.ServiceName), r(n.Method), r(n.ApprovalID)
	n.Path, n.Query = r(n.Path), r(n.Query)
	return n
}

// bodyLine shows the short body hash an approval is bound to (markdown).
func bodyLine(hash string) string {
	if hash == "" {
		return ""
	}
	return "\n**Body hash:** `" + strings.ReplaceAll(hash, "`", "'") + "`"
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
	notif = notif.mdSafe()
	var cfg struct {
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse telegram config: %w", err)
	}

	text := fmt.Sprintf(
		"🔑 *Duckway Approval Required*\n\nClient: `%s`\nService: `%s`\nRequest: `%s %s`\nApproval ID: `%s`",
		notif.ClientName, notif.ServiceName, notif.Method, notif.requestTarget(), notif.ApprovalID,
	)
	if notif.BodyHash != "" {
		text += fmt.Sprintf("\nBody hash: `%s`", strings.ReplaceAll(notif.BodyHash, "`", "'"))
	}
	if notif.Rule != "" {
		text += fmt.Sprintf("\nRule: `%s`", strings.ReplaceAll(notif.Rule, "`", "'"))
	}

	// Inline keyboard with Approve/Reject buttons
	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ Approve", "callback_data": "approve:" + notif.ApprovalID},
				{"text": "✅ Approve for 1 h", "callback_data": "approve1h:" + notif.ApprovalID},
				{"text": "❌ Reject", "callback_data": "reject:" + notif.ApprovalID},
			},
		},
	}

	apiURL := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBase(), cfg.BotToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":      cfg.ChatID,
		"text":         text,
		"parse_mode":   "Markdown",
		"reply_markup": keyboard,
	})

	resp, err := n.client.Post(apiURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return scrubNetErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("telegram API returned %d", resp.StatusCode)
	}
	var sent struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)); json.Unmarshal(b, &sent) == nil && sent.Result.MessageID != 0 {
		ref := tgMessageRef{cfg.BotToken, cfg.ChatID, sent.Result.MessageID, time.Now()}
		n.tgMessages.Update(notif.ApprovalID, func(old []tgMessageRef, _ bool) []tgMessageRef { return append(old, ref) })
		// A decision may have landed while the message was in flight: its observer
		// found no ref. Check after storing and edit now.
		if n.lookup != nil {
			if dec, err := n.lookup(notif.ApprovalID); err == nil && dec.Status != "" && dec.Status != "pending" {
				n.removeTelegramRef(notif.ApprovalID, ref.chatID, ref.msgID)
				n.editTelegramRef(ref, notif.ApprovalID, describeDecision(dec.Status, dec.DecidedBy, 0, time.Now()))
			}
		}
	}
	return nil
}

// Discord: config = {"webhook_url": "..."}
func (n *Notifier) sendDiscord(configJSON string, notif ApprovalNotification) error {
	notif = notif.mdSafe()
	var cfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse discord config: %w", err)
	}

	embed := map[string]interface{}{
		"title":       "🔑 Duckway Approval Required",
		"color":       16750848,
		"description": fmt.Sprintf("**Client:** `%s`\n**Service:** `%s`\n**Request:** `%s %s`\n**Approval ID:** `%s`", notif.ClientName, notif.ServiceName, notif.Method, notif.requestTarget(), notif.ApprovalID) + ruleLine(notif.Rule) + bodyLine(notif.BodyHash),
		"footer":      map[string]string{"text": "Reply: !approve " + notif.ApprovalID},
	}

	body, _ := json.Marshal(map[string]interface{}{
		"embeds": []interface{}{embed},
	})

	resp, err := n.client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return scrubNetErr(err)
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
	notif = notif.mdSafe()
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
		"description": fmt.Sprintf("**Client:** `%s`\n**Service:** `%s`\n**Request:** `%s %s`\n**Approval ID:** `%s`", notif.ClientName, notif.ServiceName, notif.Method, notif.requestTarget(), notif.ApprovalID) + ruleLine(notif.Rule) + bodyLine(notif.BodyHash),
		"footer":      map[string]string{"text": "No listener is running for this channel: decide in the Duckway console or ducklord."},
	}

	// No buttons: nothing listens for them without a gateway.
	body, _ := json.Marshal(map[string]interface{}{
		"embeds": []interface{}{embed},
	})

	apiURL := fmt.Sprintf("%s/channels/%s/messages", discordAPIBase(), cfg.ChannelID)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return scrubNetErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+cfg.BotToken)

	resp, err := n.client.Do(req)
	if err != nil {
		return scrubNetErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord bot API returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// DecisionMade updates the held-request messages of an approval that was
// decided somewhere (web console, TUI, expiry, or a chat). It is a DecisionHook
// body: best effort, never blocks the decider (it only enqueues), and only
// touches messages this process sent. The chat surface that decided has already
// edited its own message and dropped its ref, so nothing is skipped by via.
func (n *Notifier) DecisionMade(approvalID, decidedBy, via, status string) {
	n.workersOnce.Do(func() {
		for i := 0; i < decisionWorkers; i++ {
			go n.decisionWorker()
		}
	})
	job := decisionJob{approvalID: approvalID, decidedBy: decidedBy, via: via, status: status}
	if via == ViaDiscord || via == ViaTelegram {
		job.notBefore = time.Now().Add(chatDecisionGrace)
	}
	select {
	case n.decisionQ <- job:
	default:
		log.Printf("[notifier] decision queue full; chat message for approval %s not updated", approvalID)
	}
}

func (n *Notifier) decisionWorker() {
	for {
		select {
		case <-n.stopCh:
			return
		case job := <-n.decisionQ:
			if d := time.Until(job.notBefore); d > 0 {
				select {
				case <-time.After(d):
				case <-n.stopCh:
					return
				}
			}
			n.applyDecision(job)
		}
	}
}

func (n *Notifier) applyDecision(job decisionJob) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("[notifier] decision update panic: %v", p)
		}
	}()
	line := describeDecision(job.status, job.decidedBy, 0, time.Now())
	n.Gateways.Range(func(_, v any) bool {
		if gw, ok := v.(*DiscordGateway); ok {
			gw.MarkDecided(job.approvalID, "**"+line+"**")
		}
		return true
	})
	refs, ok := n.tgMessages.LoadAndDelete(job.approvalID)
	if !ok {
		return
	}
	for _, r := range refs {
		n.editTelegramRef(r, job.approvalID, line)
	}
}

// removeTelegramRef forgets one message ref (it was already edited).
func (n *Notifier) removeTelegramRef(approvalID, chatID string, msgID int64) {
	n.tgMessages.Update(approvalID, func(old []tgMessageRef, _ bool) []tgMessageRef {
		var keep []tgMessageRef
		for _, r := range old {
			if r.chatID != chatID || r.msgID != msgID {
				keep = append(keep, r)
			}
		}
		return keep
	})
}

// TelegramMessageDone tells the notifier that the poller already edited the
// message it received a button press on, so the decision observer skips it.
func (n *Notifier) TelegramMessageDone(approvalID string, chatID int64, msgID int) {
	n.removeTelegramRef(approvalID, strconv.FormatInt(chatID, 10), int64(msgID))
}

// editTelegramRef rewrites a held-request message with the result line,
// honouring one 429 retry_after.
func (n *Notifier) editTelegramRef(r tgMessageRef, approvalID, line string) {
	body, _ := json.Marshal(map[string]interface{}{"chat_id": r.chatID, "message_id": r.msgID,
		"text": "Duckway approval " + approvalID + ": " + line})
	url := fmt.Sprintf("%s/bot%s/editMessageText", telegramAPIBase(), r.botToken)
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := n.client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("[notifier] telegram result edit failed: %v", scrubNetErr(err))
			return
		}
		wait := time.Duration(0)
		if resp.StatusCode == http.StatusTooManyRequests {
			var v struct {
				Parameters struct {
					RetryAfter float64 `json:"retry_after"`
				} `json:"parameters"`
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = json.Unmarshal(b, &v)
			wait = capRetryAfter(v.Parameters.RetryAfter)
		} else {
			io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
		if wait <= 0 {
			return
		}
		select {
		case <-time.After(wait):
		case <-n.stopCh:
			return
		}
	}
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
		return TestResult{Message: "send failed: " + scrubNetErr(err).Error()}
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
				if r, err := n.client.Post(
					fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", cfg.BotToken),
					"application/json", bytes.NewReader(answerBody),
				); err == nil {
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
				}
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
		return TestResult{Message: "send failed: " + scrubNetErr(err).Error()}
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
	var sentMsg struct {
		ID string `json:"id"`
	}
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
			Author  struct {
				Bot bool `json:"bot"`
			} `json:"author"`
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
				if r, err := n.client.Do(reactReq); err == nil {
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
				}
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
		return TestResult{Message: "send failed: " + scrubNetErr(err).Error()}
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return TestResult{Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return TestResult{SendOK: true, ReceiveOK: true, Message: "Webhook sent OK (no receive test for webhooks)"}
}

func (n *Notifier) testWebhook(configJSON string) TestResult {
	body, _ := json.Marshal(map[string]string{"test": "duckway"})
	if err := n.postWebhook(configJSON, body); err != nil {
		return TestResult{Message: err.Error()}
	}
	return TestResult{SendOK: true, ReceiveOK: true, Message: "Webhook OK"}
}

// Webhook: config = {"url": "...", "secret": "..."}
func (n *Notifier) sendWebhook(configJSON string, notif ApprovalNotification) error {
	body, _ := json.Marshal(notif)
	return n.postWebhook(configJSON, body)
}

// postWebhook is the single webhook send path, shared by real notifications and
// the admin Test button so the Test exercises exactly what real sends do,
// including the X-Duckway-Secret header when a secret is configured.
func (n *Notifier) postWebhook(configJSON string, body []byte) error {
	var cfg struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse webhook config: %w", err)
	}

	parsedURL, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("invalid webhook URL: %w", err)
	}

	req, err := http.NewRequest("POST", parsedURL.String(), bytes.NewReader(body))
	if err != nil {
		return scrubNetErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Secret != "" {
		req.Header.Set("X-Duckway-Secret", cfg.Secret)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("send failed: %w", scrubNetErr(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

var (
	telegramTokenInPath = regexp.MustCompile(`/bot[^/\s"?]+`)
	urlInText           = regexp.MustCompile(`https?://[^\s"']+`)
)

// scrubNetErr strips a *url.Error down to its inner error (the wrapper echoes
// the full request URL, which carries Telegram bot tokens and webhook
// secrets) and redacts any URL that still appears in the message.
func scrubNetErr(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = fmt.Errorf("%s request failed: %w", ue.Op, ue.Err)
	}
	msg := telegramTokenInPath.ReplaceAllString(err.Error(), "/bot********")
	msg = urlInText.ReplaceAllString(msg, "[url]")
	return errors.New(msg)
}

// ErrChannelNotAuthoritative is returned when an approve/reject arrives from a
// listener whose notification channel is now disabled, deleted or edited.
var ErrChannelNotAuthoritative = errors.New("this notification channel is disabled or was changed; approvals from it are refused")

// sameConfig compares two config JSON documents semantically, so a
// formatting-only change (whitespace, key order) does not count as an edit.
//
// The approver allowlist keys are ignored: they are read live from the database
// on every decision (see GuardChannelDecision), so editing who may approve
// takes effect at once and does not need a listener restart.
func sameConfig(a, b string) bool {
	if a == b {
		return true
	}
	var x, y map[string]interface{}
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	for _, m := range []map[string]interface{}{x, y} {
		delete(m, ApproverUsersKey)
		delete(m, ApproverRolesKey)
	}
	return reflect.DeepEqual(x, y)
}

// guardChannelAction wraps an approve/reject callback so it only runs while
// the channel it came from is still active and its stored config still equals
// the one the listener started with. The DB is consulted on every call and
// nothing is cached, so a disabled or edited channel loses the ability to
// approve immediately, even though its listener runs until the next restart.
func guardChannelAction(q *queries.NotificationQueries, channelID, startedConfig string, fn func(string) error) func(string) error {
	return func(approvalID string) error {
		cur, err := q.GetByID(channelID)
		if err != nil || !cur.IsActive || !sameConfig(cur.Config, startedConfig) {
			return ErrChannelNotAuthoritative
		}
		return fn(approvalID)
	}
}

// GuardChannelDecision is guardChannelAction for decisions that carry the
// actor: besides the active/unchanged-config checks it requires the actor to be
// on the channel's approver list, read from the database on every call. An
// empty list refuses everyone (ErrNotApprover).
func GuardChannelDecision(q *queries.NotificationQueries, channelID, startedConfig string, fn DecisionFunc) DecisionFunc {
	return func(approvalID string, approve bool, minutes int, actor ChatActor) error {
		cur, err := q.GetByID(channelID)
		if err != nil || !cur.IsActive || !sameConfig(cur.Config, startedConfig) {
			return ErrChannelNotAuthoritative
		}
		if !ParseApprovers(cur.Config).Allows(actor) {
			return ErrNotApprover
		}
		return fn(approvalID, approve, minutes, actor)
	}
}

// ruleLine is the extra notification line naming the approve rule that held a
// request ("" for token-wide approvals).
func ruleLine(rule string) string {
	if rule == "" {
		return ""
	}
	return "\n**Rule:** `" + strings.ReplaceAll(rule, "`", "'") + "`"
}

// SameChannelConfig reports whether two channel configs are equal apart from
// the approver allowlist (which is read live and needs no listener restart).
func SameChannelConfig(a, b string) bool { return sameConfig(a, b) }
