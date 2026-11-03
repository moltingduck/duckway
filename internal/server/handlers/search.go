package handlers

import (
	"database/sql"
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Search limits. The palette asks for one entity per row, never for secrets.
const (
	searchMaxResults = 20
	searchMaxQueryLn = 100
)

// SearchResult is one command-palette entity hit. It deliberately carries only
// a kind, an opaque id, a display name and a console URL.
type SearchResult struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// SearchHandler serves GET /api/search?q= for the admin command palette.
type SearchHandler struct {
	db *sql.DB
}

func NewSearchHandler(db *sql.DB) *SearchHandler { return &SearchHandler{db: db} }

// searchSource is one entity table. Only id and the single name column are ever
// selected, so no secret column can leak. urlFor builds the console deep link.
type searchSource struct {
	kind   string
	query  string // SELECT id, <name> ... WHERE LOWER(<name>) LIKE ? ESCAPE '\' ...
	args   int    // how many times the pattern is bound
	urlFor func(id, name string) string
}

const likeEsc = ` LIKE ? ESCAPE '\'`

var searchSources = []searchSource{
	{kind: "client", args: 1,
		query:  `SELECT id, name FROM clients WHERE LOWER(name)` + likeEsc + ` ORDER BY LOWER(name), id LIMIT ?`,
		urlFor: func(id, _ string) string { return "/admin/clients/" + url.PathEscape(id) }},
	{kind: "credential", args: 1,
		query:  `SELECT id, name FROM api_keys WHERE LOWER(name)` + likeEsc + ` ORDER BY LOWER(name), id LIMIT ?`,
		urlFor: func(_, name string) string { return "/admin/credentials?q=" + url.QueryEscape(name) }},
	{kind: "key_pool", args: 1,
		query:  `SELECT 'rotation:' || id, name FROM api_key_groups WHERE LOWER(name)` + likeEsc + ` ORDER BY LOWER(name), id LIMIT ?`,
		urlFor: func(id, _ string) string { return "/admin/key-pools?pool=" + url.QueryEscape(id) }},
	{kind: "key_pool", args: 1,
		query:  `SELECT 'lease:' || id, name FROM key_groups WHERE LOWER(name)` + likeEsc + ` ORDER BY LOWER(name), id LIMIT ?`,
		urlFor: func(id, _ string) string { return "/admin/key-pools?pool=" + url.QueryEscape(id) }},
	{kind: "bundle", args: 1,
		query:  `SELECT id, name FROM key_suites WHERE LOWER(name)` + likeEsc + ` ORDER BY LOWER(name), id LIMIT ?`,
		urlFor: func(_, _ string) string { return "/admin/key-bundles" }},
	{kind: "phantom_token", args: 1,
		query:  `SELECT id, env_name FROM placeholder_keys WHERE LOWER(env_name)` + likeEsc + ` ORDER BY LOWER(env_name), id LIMIT ?`,
		urlFor: func(id, _ string) string { return "/admin/phantom-tokens/" + url.PathEscape(id) }},
	{kind: "service", args: 2,
		query:  `SELECT id, display_name FROM services WHERE LOWER(display_name)` + likeEsc + ` OR LOWER(name)` + likeEsc + ` ORDER BY LOWER(display_name), id LIMIT ?`,
		urlFor: func(id, _ string) string { return "/admin/services?open=" + url.QueryEscape(id) }},
	{kind: "notification", args: 1,
		query:  `SELECT id, name FROM notification_channels WHERE LOWER(name)` + likeEsc + ` ORDER BY LOWER(name), id LIMIT ?`,
		urlFor: func(_, _ string) string { return "/admin/notifications" }},
}

// escapeLike escapes the LIKE metacharacters (backslash is the ESCAPE char).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Search handles GET /api/search?q=. Admin auth is enforced by the /api/ mux.
func (h *SearchHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		jsonError(w, "q is required", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(q) > searchMaxQueryLn {
		q = string([]rune(q)[:searchMaxQueryLn])
	}
	pattern := "%" + escapeLike(strings.ToLower(q)) + "%"

	perKind := make([][]SearchResult, 0, len(searchSources))
	for _, src := range searchSources {
		args := make([]any, 0, src.args+1)
		for i := 0; i < src.args; i++ {
			args = append(args, pattern)
		}
		args = append(args, searchMaxResults)
		rows, err := h.db.QueryContext(r.Context(), src.query, args...)
		if err != nil {
			log.Printf("search %s: %v", src.kind, err)
			jsonError(w, "search failed", http.StatusInternalServerError)
			return
		}
		var hits []SearchResult
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				log.Printf("search %s scan: %v", src.kind, err)
				jsonError(w, "search failed", http.StatusInternalServerError)
				return
			}
			hits = append(hits, SearchResult{Kind: src.kind, ID: id, Name: name, URL: src.urlFor(id, name)})
		}
		rows.Close()
		perKind = append(perKind, hits)
	}

	// Round-robin across kinds so one large table cannot crowd out the rest.
	out := make([]SearchResult, 0, searchMaxResults)
	for i := 0; len(out) < searchMaxResults; i++ {
		added := false
		for _, hits := range perKind {
			if i < len(hits) && len(out) < searchMaxResults {
				out = append(out, hits[i])
				added = true
			}
		}
		if !added {
			break
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]any{"results": out})
}

// SearchSQLForTest exposes the entity queries to the portability test.
func SearchSQLForTest() []string {
	out := make([]string, 0, len(searchSources))
	for _, s := range searchSources {
		out = append(out, s.query)
	}
	return out
}
