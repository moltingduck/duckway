package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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
	decide    DecisionFunc // when set, handles decisions with the actor (allowlist enforced by the guard)
	base      string       // Telegram API base URL (tests override)
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
		base:      telegramAPIBase(),
		client:    &http.Client{Timeout: 35 * time.Second},
		stopCh:    make(chan struct{}),
	}
}

// WithDecider routes every button press through fn (which receives the actor
// and enforces the approver allowlist) instead of the plain callbacks.
func (p *TelegramPoller) WithDecider(fn DecisionFunc) *TelegramPoller {
	p.decide = fn
	return p
}

func telegramAPIBase() string {
	if b := os.Getenv("DUCKWAY_TELEGRAM_BASE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://api.telegram.org"
}

// decideAs applies a decision through the wired DecisionFunc only. With none
// wired it fails closed: the plain onApprove/onReject callbacks carry no actor
// and would bypass the approver allowlist.
func (p *TelegramPoller) decideAs(approvalID string, approve bool, minutes int, actor ChatActor) error {
	if p.decide == nil {
		return ErrChannelNotAuthoritative
	}
	return p.decide(approvalID, approve, minutes, actor)
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
		ID       int64  `json:"id"`
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
	var approve bool
	var minutes int
	var approvalID string
	switch {
	case strings.HasPrefix(data, "approve1h:"):
		approve, minutes, approvalID = true, 60, strings.TrimPrefix(data, "approve1h:")
	case strings.HasPrefix(data, "approve:"):
		approve, approvalID = true, strings.TrimPrefix(data, "approve:")
	case strings.HasPrefix(data, "reject:"):
		approvalID = strings.TrimPrefix(data, "reject:")
	default:
		return
	}
	actor := ChatActor{Kind: "telegram", UserID: strconv.FormatInt(cb.From.ID, 10), Username: cb.From.Username}
	if err := p.decideAs(approvalID, approve, minutes, actor); err != nil {
		if errors.Is(err, ErrNotApprover) {
			reply = "Not an approver"
			return
		}
		reply = decisionFailure(err)
		return
	}
	who := "@" + cb.From.Username
	if cb.From.Username == "" {
		who = "user " + actor.UserID
	}
	verb := "🚫 Rejected"
	if approve {
		verb = "✅ Approved"
		if minutes > 0 {
			verb += " for " + fmtMinutes(minutes)
		}
	}
	p.editMessage(cb.Message.Chat.ID, cb.Message.MessageID,
		fmt.Sprintf("%s `%s` by %s at %s UTC", verb, approvalID, who, time.Now().UTC().Format("2006-01-02 15:04")))
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
	url := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=30&allowed_updates=[\"callback_query\"]",
		p.base, p.botToken, p.offset)

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
	url := fmt.Sprintf("%s/bot%s/answerCallbackQuery", p.base, p.botToken)
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
	url := fmt.Sprintf("%s/bot%s/editMessageText", p.base, p.botToken)
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
