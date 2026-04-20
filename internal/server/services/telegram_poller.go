package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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

func (p *TelegramPoller) Stop() {
	close(p.stopCh)
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
			log.Printf("[telegram-poll] Error: %v", err)
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
	ID      string `json:"id"`
	Data    string `json:"data"`
	From    struct {
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
	// Answer the callback to remove the loading spinner
	p.answerCallback(cb.ID, "")

	data := cb.Data
	if strings.HasPrefix(data, "approve:") {
		approvalID := strings.TrimPrefix(data, "approve:")
		if err := p.onApprove(approvalID); err != nil {
			p.answerCallback(cb.ID, "Failed: "+err.Error())
			return
		}
		p.editMessage(cb.Message.Chat.ID, cb.Message.MessageID,
			fmt.Sprintf("✅ Approved `%s` for 24h by @%s", approvalID, cb.From.Username))
	} else if strings.HasPrefix(data, "reject:") {
		approvalID := strings.TrimPrefix(data, "reject:")
		if err := p.onReject(approvalID); err != nil {
			return
		}
		p.editMessage(cb.Message.Chat.ID, cb.Message.MessageID,
			fmt.Sprintf("🚫 Rejected `%s` by @%s", approvalID, cb.From.Username))
	}
}

func (p *TelegramPoller) getUpdates() ([]tgUpdate, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30&allowed_updates=[\"callback_query\"]",
		p.botToken, p.offset)

	resp, err := p.client.Get(url)
	if err != nil {
		return nil, err
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
	http.Post(url, "application/json", strings.NewReader(string(body)))
}

func (p *TelegramPoller) editMessage(chatID int64, messageID int, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", p.botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	http.Post(url, "application/json", strings.NewReader(string(body)))
}
