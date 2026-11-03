package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TelegramPoller uses long-polling getUpdates to receive callback_query events
// from inline keyboard buttons. No public webhook needed.
type TelegramPoller struct {
	botToken  string
	chatID    string
	onApprove func(approvalID string) error
	onReject  func(approvalID string) error
	client    *http.Client
	offset    int
	stopCh    chan struct{}
	stopOnce  sync.Once

	cancelMu sync.Mutex
	cancel   context.CancelFunc // cancels the in-flight long-poll HTTP request; guarded by cancelMu
}

func NewTelegramPoller(botToken, chatID string, onApprove, onReject func(string) error) *TelegramPoller {
	return &TelegramPoller{
		botToken:  botToken,
		chatID:    chatID,
		onApprove: onApprove,
		onReject:  onReject,
		client:    &http.Client{Timeout: 35 * time.Second},
		stopCh:    make(chan struct{}),
	}
}

func (p *TelegramPoller) Start() {
	go p.pollLoop()
	log.Printf("[telegram-poll] Started polling for chat %s", p.chatID)
}

// Stop cancels the in-flight long-poll request immediately and exits the loop.
func (p *TelegramPoller) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.cancelMu.Lock()
		cancel := p.cancel
		p.cancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}

func (p *TelegramPoller) pollLoop() {
	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		updates, err := p.getUpdates()
		if err != nil {
			log.Printf("[telegram-poll] Error: %v", scrubNetErr(err))
			time.Sleep(5 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= p.offset {
				p.offset = u.UpdateID + 1
			}

			if u.CallbackQuery != nil {
				p.handleCallback(u.CallbackQuery)
			}
		}
	}
}

type tgUpdate struct {
	UpdateID      int              `json:"update_id"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgCallbackQuery struct {
	ID   string `json:"id"`
	Data string `json:"data"`
	From struct {
		Username string `json:"username"`
	} `json:"from"`
	Message struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (p *TelegramPoller) handleCallback(cb *tgCallbackQuery) {
	// Answer exactly once (Telegram accepts a single answer per callback): empty
	// on success, a short reason when the decision could not be applied.
	reply := ""
	defer func() { p.answerCallback(cb.ID, reply) }()

	data := cb.Data
	if strings.HasPrefix(data, "approve:") {
		approvalID := strings.TrimPrefix(data, "approve:")
		if err := p.onApprove(approvalID); err != nil {
			reply = decisionFailure(err)
			return
		}
		p.editMessage(cb.Message.Chat.ID, cb.Message.MessageID,
			fmt.Sprintf("✅ Approved `%s` for 24h by @%s", approvalID, cb.From.Username))
	} else if strings.HasPrefix(data, "reject:") {
		approvalID := strings.TrimPrefix(data, "reject:")
		if err := p.onReject(approvalID); err != nil {
			reply = decisionFailure(err)
			return
		}
		p.editMessage(cb.Message.Chat.ID, cb.Message.MessageID,
			fmt.Sprintf("🚫 Rejected `%s` by @%s", approvalID, cb.From.Username))
	}
}

// decisionFailure is the short reply for a button press that did not change
// the approval: "Already decided" when it was decided before, else the error.
func decisionFailure(err error) string {
	if strings.Contains(err.Error(), "already decided") {
		return "Already decided"
	}
	return "Failed: " + err.Error()
}

func (p *TelegramPoller) getUpdates() ([]tgUpdate, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30&allowed_updates=[\"callback_query\"]",
		p.botToken, p.offset)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // always release the context goroutine
	p.cancelMu.Lock()
	p.cancel = cancel
	p.cancelMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, scrubNetErr(fmt.Errorf("build request: %w", err))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, scrubNetErr(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	json.Unmarshal(body, &result)
	return result.Result, nil
}

func (p *TelegramPoller) answerCallback(callbackID, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", p.botToken)
	body, _ := json.Marshal(map[string]string{
		"callback_query_id": callbackID,
		"text":              text,
	})
	if resp, err := p.client.Post(url, "application/json", strings.NewReader(string(body))); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	} else {
		log.Printf("[telegram-poll] answerCallbackQuery failed: %v", scrubNetErr(err))
	}
}

func (p *TelegramPoller) editMessage(chatID int64, messageID int, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", p.botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	if resp, err := p.client.Post(url, "application/json", strings.NewReader(string(body))); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	} else {
		log.Printf("[telegram-poll] editMessageText failed: %v", scrubNetErr(err))
	}
}
