package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ptRow is one row of the phantom token list. The full token never leaves
// the server on this page: only a masked form is rendered.
type ptRow struct {
	ID          string
	EnvName     string
	ClientID    string
	ClientName  string
	ServiceName string
	Credential  string // key name, pool name or ""
	CredKind    string // key | pool | none
	Source      string // manual | bundle
	BundleName  string
	Approval    string // required | auto
	RulesKind   string // none | rules | custom | github | invalid
	RulesText   string
	Masked      string
	Usage       int64
	Inactive    bool
}

type ptKeyOption struct {
	ID          string
	Name        string
	ServiceID   string
	ServiceName string
}

type ptGroupOption struct {
	ID          string
	Name        string
	ServiceID   string
	ServiceName string
	Strategy    string
	Members     int
}

type ptListData struct {
	Rows    []ptRow
	Keys    []ptKeyOption
	Groups  []ptGroupOption
	Clients []ptOption
	Svcs    []ptSvcOption
}

type ptOption struct {
	ID   string
	Name string
}

type ptSvcOption struct {
	ID          string
	Name        string
	DisplayName string
}

// maskPhantom shows just enough of a phantom token to recognise it.
func maskPhantom(tok string) string {
	tok = strings.TrimSpace(tok)
	if len(tok) <= 12 {
		return "••••"
	}
	return tok[:6] + "…" + tok[len(tok)-4:]
}

// ptSimpleMethods are the HTTP methods the visual rule builder can express.
var ptSimpleMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true, "*": true}

