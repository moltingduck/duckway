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

type meteredUsageTotals struct {
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	BillableTokens      int64   `json:"billable_tokens"`
	CostUSDMicros       int64   `json:"cost_usd_micros"`
	PricedRequests      int64   `json:"priced_requests"`
	UnpricedRequests    int64   `json:"unpriced_requests"`
	TotalTokens         int64   `json:"total_tokens"`
	CacheTokens         int64   `json:"cache_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	Clients             int     `json:"clients,omitempty"`
}

type meteredClientUsage struct {
	ClientID   string              `json:"client_id"`
	ClientName string              `json:"client_name"`
	Summary    meteredUsageTotals  `json:"summary"`
	Models     []meteredModelUsage `json:"models"`
	Daily      []meteredDailyUsage `json:"daily"`
}

type meteredModelUsage struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	meteredUsageTotals
}

type meteredDailyUsage struct {
	Date string `json:"date"`
	meteredUsageTotals
}

type meteredUsageDetailResponse struct {
	WindowDays int                  `json:"window_days"`
	Summary    meteredUsageTotals   `json:"summary"`
	Clients    []meteredClientUsage `json:"clients"`
	Models     []meteredModelUsage  `json:"models"`
	Daily      []meteredDailyUsage  `json:"daily"`
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

// KeyDetail returns daily metered usage at the client+model grain for one key.
func (h *UsageHandler) KeyDetail(w http.ResponseWriter, r *http.Request) {
	if h.convUsage == nil {
		JsonResponsePublic(w, []queries.MeteredUsageDetailRow{})
		return
	}
	keyID := strings.TrimSpace(r.PathValue("id"))
	if keyID == "" {
		JsonErrorPublic(w, "key id required", http.StatusBadRequest)
		return
	}
	if _, err := h.apiKeys.GetByID(keyID); err != nil {
		JsonErrorPublic(w, "key not found", http.StatusNotFound)
		return
	}
	rows, err := h.convUsage.DetailByKey(keyID, parseUsageDetailDays(r))
	if err != nil {
		JsonErrorPublic(w, "failed to aggregate key usage", http.StatusInternalServerError)
		return
	}
	JsonResponsePublic(w, rows)
}

// KeyGroupDetail returns usage for the key group's current member set.
func (h *UsageHandler) KeyGroupDetail(w http.ResponseWriter, r *http.Request) {
	if h.convUsage == nil {
		JsonResponsePublic(w, []queries.MeteredUsageDetailRow{})
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		JsonErrorPublic(w, "key group id required", http.StatusBadRequest)
		return
	}
	rows, err := h.convUsage.DetailByKeyGroup(groupID, parseUsageDetailDays(r))
	if err != nil {
		JsonErrorPublic(w, "failed to aggregate key group usage", http.StatusInternalServerError)
		return
	}
	JsonResponsePublic(w, rows)
}

// Detail returns the usage/cost hierarchy consumed by the usage UI. Exactly
// one of key_id or key_group_id selects the root of the hierarchy.
func (h *UsageHandler) Detail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	keyID := strings.TrimSpace(r.URL.Query().Get("key_id"))
	groupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
	if groupID == "" {
		groupID = strings.TrimSpace(r.URL.Query().Get("key_group_id"))
	}
	if (keyID == "") == (groupID == "") {
		JsonErrorPublic(w, "exactly one of key_id or key_group_id is required", http.StatusBadRequest)
		return
	}
	days := parseUsageDetailDays(r)
	response := meteredUsageDetailResponse{
		WindowDays: days,
		Clients:    []meteredClientUsage{},
		Models:     []meteredModelUsage{},
		Daily:      []meteredDailyUsage{},
	}
	if h.convUsage == nil {
		JsonResponsePublic(w, response)
		return
	}
	var rows []queries.MeteredUsageDetailRow
	var err error
	if keyID != "" {
		if _, getErr := h.apiKeys.GetByID(keyID); getErr != nil {
			JsonErrorPublic(w, "key not found", http.StatusNotFound)
			return
		}
		rows, err = h.convUsage.DetailByKey(keyID, days)
	} else {
		rows, err = h.convUsage.DetailByKeyGroup(groupID, days)
	}
	if err != nil {
		JsonErrorPublic(w, "failed to aggregate usage detail", http.StatusInternalServerError)
		return
	}
	response = buildMeteredUsageDetail(days, rows)
	JsonResponsePublic(w, response)
}

func buildMeteredUsageDetail(days int, rows []queries.MeteredUsageDetailRow) meteredUsageDetailResponse {
	response := meteredUsageDetailResponse{WindowDays: days, Clients: []meteredClientUsage{}, Models: []meteredModelUsage{}, Daily: []meteredDailyUsage{}}
	type clientAccumulator struct {
		view   *meteredClientUsage
		models map[string]*meteredModelUsage
		daily  map[string]*meteredDailyUsage
	}
	clients := map[string]*clientAccumulator{}
	modelsByName := map[string]*meteredModelUsage{}
	daily := map[string]*meteredDailyUsage{}
	for _, row := range rows {
		totals := meteredUsageTotals{
			Requests: row.Requests, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			CacheReadTokens: row.CacheReadTokens, CacheCreationTokens: row.CacheCreationTokens,
			BillableTokens: row.BillableTokens, CostUSDMicros: row.CostUSDMicros,
			PricedRequests: row.PricedRequests, UnpricedRequests: row.Requests - row.PricedRequests,
		}
		addMeteredTotals(&response.Summary, totals)
		clientKey := row.ClientID
		if clientKey == "" {
			clientKey = "(unknown)"
		}
		if clients[clientKey] == nil {
			name := row.ClientName
			if name == "" {
				name = clientKey
			}
			clients[clientKey] = &clientAccumulator{
				view:   &meteredClientUsage{ClientID: clientKey, ClientName: name, Models: []meteredModelUsage{}, Daily: []meteredDailyUsage{}},
				models: map[string]*meteredModelUsage{}, daily: map[string]*meteredDailyUsage{},
			}
		}
		client := clients[clientKey]
		addMeteredTotals(&client.view.Summary, totals)
		modelKey := row.Provider + "\x00" + row.Model
		if modelsByName[modelKey] == nil {
			modelsByName[modelKey] = &meteredModelUsage{Provider: row.Provider, Model: row.Model}
		}
		addMeteredTotals(&modelsByName[modelKey].meteredUsageTotals, totals)
		if client.models[modelKey] == nil {
			client.models[modelKey] = &meteredModelUsage{Provider: row.Provider, Model: row.Model}
		}
		addMeteredTotals(&client.models[modelKey].meteredUsageTotals, totals)
		if daily[row.Day] == nil {
			daily[row.Day] = &meteredDailyUsage{Date: row.Day}
		}
		addMeteredTotals(&daily[row.Day].meteredUsageTotals, totals)
		if client.daily[row.Day] == nil {
			client.daily[row.Day] = &meteredDailyUsage{Date: row.Day}
		}
		addMeteredTotals(&client.daily[row.Day].meteredUsageTotals, totals)
	}
	response.Summary.Clients = len(clients)
	for _, client := range clients {
		finalizeMeteredTotals(&client.view.Summary)
		for _, row := range client.models {
			finalizeMeteredTotals(&row.meteredUsageTotals)
			client.view.Models = append(client.view.Models, *row)
		}
		for _, row := range client.daily {
			finalizeMeteredTotals(&row.meteredUsageTotals)
			client.view.Daily = append(client.view.Daily, *row)
		}
		sort.Slice(client.view.Models, func(i, j int) bool { return client.view.Models[i].TotalTokens > client.view.Models[j].TotalTokens })
		sort.Slice(client.view.Daily, func(i, j int) bool { return client.view.Daily[i].Date < client.view.Daily[j].Date })
		response.Clients = append(response.Clients, *client.view)
	}
	for _, row := range modelsByName {
		finalizeMeteredTotals(&row.meteredUsageTotals)
		response.Models = append(response.Models, *row)
	}
	for _, row := range daily {
		finalizeMeteredTotals(&row.meteredUsageTotals)
		response.Daily = append(response.Daily, *row)
	}
	finalizeMeteredTotals(&response.Summary)
	sort.Slice(response.Clients, func(i, j int) bool {
		return response.Clients[i].Summary.CostUSDMicros > response.Clients[j].Summary.CostUSDMicros
	})
	sort.Slice(response.Models, func(i, j int) bool { return response.Models[i].CostUSDMicros > response.Models[j].CostUSDMicros })
	sort.Slice(response.Daily, func(i, j int) bool { return response.Daily[i].Date < response.Daily[j].Date })
	return response
}

func finalizeMeteredTotals(t *meteredUsageTotals) {
	t.TotalTokens = t.BillableTokens
	t.CacheTokens = t.CacheReadTokens + t.CacheCreationTokens
	t.CostUSD = float64(t.CostUSDMicros) / 1_000_000
}

func addMeteredTotals(dst *meteredUsageTotals, src meteredUsageTotals) {
	dst.Requests += src.Requests
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.BillableTokens += src.BillableTokens
	dst.CostUSDMicros += src.CostUSDMicros
	dst.PricedRequests += src.PricedRequests
	dst.UnpricedRequests += src.UnpricedRequests
}

// isLLMService reports whether a service name is a known LLM provider
// whose keys are worth showing on the usage panel even before any
// snapshot has been captured.
func isLLMService(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "anthropic", "openai", "openai-chatgpt", "codex", "claude", "xai", "xai-grok":
		return true
	}
	return false
}

func parseDays(r *http.Request) int {
	switch strings.TrimSpace(r.URL.Query().Get("days")) {
	case "1":
		return 1
	case "3":
		return 3
	case "7":
		return 7
	case "30":
		return 30
	case "90":
		return 90
	default:
		return 3
	}
}

func parseUsageDetailDays(r *http.Request) int {
	return parseDays(r)
}
