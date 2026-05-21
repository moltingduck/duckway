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
}

func NewUsageHandler(apiKeys *queries.APIKeyQueries, requestLog *queries.RequestLogQueries) *UsageHandler {
	return &UsageHandler{apiKeys: apiKeys, requestLog: requestLog}
}

// usageMetricRow is one rate-limited dimension flattened for the UI.
type usageMetricRow struct {
	Name      string  `json:"name"`       // requests, tokens, input_tokens, output_tokens
	Limit     int64   `json:"limit"`
	Remaining int64   `json:"remaining"`
	Used      int64   `json:"used"`       // limit - remaining (>=0)
	UsedPct   float64 `json:"used_pct"`   // 0..100, -1 when limit unknown
	Reset     string  `json:"reset"`      // RFC3339 or ""
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
	UpdatedAt     string            `json:"updated_at"`            // from snapshot, "" if none
	HasData       bool              `json:"has_data"`             // a snapshot was recorded
	Metrics       []usageMetricRow  `json:"metrics"`
	Subscription  map[string]string `json:"subscription,omitempty"` // Claude Max 5h/7d windows
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

		// Only include LLM-provider keys (or any key that has data).
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

// Sessions handles GET /api/usage/sessions[?hours=N]. Returns per-client,
// per-service request-volume aggregates from the request log. request_log
// has no token data, so this is a call-count view (total + errors + last
// seen) — the per-key panel covers token/rate-limit usage.
func (h *UsageHandler) Sessions(w http.ResponseWriter, r *http.Request) {
	hours := 0 // 0 = all time
	if v := strings.TrimSpace(r.URL.Query().Get("hours")); v != "" {
		// Best-effort parse; ignore garbage and fall back to all-time.
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		hours = n
	}
	rows, err := h.requestLog.SessionUsage(hours)
	if err != nil {
		JsonErrorPublic(w, "failed to aggregate session usage", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []queries.SessionUsageRow{}
	}
	JsonResponsePublic(w, rows)
}

// isLLMService reports whether a service name is a known LLM provider
// whose keys are worth showing on the usage panel even before any
// snapshot has been captured.
func isLLMService(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "anthropic", "openai", "codex", "claude":
		return true
	}
	return false
}
