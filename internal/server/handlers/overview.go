package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
)

// OverviewHandler serves GET /api/overview (admin-authenticated by the route
// group): the "Needs attention" queue, stat-card counters and security posture
// signals. Responses carry names and counts only, never secrets.
type OverviewHandler struct {
	q        *queries.OverviewQueries
	settings *queries.SettingsQueries
	now      func() time.Time

	mu       sync.Mutex
	cached   *OverviewResponse
	cachedAt time.Time
	cacheTTL time.Duration
}

func NewOverviewHandler(q *queries.OverviewQueries, settings *queries.SettingsQueries) *OverviewHandler {
	return &OverviewHandler{q: q, settings: settings, now: time.Now, cacheTTL: 3 * time.Second}
}

const (
	overviewStaleAfter      = 24 * time.Hour
	overviewOnlineWindow    = 5 * time.Minute
	overviewExpiryHorizon   = 7 * 24 * time.Hour
	overviewRefreshGrace    = 15 * time.Minute
	overviewMaxNames        = 3
	overviewDefaultApproval = 60
)

// OverviewItem is one row of the attention queue. Title and Meta are plain
// text (they may contain client/service names; consumers must escape).
type OverviewItem struct {
	Kind     string   `json:"kind"`     // approvals|credentials|clients|rollout|canary
	Label    string   `json:"label"`    // text for the kind pill
	Severity string   `json:"severity"` // critical|warn|info
	Title    string   `json:"title"`
	Meta     string   `json:"meta"`
	Count    int      `json:"count"`
	Names    []string `json:"names"`
	Href     string   `json:"href"`
	CTA      string   `json:"cta"`
}

type OverviewPosture struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	State  string `json:"state"` // ok|warn|info
	Status string `json:"status"`
	Detail string `json:"detail"`
	Href   string `json:"href,omitempty"`
}

type OverviewResponse struct {
	GeneratedAt string `json:"generated_at"`
	Attention   struct {
		Count int            `json:"count"`
		Items []OverviewItem `json:"items"`
	} `json:"attention"`
	Stats struct {
		Clients struct {
			Online int `json:"online"`
			Total  int `json:"total"`
		} `json:"clients"`
		Credentials struct {
			Total    int `json:"total"`
			Expiring int `json:"expiring"`
		} `json:"credentials"`
		PhantomTokens int `json:"phantom_tokens"`
		Requests24h   int `json:"requests_24h"`
	} `json:"stats"`
	Posture []OverviewPosture `json:"posture"`
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func joinNames(names []string, total int) string {
	s := strings.Join(names, ", ")
	if total > len(names) {
		s += fmt.Sprintf(" and %d more", total-len(names))
	}
	return s
}

func humanDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%d h %02d min", h, int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func (h *OverviewHandler) approvalTTL() time.Duration {
	n, err := strconv.Atoi(h.settings.Get(queries.SettingApprovalRequestTTL))
	if err != nil || n <= 0 {
		n = overviewDefaultApproval
	}
	return time.Duration(n) * time.Minute
}

// Get serves the overview, reusing a computed response for overviewCacheTTL so
// many open dashboards (or SSE-triggered refreshes) do not repeat the queries.
func (h *OverviewHandler) Get(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	wall := time.Now()
	if h.cached != nil && h.cacheTTL > 0 && wall.Sub(h.cachedAt) < h.cacheTTL {
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, h.cached)
		return
	}
	resp, err := h.build()
	if err != nil {
		log.Printf("[overview] %v", err)
		jsonError(w, "failed to load overview", http.StatusInternalServerError)
		return
	}
	h.cached, h.cachedAt = &resp, wall
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, resp)
}