// summarizePermissionConfig classifies a stored permission_config the same
// way the detail page's builder does: a rule is "simple" when it is exactly
// one path with an explicit allow/deny for one or more plain methods, and
// "custom" otherwise (rate limits, constraints, deny_all_other, mixed paths).
func summarizePermissionConfig(cfg string) (kind, text string) {
	if strings.TrimSpace(cfg) == "" {
		return "none", "Inherits service and credential rules"
	}
	if err := svc.ValidatePermissionConfig(cfg); err != nil {
		return "invalid", "Invalid rules"
	}
	var head struct {
		Version      string                     `json:"version"`
		Repositories map[string]json.RawMessage `json:"repositories"`
		Rules        []struct {
			Endpoints []struct {
				Method      string          `json:"method"`
				Path        string          `json:"path"`
				Allow       *bool           `json:"allow"`
				Constraints json.RawMessage `json:"constraints"`
			} `json:"endpoints"`
			RateLimit    json.RawMessage `json:"rate_limit"`
			DenyAllOther bool            `json:"deny_all_other"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(cfg), &head); err != nil {
		return "invalid", "Invalid rules"
	}
	if head.Version == "2" {
		n := len(head.Repositories)
		return "github", fmt.Sprintf("GitHub grants · %d %s", n, plural(n, "repository", "repositories"))
	}
	custom := 0
	for _, r := range head.Rules {
		simple := !r.DenyAllOther && len(r.Endpoints) > 0 && (len(r.RateLimit) == 0 || string(r.RateLimit) == "null")
		for _, ep := range r.Endpoints {
			if ep.Allow == nil || !ptSimpleMethods[ep.Method] || (len(ep.Constraints) > 0 && string(ep.Constraints) != "null") ||
				ep.Path != r.Endpoints[0].Path || *ep.Allow != (r.Endpoints[0].Allow != nil && *r.Endpoints[0].Allow) {
				simple = false
			}
		}
		if !simple {
			custom++
		}
	}
	n := len(head.Rules)
	text = fmt.Sprintf("%d %s", n, plural(n, "rule", "rules"))
	if custom > 0 {
		return "custom", text + fmt.Sprintf(" · %d custom", custom)
	}
	return "rules", text
}

// PhantomTokensPage serves GET /admin/phantom-tokens.
func (h *AdminHandler) PhantomTokensPage(w http.ResponseWriter, r *http.Request) {
	phs, err1 := h.placeholders.List("", "")
	svcs, err2 := h.services.List()
	clients, err3 := h.clients.List()
	keys, err4 := h.apiKeys.List("")
	groups, err5 := h.groups.List("")
	for _, err := range []error{err1, err2, err3, err4, err5} {
		if err != nil {
			log.Printf("admin: phantom tokens page: %v", err)
			http.Error(w, "Could not load phantom tokens. Reload the page or check the server log.", http.StatusInternalServerError)
			return
		}
	}
	memberCounts := map[string]int{}
	if h.db != nil {
		// Active keys only, matching what GetMembers returned before.
		rows, err := h.db.Query(`SELECT m.group_id, COUNT(*) FROM api_key_group_members m
			JOIN api_keys k ON k.id = m.api_key_id WHERE k.is_active = 1 GROUP BY m.group_id`)
		if err != nil {
			log.Printf("admin: phantom tokens page: member counts: %v", err)
		} else {
			for rows.Next() {
				var gid string
				var n int
				if err := rows.Scan(&gid, &n); err != nil {
					log.Printf("admin: phantom tokens page: member count scan: %v", err)
					continue
				}
				memberCounts[gid] = n
			}
			if err := rows.Err(); err != nil {
				log.Printf("admin: phantom tokens page: member counts: %v", err)
			}
			_ = rows.Close()
		}
	}

	groupNames := make(map[string]string, len(groups))
	data := ptListData{Rows: []ptRow{}}
	for _, g := range groups {
		groupNames[g.ID] = g.Name
		data.Groups = append(data.Groups, ptGroupOption{ID: g.ID, Name: g.Name, ServiceID: g.ServiceID, ServiceName: g.ServiceName, Strategy: g.Strategy, Members: memberCounts[g.ID]})
	}
	for _, k := range keys {
		data.Keys = append(data.Keys, ptKeyOption{ID: k.ID, Name: k.Name, ServiceID: k.ServiceID, ServiceName: k.ServiceName})
	}
	for _, c := range clients {
		data.Clients = append(data.Clients, ptOption{ID: c.ID, Name: c.Name})
	}
	for _, s := range svcs {
		data.Svcs = append(data.Svcs, ptSvcOption{ID: s.ID, Name: s.Name, DisplayName: s.DisplayName})
	}
	suiteNames := map[string]string{}
	if h.keySuites != nil {
		if list, err := h.keySuites.List(); err == nil {
			for _, s := range list {
				suiteNames[s.ID] = s.Name
			}
		}
	}
	for _, p := range phs {
		row := ptRow{ID: p.ID, EnvName: p.EnvName, ClientID: p.ClientID, ClientName: p.ClientName, ServiceName: p.ServiceName,
			Source: "manual", Approval: "auto", Masked: maskPhantom(p.Placeholder), Usage: p.UsageCount, Inactive: !p.IsActive, CredKind: "none"}
		switch {
		case p.APIKeyID != nil && p.APIKeyName != nil:
			row.Credential, row.CredKind = *p.APIKeyName, "key"
		case p.GroupID != nil:
			row.Credential, row.CredKind = groupNames[*p.GroupID], "pool"
			if row.Credential == "" {
				row.Credential = "Key pool"
			}
		}
		if p.SuiteID != nil {
			row.Source = "bundle"
			row.BundleName = suiteNames[*p.SuiteID]
		}
		if p.RequiresApproval {
			row.Approval = "required"
		}
		cfg := ""
		if p.PermissionConfig != nil {
			cfg = *p.PermissionConfig
		}
		row.RulesKind, row.RulesText = summarizePermissionConfig(cfg)
		data.Rows = append(data.Rows, row)
	}
	h.render(w, r, "phantom_tokens", pageData{
		Title:        "Phantom tokens",
		Active:       "placeholders",
		Placeholders: data,
	})
}

// ptDetailData is JSON-encoded into the detail page. It never contains the
// full phantom token or any stored upstream secret.
type ptDetailData struct {
	ID         string `json:"id"`
	EnvName    string `json:"env_name"`
	Masked     string `json:"masked_token"`
	KeyPath    string `json:"key_path"`
	IsActive   bool   `json:"is_active"`
	Usage      int64  `json:"usage_count"`
	Requests7d *int64 `json:"requests_7d"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	CreatedAt  string `json:"created_at"`
	Source     struct {
		Kind string `json:"kind"` // manual | bundle
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"source"`
	Client struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"client"`
	Service struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		DefaultACL  string `json:"default_acl"`
	} `json:"service"`
	Credential struct {
		Kind string `json:"kind"` // key | pool | none
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
		ACL  string `json:"acl,omitempty"`
	} `json:"credential"`
	PermissionConfig   string `json:"permission_config"`
	RequiresApproval   bool   `json:"requires_approval"`
	ApprovalTTLMinutes int    `json:"approval_ttl_minutes"`
	IsGitHub           bool   `json:"is_github"`
}

type ptDetailPage struct {
	ID      string
	EnvName string
	Data    ptDetailData
}

// PhantomTokenDetailPage serves GET /admin/phantom-tokens/{id}. An unknown id
// renders the shell with a 404 notice.
func (h *AdminHandler) PhantomTokenDetailPage(w http.ResponseWriter, r *http.Request) {
	ph, err := h.placeholders.GetByID(r.PathValue("id"))
	if err != nil || ph == nil {
		w.WriteHeader(http.StatusNotFound)
		h.render(w, r, "phantom_token_detail", pageData{Title: "Phantom token not found", Active: "placeholders", Error: "not_found"})
		return
	}
	d := ptDetailData{ID: ph.ID, EnvName: ph.EnvName, Masked: maskPhantom(ph.Placeholder), KeyPath: ph.KeyPath, IsActive: ph.IsActive,
		Usage: ph.UsageCount, CreatedAt: ph.CreatedAt, RequiresApproval: ph.RequiresApproval, ApprovalTTLMinutes: ph.ApprovalTTLMinutes,
		IsGitHub: ph.ServiceName == "github"}
	if ph.LastUsedAt != nil {
		d.LastUsedAt = *ph.LastUsedAt
	}
	if ph.PermissionConfig != nil {
		d.PermissionConfig = *ph.PermissionConfig
	}
	d.Source.Kind = "manual"
	if ph.SuiteID != nil {
		d.Source.Kind, d.Source.ID = "bundle", *ph.SuiteID
		if h.keySuites != nil {
			if s, err := h.keySuites.GetByID(*ph.SuiteID); err == nil && s != nil {
				d.Source.Name = s.Name
			}
		}
	}
	d.Client.ID, d.Client.Name = ph.ClientID, ph.ClientName
	d.Service.ID, d.Service.Name, d.Service.DisplayName = ph.ServiceID, ph.ServiceName, ph.ServiceName
	if s, err := h.services.GetByID(ph.ServiceID); err == nil && s != nil {
		d.Service.DisplayName, d.Service.DefaultACL = s.DisplayName, s.DefaultACL
	}
	d.Credential.Kind = "none"
	switch {
	case ph.APIKeyID != nil:
		d.Credential.Kind, d.Credential.ID = "key", *ph.APIKeyID
		if k, err := h.apiKeys.GetByID(*ph.APIKeyID); err == nil && k != nil {
			d.Credential.Name, d.Credential.ACL = k.Name, k.ACL
		}
	case ph.GroupID != nil:
		d.Credential.Kind, d.Credential.ID = "pool", *ph.GroupID
		if g, err := h.groups.GetByID(*ph.GroupID); err == nil && g != nil {
			d.Credential.Name = g.Name
		}
	}
	if h.db != nil {
		var n int64
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE placeholder_id = ? AND created_at >= ?`,
			ph.ID, time.Now().UTC().Add(-7*24*time.Hour).Format("2006-01-02 15:04:05")).Scan(&n); err == nil {
			d.Requests7d = &n
		}
	}
	h.render(w, r, "phantom_token_detail", pageData{
		Title:        ph.EnvName,
		Active:       "placeholders",
		Placeholders: ptDetailPage{ID: ph.ID, EnvName: ph.EnvName, Data: d},
	})
}
