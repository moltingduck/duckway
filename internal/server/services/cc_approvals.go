package services

import (
	"sync"
)

// CCApprovalRegistry tracks in-flight reaction-based approval votes for
// the discord_request_approval MCP tool. The flow:
//
//  1. Client POSTs an approval; server posts a question message with
//     pre-added emoji reactions, then Register()s the message_id with
//     a result channel.
//  2. Server blocks on the channel up to timeout_seconds.
//  3. The CC gateway sees MESSAGE_REACTION_ADD, looks up the message_id
//     here, and Resolve()s with the chosen option.
//  4. Whoever's waiting receives the result, the registry drops the entry.
//
// One in-flight approval per message_id is enough — Discord lets multiple
// users react but we record the first one to react with a tracked emoji.
type CCApprovalRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

type pendingApproval struct {
	options          map[string]string // emoji → option label
	requiredReactors map[string]bool   // empty = anyone
	result           chan ApprovalResult
}

// ApprovalResult is what the server returns to the MCP tool.
type ApprovalResult struct {
	Chosen        string `json:"chosen"`
	Emoji         string `json:"emoji"`
	ReactorUserID string `json:"reactor_user_id"`
	TimedOut      bool   `json:"timed_out"`
	MessageID     string `json:"message_id"`
}

func NewCCApprovalRegistry() *CCApprovalRegistry {
	return &CCApprovalRegistry{pending: map[string]*pendingApproval{}}
}

// Register sets up tracking for one approval. options maps emoji
// (e.g. "✅") to the human label ("approve"). requiredReactors, if
// non-empty, restricts which Discord user_ids may decide.
//
// Returns a channel the caller should select on alongside a timeout.
func (r *CCApprovalRegistry) Register(messageID string, options map[string]string, requiredReactors []string) <-chan ApprovalResult {
	required := map[string]bool{}
	for _, u := range requiredReactors {
		required[u] = true
	}
	p := &pendingApproval{
		options:          options,
		requiredReactors: required,
		result:           make(chan ApprovalResult, 1),
	}
	r.mu.Lock()
	r.pending[messageID] = p
	r.mu.Unlock()
	return p.result
}

// Cancel removes an approval without resolving (used on timeout).
func (r *CCApprovalRegistry) Cancel(messageID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, messageID)
}

// Resolve is called from the gateway when a reaction lands. Returns
// false if the approval is unknown, the emoji isn't tracked, or the
// reactor isn't allowed.
func (r *CCApprovalRegistry) Resolve(messageID, emoji, reactorUserID string) bool {
	r.mu.Lock()
	p, ok := r.pending[messageID]
	if !ok {
		r.mu.Unlock()
		return false
	}
	chosen, isTracked := p.options[emoji]
	if !isTracked {
		r.mu.Unlock()
		return false
	}
	if len(p.requiredReactors) > 0 && !p.requiredReactors[reactorUserID] {
		r.mu.Unlock()
		return false
	}
	delete(r.pending, messageID)
	r.mu.Unlock()

	// Buffered chan; non-blocking send.
	select {
	case p.result <- ApprovalResult{
		Chosen:        chosen,
		Emoji:         emoji,
		ReactorUserID: reactorUserID,
		MessageID:     messageID,
	}:
	default:
	}
	return true
}

// Pending is a diagnostic helper for tests.
func (r *CCApprovalRegistry) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// DefaultEmojiForOption maps the option index (0-based) to the emoji
// the bot pre-adds and the user clicks. Index 0/1 default to ✅/❌ for
// 2-option (yes/no) approvals; indices 2+ use numbered keycaps.
func DefaultEmojiForOption(i, total int) string {
	if total == 2 {
		switch i {
		case 0:
			return "✅"
		case 1:
			return "❌"
		}
	}
	switch i {
	case 0:
		return "1️⃣"
	case 1:
		return "2️⃣"
	case 2:
		return "3️⃣"
	case 3:
		return "4️⃣"
	case 4:
		return "5️⃣"
	case 5:
		return "6️⃣"
	case 6:
		return "7️⃣"
	case 7:
		return "8️⃣"
	case 8:
		return "9️⃣"
	case 9:
		return "🔟"
	}
	return "❓"
}
