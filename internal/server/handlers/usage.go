package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/services"
)

// UsageHandler serves the LLM API-key usage view: per-key rate-limit
// snapshots (tokens/requests used vs. remaining, reset times) captured
// from upstream Anthropic / OpenAI responses on each proxied call, plus
// per-client ("session") request-volume aggregates from the request log.
type UsageHandler struct {
	apiKeys    *queries.APIKeyQueries
	requestLog *queries.RequestLogQueries
	convUsage  *queries.ConversationUsageQueries
}

func NewUsageHandler(apiKeys *queries.APIKeyQueries, requestLog *queries.RequestLogQueries, convUsage *queries.ConversationUsageQueries) *UsageHandler {
	return &UsageHandler{apiKeys: apiKeys, requestLog: requestLog, convUsage: convUsage}
}

// parseHours pulls a non-negative ?hours=N off the query string,
// defaulting to 0 (= all time) for missing or malformed values.
func parseHours(r *http.Request) int {
	v := strings.TrimSpace(r.URL.Query().Get("hours"))
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// usageMetricRow is one rate-limited dimension flattened for the UI.
type usageMetricRow struct {
	Name      string  `json:"name"` // requests, tokens, input_tokens, output_tokens
	Limit     int64   `json:"limit"`
	Remaining int64   `json:"remaining"`
	Used      int64   `json:"used"`     // limit - remaining (>=0)
	UsedPct   float64 `json:"used_pct"` // 0..100, -1 when limit unknown
	Reset     string  `json:"reset"`    // RFC3339 or ""
}

// usageRow is one API key's usage view.
type usageRow struct {
	KeyID         string            `json:"key_id"`
	KeyName       string            `json:"key_name"`
	Service       string            `json:"service"`
	Provider      string            `json:"provider"`
	IsRefreshable bool              `json:"is_refreshable"`
	UsageCount    int64             `json:"usage_count"`
	LastUsedAt    string            `json:"last_used_at"`
	UpdatedAt     string            `json:"updated_at"` // from snapshot, "" if none
	HasData       bool              `json:"has_data"`   // a snapshot was recorded
	Metrics       []usageMetricRow  `json:"metrics"`
	Subscription  map[string]string `json:"subscription,omitempty"` // Claude Max 5h/7d windows

	// Token totals parsed from response bodies over the requested window
	// (all-time by default). Distinct from the rate-limit snapshot above:
	// these are cumulative captured usage, not the provider's remaining
	// allowance. Zero when no conversation_usage rows exist for the key.
	TokensInput         int64 `json:"tokens_input"`
	TokensOutput        int64 `json:"tokens_output"`
	TokensCacheRead     int64 `json:"tokens_cache_read"`
	TokensCacheCreation int64 `json:"tokens_cache_creation"`
	CapturedRequests    int64 `json:"captured_requests"`
	Conversations       int64 `json:"conversations"`
}

type clientKeyUsageView struct {
	APIKeyID            string  `json:"api_key_id"`
	KeyName             string  `json:"key_name"`
	Service             string  `json:"service"`
	Provider            string  `json:"provider"`
	IsRefreshable       bool    `json:"is_refreshable"`
	Requests            int64   `json:"requests"`
	TokensInput         int64   `json:"tokens_input"`
	TokensOutput        int64   `json:"tokens_output"`
	TokensCacheRead     int64   `json:"tokens_cache_read"`
	TokensCacheCreation int64   `json:"tokens_cache_creation"`
	TotalTokens         int64   `json:"total_tokens"`
	Conversations       int64   `json:"conversations"`
	LastSeen            string  `json:"last_seen"`
	MaxUsedPct          float64 `json:"max_used_pct"`
	ResetAt             string  `json:"reset_at"`
}

type clientUsageView struct {
	ClientID            string               `json:"client_id"`
	ClientName          string               `json:"client_name"`
	Requests            int64                `json:"requests"`
	TokensInput         int64                `json:"tokens_input"`
	TokensOutput        int64                `json:"tokens_output"`
	TokensCacheRead     int64                `json:"tokens_cache_read"`
	TokensCacheCreation int64                `json:"tokens_cache_creation"`
	TotalTokens         int64                `json:"total_tokens"`
	Conversations       int64                `json:"conversations"`
	KeysUsed            int                  `json:"keys_used"`
	Services            []string             `json:"services"`
	LastSeen            string               `json:"last_seen"`
	MaxKeyUsedPct       float64              `json:"max_key_used_pct"`
	Status              string               `json:"status"`
	Keys                []clientKeyUsageView `json:"keys"`
}

// List handles GET /api/usage. Returns one row per API key whose
// service is an LLM provider (anthropic/openai) OR that has a recorded
// usage snapshot. Keys with no data yet are still listed (HasData=false)
// so the operator can see they exist but haven't been exercised.
func (h *UsageHandler) List(w http.ResponseWriter, r *http.Request) {
	keys, err := h.apiKeys.List("")
	if err != nil {
		JsonErrorPublic(w, "failed to list keys", http.StatusInternalServerError)
		return
	}

	// Token rollups keyed by api_key_id over the requested window.
	tokenTotals := map[string]queries.KeyTokenTotals{}
	if h.convUsage != nil {
		if t, terr := h.convUsage.TotalsByKey(parseHours(r)); terr == nil {
			tokenTotals = t
		}
	}

	rows := []usageRow{}
	for _, k := range keys {
		row := usageRow{
			KeyID:         k.ID,
			KeyName:       k.Name,
			Service:       k.ServiceName,
			IsRefreshable: k.IsRefreshable,
			UsageCount:    k.UsageCount,
			Metrics:       []usageMetricRow{},
		}
		if k.LastUsedAt != nil {
			row.LastUsedAt = *k.LastUsedAt
		}

		var snap services.UsageSnapshot
		if k.UsageSnapshot != "" {
			if jerr := json.Unmarshal([]byte(k.UsageSnapshot), &snap); jerr == nil {
				row.HasData = len(snap.Metrics) > 0 || len(snap.Subscription) > 0
				row.Provider = snap.Provider
				row.UpdatedAt = snap.UpdatedAt
				row.Subscription = snap.Subscription
				for name, m := range snap.Metrics {
					mr := usageMetricRow{
						Name:      name,
						Limit:     m.Limit,
						Remaining: m.Remaining,
						Reset:     m.Reset,
						UsedPct:   -1,
					}
					if m.Limit > 0 {
						used := m.Limit - m.Remaining
						if used < 0 {
							used = 0
						}
						mr.Used = used
						mr.UsedPct = float64(used) / float64(m.Limit) * 100
					}
					row.Metrics = append(row.Metrics, mr)
				}
				// Stable metric ordering for the UI.
				sort.Slice(row.Metrics, func(i, j int) bool {
					return row.Metrics[i].Name < row.Metrics[j].Name
				})
			}
		}

		// Decorate with captured token totals.
		if tt, ok := tokenTotals[k.ID]; ok {
			row.TokensInput = tt.InputTokens
			row.TokensOutput = tt.OutputTokens
			row.TokensCacheRead = tt.CacheReadTokens
			row.TokensCacheCreation = tt.CacheCreationTokens
			row.CapturedRequests = tt.Requests
			row.Conversations = tt.Conversations
			if tt.Requests > 0 {
				row.HasData = true
			}
		}

		// Include LLM-provider keys, keys with a rate-limit snapshot, or
		// keys with captured token data.
		if row.HasData || isLLMService(row.Service) {
			rows = append(rows, row)
		}
	}

	// Keys with live data first, then by service+name for stability.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].HasData != rows[j].HasData {
			return rows[i].HasData
		}
		if rows[i].Service != rows[j].Service {
			return rows[i].Service < rows[j].Service
		}
		return rows[i].KeyName < rows[j].KeyName
	})

	JsonResponsePublic(w, rows)
}