func (h *OverviewHandler) build() (OverviewResponse, error) {
	now := h.now().UTC()
	var resp OverviewResponse
	resp.GeneratedAt = now.Format(time.RFC3339)
	items := []OverviewItem{}

	pend, err := h.q.Pending()
	if err != nil {
		return resp, err
	}
	if pend.Count > 0 {
		it := OverviewItem{Kind: "approvals", Label: "Approval", Severity: "critical", Count: pend.Count,
			Title: fmt.Sprintf("%d %s waiting for a decision", pend.Count, plural(pend.Count, "approval", "approvals")),
			Href:  "/admin/approvals", CTA: "Review", Names: []string{}}
		meta := "Oldest"
		if pend.OldestClient != "" || pend.OldestService != "" {
			meta += ": " + pend.OldestClient + " → " + pend.OldestService
		}
		if created, ok := queries.ParseStoredTime(pend.OldestCreated); ok {
			left := created.Add(h.approvalTTL()).Sub(now)
			if left > 0 {
				meta += " · expires in " + humanDur(left)
			} else {
				meta += " · expiry due"
			}
		}
		it.Meta = meta
		items = append(items, it)
	}

	creds, err := h.q.Credentials(now, overviewExpiryHorizon, overviewRefreshGrace, overviewMaxNames)
	if err != nil {
		return resp, err
	}
	addCred := func(n queries.OverviewNames, sev, title, meta, href, cta string) {
		if n.Count == 0 {
			return
		}
		items = append(items, OverviewItem{Kind: "credentials", Label: "Credential", Severity: sev, Count: n.Count,
			Title: fmt.Sprintf(title, n.Count, plural(n.Count, "credential", "credentials")),
			Meta:  meta + joinNames(n.Names, n.Count), Names: n.Names, Href: href, CTA: cta})
	}
	addCred(creds.Deactivated, "warn", "%d refreshable %s disabled", "Not refreshing: ", "/admin/oauth", "Open tokens")
	addCred(creds.RefreshOverdue, "warn", "%d refreshable %s not renewing", "Access token expired: ", "/admin/oauth", "Open tokens")
	addCred(creds.Expiring, "warn", "%d %s expiring within 7 days or expired", "", "/admin/keys", "Open keys")

	stale, err := h.q.StaleClients(now.Add(-overviewStaleAfter), overviewMaxNames)
	if err != nil {
		return resp, err
	}
	if stale.Count > 0 {
		items = append(items, OverviewItem{Kind: "clients", Label: "Client", Severity: "warn", Count: stale.Count,
			Title: fmt.Sprintf("%d %s not seen for over 24 hours", stale.Count, plural(stale.Count, "client", "clients")),
			Meta:  joinNames(stale.Names, stale.Count), Names: stale.Names, Href: "/admin/clients", CTA: "View clients"})
	}

	rolls, err := h.q.AttentionRollouts()
	if err != nil {
		return resp, err
	}
	for _, ro := range rolls {
		var parts []string
		if ro.Status == "paused" {
			parts = append(parts, "paused")
		}
		if ro.Failed > 0 {
			parts = append(parts, fmt.Sprintf("%d of %d jobs failed", ro.Failed, ro.Total))
		}
		sev := "warn"
		if ro.Failed > 0 {
			sev = "critical"
		}
		items = append(items, OverviewItem{Kind: "rollout", Label: "Rollout", Severity: sev, Count: 1,
			Title: "Client rollout to " + ro.TargetVersion + " needs attention",
			Meta:  strings.Join(parts, " · "), Names: []string{ro.TargetVersion}, Href: "/admin/client-updates", CTA: "Open rollout"})
	}

	stats, err := h.q.Stats(now, overviewOnlineWindow)
	if err != nil {
		return resp, err
	}
	if stats.CanaryTokens > 0 && strings.TrimSpace(stats.CanaryAlertTo) == "" {
		items = append(items, OverviewItem{Kind: "canary", Label: "Canary", Severity: "warn", Count: stats.CanaryTokens,
			Title: fmt.Sprintf("%d canary %s deployed but no alert recipient is set", stats.CanaryTokens, plural(stats.CanaryTokens, "token", "tokens")),
			Meta:  "A tripped canary would not notify anyone.", Names: []string{}, Href: "/admin/canary", CTA: "Set recipient"})
	}

	resp.Attention.Items = items
	// Count queue rows, not underlying objects, so the pill matches what is listed.
	resp.Attention.Count = len(items)
	resp.Stats.Clients.Online = stats.ClientsOnline
	resp.Stats.Clients.Total = stats.ClientsTotal
	resp.Stats.Credentials.Total = creds.Total
	resp.Stats.Credentials.Expiring = creds.Expiring.Count + creds.RefreshOverdue.Count + creds.Deactivated.Count
	resp.Stats.PhantomTokens = stats.PhantomTokens
	resp.Stats.Requests24h = stats.Requests24h

	posture := []OverviewPosture{}
	if middleware.EnforceCSPEnabled() {
		posture = append(posture, OverviewPosture{Key: "csp", Label: "Content Security Policy", State: "ok", Status: "Enforced", Detail: "Admin pages block inline and third-party scripts."})
	} else {
		posture = append(posture, OverviewPosture{Key: "csp", Label: "Content Security Policy", State: "warn", Status: "Report only", Detail: "Violations are reported but not blocked."})
	}
	if h.settings.Get(queries.SettingRequestLogCaptureOn) == "1" {
		posture = append(posture, OverviewPosture{Key: "capture", Label: "Request body capture", State: "warn", Status: "On", Detail: "Request and response bodies are stored for the request log.", Href: "/admin/logs"})
	} else {
		posture = append(posture, OverviewPosture{Key: "capture", Label: "Request body capture", State: "ok", Status: "Off", Detail: "Bodies are not stored.", Href: "/admin/logs"})
	}
	switch {
	case stats.CanaryTokens == 0:
		posture = append(posture, OverviewPosture{Key: "canary", Label: "Canary alerts", State: "info", Status: "No canaries", Detail: "No canary tokens are deployed.", Href: "/admin/canary"})
	case strings.TrimSpace(stats.CanaryAlertTo) == "":
		posture = append(posture, OverviewPosture{Key: "canary", Label: "Canary alerts", State: "warn", Status: "No recipient", Detail: "Canaries are deployed but alerts have nowhere to go.", Href: "/admin/canary"})
	default:
		posture = append(posture, OverviewPosture{Key: "canary", Label: "Canary alerts", State: "ok", Status: "Configured", Detail: "Alerts are sent to the configured recipient.", Href: "/admin/canary"})
	}
	resp.Posture = posture

	return resp, nil
}
