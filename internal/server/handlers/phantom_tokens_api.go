package handlers

import (
	"net/http"
	"strings"

	svc "github.com/hackerduck/duckway/internal/server/services"
)

const (
	maxACLTestBodyBytes  = 64 << 10
	maxACLTestPathLength = 2048
)

var aclTestMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true,
}

// aclTestLayer is one ACL layer of the three-level check, evaluated on its own.
type aclTestLayer struct {
	Layer      string `json:"layer"` // service | credential | token
	Label      string `json:"label"` // human name of the config owner
	Configured bool   `json:"configured"`
	Result     string `json:"result"` // allow | deny | approve | skipped
	Reason     string `json:"reason,omitempty"`
	RuleIndex  int    `json:"rule_index"` // -1 when no rule decided
	RuleName   string `json:"rule_name,omitempty"`
	Default    bool   `json:"default,omitempty"` // allowed because no rule matched
	Note       string `json:"note,omitempty"`
}

type aclTestResponse struct {
	Decision  string         `json:"decision"`   // allow | deny | approve
	DecidedBy string         `json:"decided_by"` // service | credential | token | default
	Reason    string         `json:"reason,omitempty"`
	RuleIndex int            `json:"rule_index"` // index within the deciding layer's rules, -1 if none
	RuleName  string         `json:"rule_name,omitempty"`
	Method    string         `json:"method"`
	Path      string         `json:"path"`
	NeedsBody bool           `json:"needs_body"`
	Layers    []aclTestLayer `json:"layers"`
	Approval  struct {
		Required   bool `json:"required"`
		TTLMinutes int  `json:"ttl_minutes"`
	} `json:"approval"`
}