func (h *UsageHandler) Clients(w http.ResponseWriter, r *http.Request) {
	if h.convUsage == nil {
		JsonResponsePublic(w, []clientUsageView{})
		return
	}

	days := parseDays(r)
	rows, err := h.convUsage.ClientKeyUsage(days)
	if err != nil {
		JsonErrorPublic(w, "failed to aggregate client usage", http.StatusInternalServerError)
		return
	}

	keyMeta := h.usageMetaByKey()
	byClient := map[string]*clientUsageView{}
	for _, row := range rows {
		clientID := row.ClientID
		if clientID == "" {
			clientID = "(unknown)"
		}
		c := byClient[clientID]
		if c == nil {
			c = &clientUsageView{
				ClientID:      clientID,
				ClientName:    row.ClientName,
				MaxKeyUsedPct: -1,
				Status:        "normal",
			}
			if c.ClientName == "" {
				c.ClientName = clientID
			}
			byClient[clientID] = c
		}

		total := row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheCreationTokens
		meta := keyMeta[row.APIKeyID]
		k := clientKeyUsageView{
			APIKeyID:            row.APIKeyID,
			KeyName:             row.KeyName,
			Service:             row.ServiceName,
			Provider:            meta.provider,
			IsRefreshable:       row.IsRefreshable,
			Requests:            row.Requests,
			TokensInput:         row.InputTokens,
			TokensOutput:        row.OutputTokens,
			TokensCacheRead:     row.CacheReadTokens,
			TokensCacheCreation: row.CacheCreationTokens,
			TotalTokens:         total,
			Conversations:       row.Conversations,
			LastSeen:            row.LastSeen,
			MaxUsedPct:          meta.maxUsedPct,
			ResetAt:             meta.resetAt,
		}
		c.Keys = append(c.Keys, k)
		c.Requests += row.Requests
		c.TokensInput += row.InputTokens
		c.TokensOutput += row.OutputTokens
		c.TokensCacheRead += row.CacheReadTokens
		c.TokensCacheCreation += row.CacheCreationTokens
		c.TotalTokens += total
		c.Conversations += row.Conversations
		if row.LastSeen > c.LastSeen {
			c.LastSeen = row.LastSeen
		}
		if k.MaxUsedPct > c.MaxKeyUsedPct {
			c.MaxKeyUsedPct = k.MaxUsedPct
		}
	}

	out := make([]clientUsageView, 0, len(byClient))
	for _, c := range byClient {
		serviceSeen := map[string]bool{}
		for _, k := range c.Keys {
			if k.Service != "" && !serviceSeen[k.Service] {
				serviceSeen[k.Service] = true
				c.Services = append(c.Services, k.Service)
			}
		}
		sort.Strings(c.Services)
		c.KeysUsed = len(c.Keys)
		sort.SliceStable(c.Keys, func(i, j int) bool {
			return c.Keys[i].TotalTokens > c.Keys[j].TotalTokens
		})
		switch {
		case c.MaxKeyUsedPct >= 90:
			c.Status = "near shared key limit"
		case c.MaxKeyUsedPct >= 75:
			c.Status = "high shared key usage"
		case c.TotalTokens == 0:
			c.Status = "no token data"
		default:
			c.Status = "normal"
		}
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].TotalTokens > out[j].TotalTokens
	})
	JsonResponsePublic(w, out)
}

