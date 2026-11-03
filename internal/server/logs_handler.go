package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
)

const (
	logsLegacyLimit  = 500
	logsDefaultLimit = 50
	logsMaxLimit     = 200
	logsMaxTextLen   = 200
	logsTimeLayout   = "2006-01-02 15:04:05"
)

var logsRanges = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

var logsAllowedParams = map[string]bool{
	"range": true, "since": true, "until": true, "q": true, "client": true,
	"service": true, "credential": true, "status": true, "held": true,
	"acl_denied": true, "decision": true, "before_id": true, "after_id": true, "limit": true,
}

type logsListResponse struct {
	Items        []queries.RequestLogEntry `json:"items"`
	NextBeforeID *int64                    `json:"next_before_id"`
	Total        int                       `json:"total"`
	TotalCapped  bool                      `json:"total_capped"`
	Offset       int                       `json:"offset"`
	HasNewer     bool                      `json:"has_newer"`
	Limit        int                       `json:"limit"`
	// Window names the time window applied: "15m".."7d" for range, "custom"
	// for since/until, "7d" when a text/held/credential search had no bound
	// and was defaulted to 7 days, or "all" for no time bound.
	Window string `json:"window"`
}

// parseLogsFilter validates the query string strictly. The returned message is
// non-empty when the request must be rejected with 400.
func parseLogsFilterWindow(r *http.Request, now time.Time) (queries.RequestLogFilter, string, string) {
	f, window, msg := parseLogsFilterRaw(r, now)
	if msg != "" {
		return f, window, msg
	}
	// Any filter scans joined rows; without a lower time bound limit it to 7
	// days (before `until` when only that is given) so it cannot walk the table.
	filtered := f.Q != "" || f.Held || f.Credential != "" || f.ACLDenied ||
		f.Decision != "" || f.ClientID != "" || f.Service != "" || len(f.StatusClasses) > 0
	if f.Since == "" && (filtered || f.Until != "") {
		end := now
		if f.Until != "" {
			if t, err := time.Parse(logsTimeLayout, f.Until); err == nil {
				end = t
			}
		}
		f.Since = end.Add(-logsRanges["7d"]).UTC().Format(logsTimeLayout)
		window = "7d"
	}
	return f, window, ""
}