// TestACL evaluates a hypothetical request against a phantom token's ACL
// stack using the real permission engine: service default_acl, then the
// credential's acl, then the token's permission_config (the unsaved draft
// when supplied). It has no side effects: nothing is stored, no rate-limit
// counter moves, no approval is created, no upstream call is made.
func (h *PlaceholderHandler) TestACL(w http.ResponseWriter, r *http.Request) {
	ph, err := h.placeholders.GetByID(r.PathValue("id"))
	if err != nil || ph == nil {
		jsonError(w, "phantom token not found", http.StatusNotFound)
		return
	}

	var req struct {
		Method             string  `json:"method"`
		Path               string  `json:"path"`
		Body               string  `json:"body"`
		PermissionConfig   *string `json:"permission_config"` // draft; absent = stored config
		RequiresApproval   *bool   `json:"requires_approval"`
		ApprovalTTLMinutes *int    `json:"approval_ttl_minutes"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*maxACLTestBodyBytes)
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if !aclTestMethods[method] {
		jsonError(w, "method must be one of GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS", http.StatusBadRequest)
		return
	}
	rawPath := strings.TrimSpace(req.Path)
	if rawPath == "" || !strings.HasPrefix(rawPath, "/") || len(rawPath) > maxACLTestPathLength || strings.ContainsAny(rawPath, " \t\r\n\x00") {
		jsonError(w, "path must start with / and contain no whitespace", http.StatusBadRequest)
		return
	}
	if len(req.Body) > maxACLTestBodyBytes {
		jsonError(w, "body is too large for a test request", http.StatusBadRequest)
		return
	}

	tokenConfig := ""
	if ph.PermissionConfig != nil {
		tokenConfig = *ph.PermissionConfig
	}
	if req.PermissionConfig != nil {
		// Draft preview only: nothing is stored, so no normalisation is needed.
		tokenConfig = *req.PermissionConfig
		if err := svc.ValidatePermissionConfig(tokenConfig); err != nil {
			jsonError(w, "draft rules are invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Same normalisation the proxy applies (git smart-HTTP discovery maps to
	// the upload/receive-pack endpoint that the ACL grants).
	rawQuery := ""
	path := rawPath
	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		path, rawQuery = rawPath[:i], rawPath[i+1:]
	}
	aclMethod, aclPath := proxyACLRequest(method, path, rawQuery)

	serviceConfig, serviceLabel := "", ph.ServiceName
	if h.services != nil {
		if s, err := h.services.GetByID(ph.ServiceID); err == nil && s != nil {
			serviceConfig = s.DefaultACL
			if s.DisplayName != "" {
				serviceLabel = s.DisplayName
			}
		}
	}
	credConfig, credLabel, credNote := "", "", ""
	credSkipped := false
	switch {
	case ph.APIKeyID != nil && h.apiKeys != nil:
		if k, err := h.apiKeys.GetByID(*ph.APIKeyID); err == nil && k != nil {
			credConfig, credLabel = k.ACL, k.Name
		}
	case ph.GroupID != nil:
		credSkipped = true
		credLabel = "key pool"
		credNote = "The credential is picked from a key pool per request; the chosen key's own rules apply at request time."
	}

	pc := svc.NewPermissionChecker()
	body := []byte(req.Body)
	layers := []proxyACLLayer{{"service", serviceConfig}, {"api_key", credConfig}, {"placeholder", tokenConfig}}
	needsBody := requestBodyRequiredForProxyACL(layers, aclMethod, aclPath)

	resp := aclTestResponse{Method: aclMethod, Path: aclPath, NeedsBody: needsBody, RuleIndex: -1, Decision: "allow", DecidedBy: "default"}
	eval := func(layer, label, cfg, note string, skipped bool) aclTestLayer {
		l := aclTestLayer{Layer: layer, Label: label, Configured: strings.TrimSpace(cfg) != "", RuleIndex: -1, Note: note}
		if skipped {
			l.Result = "skipped"
			return l
		}
		ex := pc.Explain(cfg, aclMethod, aclPath, body)
		l.RuleIndex, l.RuleName, l.Default = ex.RuleIndex, ex.RuleName, ex.Default && l.Configured
		if ex.Allowed {
			l.Result = "allow"
		} else if ex.NeedsApproval {
			l.Result, l.Reason = "approve", ex.Reason
		} else {
			l.Result, l.Reason = "deny", ex.Reason
		}
		if ex.RateLimited {
			l.Note = "This rule has a rate limit that a test does not count."
		}
		return l
	}
	resp.Layers = []aclTestLayer{
		eval("service", serviceLabel, serviceConfig, "", false),
		eval("credential", credLabel, credConfig, credNote, credSkipped),
		eval("token", ph.EnvName, tokenConfig, "", false),
	}
	for _, l := range resp.Layers {
		if l.Result == "deny" {
			resp.Decision, resp.DecidedBy, resp.Reason, resp.RuleIndex, resp.RuleName = "deny", l.Layer, l.Reason, l.RuleIndex, l.RuleName
			break
		}
	}
	if resp.Decision == "allow" {
		// No deny anywhere: if any layer matched an approve rule the proxy holds
		// the request for approval. The first such layer (service, credential,
		// token) is the one the proxy reports.
		for _, l := range resp.Layers {
			if l.Result == "approve" {
				resp.Decision, resp.DecidedBy, resp.Reason, resp.RuleIndex, resp.RuleName = "approve", l.Layer, l.Reason, l.RuleIndex, l.RuleName
				break
			}
		}
	}
	if resp.Decision == "allow" {
		// The narrowest configured layer that explicitly matched a rule, else default.
		for i := len(resp.Layers) - 1; i >= 0; i-- {
			l := resp.Layers[i]
			if l.Result == "allow" && l.Configured && !l.Default {
				resp.DecidedBy, resp.RuleIndex, resp.RuleName = l.Layer, l.RuleIndex, l.RuleName
				break
			}
		}
	}

	resp.Approval.Required = ph.RequiresApproval
	resp.Approval.TTLMinutes = ph.ApprovalTTLMinutes
	if req.RequiresApproval != nil {
		resp.Approval.Required = *req.RequiresApproval
	}
	if req.ApprovalTTLMinutes != nil {
		resp.Approval.TTLMinutes = *req.ApprovalTTLMinutes
	}
	jsonResponse(w, resp)
}