type usageKeyMeta struct {
	provider   string
	maxUsedPct float64
	resetAt    string
}

func (h *UsageHandler) usageMetaByKey() map[string]usageKeyMeta {
	meta := map[string]usageKeyMeta{}
	keys, err := h.apiKeys.List("")
	if err != nil {
		return meta
	}
	for _, k := range keys {
		m := usageKeyMeta{maxUsedPct: -1}
		if k.UsageSnapshot != "" {
			var snap services.UsageSnapshot
			if err := json.Unmarshal([]byte(k.UsageSnapshot), &snap); err == nil {
				m.provider = snap.Provider
				for _, metric := range snap.Metrics {
					if metric.Limit <= 0 {
						continue
					}
					used := metric.Limit - metric.Remaining
					if used < 0 {
						used = 0
					}
					pct := float64(used) / float64(metric.Limit) * 100
					if pct > m.maxUsedPct {
						m.maxUsedPct = pct
						m.resetAt = metric.Reset
					}
				}
			}
		}
		meta[k.ID] = m
	}
	return meta
}

// Sessions handles GET /api/usage/sessions[?hours=N]. Returns per-client,
// per-service request-volume aggregates from the request log. request_log
// has no token data, so this is a call-count view (total + errors + last
// seen) — the per-key panel covers token/rate-limit usage.
func (h *UsageHandler) Sessions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.requestLog.SessionUsage(parseHours(r))
	if err != nil {
		JsonErrorPublic(w, "failed to aggregate session usage", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []queries.SessionUsageRow{}
	}
	JsonResponsePublic(w, rows)
}

// Conversations handles GET /api/usage/conversations?key_id=X[&hours=N].
// Per-conversation token breakdown for one API key — the drill-down
// shown when an operator clicks a key on the usage panel. Each row is
// one claude session (X-Claude-Code-Session-Id), enriched with the CC
// channel name when the session is bound to one. OpenAI / non-claude
// traffic collapses into a single empty-conversation row.
func (h *UsageHandler) Conversations(w http.ResponseWriter, r *http.Request) {
	keyID := strings.TrimSpace(r.URL.Query().Get("key_id"))
	if keyID == "" {
		JsonErrorPublic(w, "key_id required", http.StatusBadRequest)
		return
	}
	if h.convUsage == nil {
		JsonResponsePublic(w, []queries.ConversationUsageRow{})
		return
	}
	rows, err := h.convUsage.ByKey(keyID, parseHours(r))
	if err != nil {
		JsonErrorPublic(w, "failed to aggregate conversation usage", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []queries.ConversationUsageRow{}
	}
	JsonResponsePublic(w, rows)
}

// isLLMService reports whether a service name is a known LLM provider
// whose keys are worth showing on the usage panel even before any
// snapshot has been captured.
func isLLMService(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "anthropic", "openai", "codex", "claude", "xai", "xai-grok":
		return true
	}
	return false
}

func parseDays(r *http.Request) int {
	switch strings.TrimSpace(r.URL.Query().Get("days")) {
	case "3":
		return 3
	case "7":
		return 7
	case "30":
		return 30
	default:
		return 3
	}
}