func parseLogsFilterRaw(r *http.Request, now time.Time) (queries.RequestLogFilter, string, string) {
	var f queries.RequestLogFilter
	window := "all"
	vals := r.URL.Query()
	for k, v := range vals {
		if !logsAllowedParams[k] {
			return f, window, "unknown parameter: " + k
		}
		if len(v) != 1 {
			return f, window, "parameter given more than once: " + k
		}
	}
	text := func(name string) (string, string) {
		v := vals.Get(name)
		if len(v) > logsMaxTextLen {
			return "", name + " too long"
		}
		if strings.ContainsRune(v, 0) {
			return "", name + " invalid"
		}
		return v, ""
	}
	var msg string
	if f.Q, msg = text("q"); msg != "" {
		return f, window, msg
	}
	if f.ClientID, msg = text("client"); msg != "" {
		return f, window, msg
	}
	if f.Service, msg = text("service"); msg != "" {
		return f, window, msg
	}
	if f.Credential, msg = text("credential"); msg != "" {
		return f, window, msg
	}

	if v := vals.Get("range"); v != "" {
		d, ok := logsRanges[v]
		if !ok {
			return f, window, "invalid range (use 15m, 1h, 24h or 7d)"
		}
		if vals.Get("since") != "" || vals.Get("until") != "" {
			return f, window, "range cannot be combined with since/until"
		}
		f.Since = now.Add(-d).UTC().Format(logsTimeLayout)
		window = v
	}
	var since, until time.Time
	if v := vals.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, window, "invalid since (RFC3339)"
		}
		since = t
		f.Since = t.UTC().Format(logsTimeLayout)
		window = "custom"
	}
	if v := vals.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, window, "invalid until (RFC3339)"
		}
		until = t
		f.Until = t.UTC().Format(logsTimeLayout)
		window = "custom"
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return f, window, "since must be before until"
	}

	if v := vals.Get("status"); v != "" {
		seen := map[string]bool{}
		for _, c := range strings.Split(v, ",") {
			switch c {
			case "2xx", "3xx", "4xx", "5xx":
			default:
				return f, window, "invalid status class (use 2xx,3xx,4xx,5xx)"
			}
			if !seen[c] {
				seen[c] = true
				f.StatusClasses = append(f.StatusClasses, c)
			}
		}
	}
	for _, b := range []struct {
		name string
		dst  *bool
	}{{"held", &f.Held}, {"acl_denied", &f.ACLDenied}} {
		if _, present := vals[b.name]; !present {
			continue
		}
		switch vals.Get(b.name) {
		case "1", "true":
			*b.dst = true
		case "0", "false":
		default:
			return f, window, "invalid " + b.name + " (use 1/0 or true/false)"
		}
	}
	if v := vals.Get("decision"); v != "" {
		switch v {
		case queries.DecisionAllowed, queries.DecisionApprovedForward, queries.DecisionACLDenied, queries.DecisionHeld:
			f.Decision = v
		default:
			return f, window, "invalid decision (use allowed, approved_forward, acl_denied or held)"
		}
	}
	id := func(name string) (int64, string) {
		v := vals.Get(name)
		if _, present := vals[name]; !present {
			return 0, ""
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return 0, "invalid " + name
		}
		return n, ""
	}
	if f.BeforeID, msg = id("before_id"); msg != "" {
		return f, window, msg
	}
	if f.AfterID, msg = id("after_id"); msg != "" {
		return f, window, msg
	}
	if f.BeforeID > 0 && f.AfterID > 0 {
		return f, window, "before_id and after_id are mutually exclusive"
	}
	f.Limit = logsDefaultLimit
	if _, present := vals["limit"]; present {
		n, err := strconv.Atoi(vals.Get("limit"))
		if err != nil || n < 1 || n > logsMaxLimit {
			return f, window, "invalid limit (1-200)"
		}
		f.Limit = n
	}
	return f, window, ""
}

// newLogsListHandler serves GET /api/logs. With no query parameters it keeps
// the original contract (a bare JSON array of the latest 500 rows). With any
// parameter it returns a filtered keyset page object.
func newLogsListHandler(q *queries.RequestLogQueries) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Query()) == 0 {
			logs, err := q.Recent(logsLegacyLimit)
			if err != nil {
				handlers.JsonErrorPublic(w, "failed", 500)
				return
			}
			if logs == nil {
				logs = []queries.RequestLogEntry{}
			}
			handlers.JsonResponsePublic(w, logs)
			return
		}
		f, window, msg := parseLogsFilterWindow(r, time.Now())
		if msg != "" {
			handlers.JsonErrorPublic(w, msg, http.StatusBadRequest)
			return
		}
		page, err := q.Search(f)
		if err != nil {
			handlers.JsonErrorPublic(w, "failed", 500)
			return
		}
		resp := logsListResponse{
			Items: page.Items, Total: page.Total, TotalCapped: page.TotalCapped,
			Offset: page.Offset, HasNewer: page.HasNewer, Limit: f.Limit, Window: window,
		}
		if resp.Items == nil {
			resp.Items = []queries.RequestLogEntry{}
		}
		if page.NextBeforeID > 0 {
			id := page.NextBeforeID
			resp.NextBeforeID = &id
		}
		handlers.JsonResponsePublic(w, resp)
	}
}

// newLogsFacetsHandler serves GET /api/logs/facets: values for filter selects.
func newLogsFacetsHandler(q *queries.RequestLogQueries) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := q.Facets()
		if err != nil {
			handlers.JsonErrorPublic(w, "failed", 500)
			return
		}
		handlers.JsonResponsePublic(w, f)
	}
}
