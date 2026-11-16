package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

// AccessSimulator answers "would the gateway let this request through?" for a
// phantom token without sending anything anywhere. It builds the request the
// agent would send and runs it through the same gatewayState the proxy uses.
//
// Nothing is written: no request-log row, no approval, no usage counter, no
// rate-limit count, and no key is leased (candidate keys are listed read-only
// instead of going through the key resolver). The one outbound call is the
// read-only default-branch lookup of a repository whose rules mention
// @default (cached, fails closed, same as the proxy), made with each
// candidate credential.
type AccessSimulator struct {
	placeholders *queries.PlaceholderQueries
	services     *queries.ServiceQueries
	apiKeys      *queries.APIKeyQueries
	groups       *queries.GroupQueries
	approvals    *queries.ApprovalQueries
	crypto       *services.Crypto
	// proxy supplies the default-branch lookup (cache, upstream clients) and
	// the GitHub App installation-token cache; it is never used to forward.
	proxy *ProxyHandler
	pc    *services.PermissionChecker
}

// NewAccessSimulator wires the simulator. groups and approvals may be nil
// (group tokens then report no candidate keys, approvals are not consulted).
func NewAccessSimulator(placeholders *queries.PlaceholderQueries, svcs *queries.ServiceQueries, apiKeys *queries.APIKeyQueries, groups *queries.GroupQueries, approvals *queries.ApprovalQueries, crypto *services.Crypto) *AccessSimulator {
	return &AccessSimulator{
		placeholders: placeholders, services: svcs, apiKeys: apiKeys, groups: groups, approvals: approvals, crypto: crypto,
		proxy: NewProxyHandler(svcs, apiKeys, nil, nil, nil, nil, nil).WithCrypto(crypto).WithGitHubDefaultFailureTTL(simDefaultFailureTTL),
		pc:    services.NewPermissionChecker(),
	}
}

// WithGitHubDefaultBranchFetcher replaces the upstream default-branch lookup (tests).
func (a *AccessSimulator) WithGitHubDefaultBranchFetcher(f GitHubDefaultBranchFetcher) *AccessSimulator {
	a.proxy.WithGitHubDefaultBranchFetcher(f)
	return a
}

const (
	simMaxJSONBytes    = 2*maxInspectableRequestBodyBytes + 1<<20
	simMaxPathLength   = 2048
	simMaxHeaders      = 50
	simMaxCandidates   = 20
	simDefaultWorkflow = "ci.yml"
	simEvalTimeout     = 15 * time.Second
	// simDefaultFailureTTL is how long a failed default-branch lookup is
	// remembered, so a down upstream is not hit on every simulation or page load.
	simDefaultFailureTTL = 60 * time.Second
)

// simulateRequest is the body of POST /api/phantom-tokens/{id}/simulate.
type simulateRequest struct {
	Mode string `json:"mode"` // "action" | "raw"

	// action mode
	Repo     string `json:"repo"`
	Action   string `json:"action"`
	Ref      string `json:"ref"`
	Workflow string `json:"workflow"`

	// raw mode
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    string            `json:"query"`
	Headers  map[string]string `json:"headers"`
	BodyText *string           `json:"body_text"`
	BodyHex  *string           `json:"body_hex"`
}

// simRequestEcho is the request that was evaluated (for action mode, the one
// the action was turned into).
type simRequestEcho struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Query    string `json:"query"`
	BodyText string `json:"body_text"` // set when the body is valid UTF-8 text
	BodyHex  string `json:"body_hex"`  // always set (lower-case hex, no spaces)
	BodySize int    `json:"body_size"`
}

type simKeyResult struct {
	KeyID   string `json:"key_id"`
	KeyName string `json:"key_name"`
	Verdict string `json:"verdict"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Layer   string `json:"layer"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason"`
	Sees    string `json:"sees"`
}

type simulateResponse struct {
	Verdict                string         `json:"verdict"` // allowed | refused | held
	Kind                   string         `json:"kind"`    // "" when allowed
	Title                  string         `json:"title"`
	Layer                  string         `json:"layer"`
	Rule                   string         `json:"rule"`
	Reason                 string         `json:"reason"`
	Steps                  []DecisionStep `json:"steps"`
	Refs                   []DecisionRef  `json:"refs"`
	Sees                   string         `json:"sees"`
	DependsOnDefaultBranch bool           `json:"depends_on_default_branch"`
	Request                simRequestEcho `json:"request"`
	// PerKey is present only when the placeholder can resolve to several keys
	// and they do not all reach the same outcome; the top-level fields then
	// describe the most restrictive one.
	PerKey []simKeyResult `json:"per_key,omitempty"`
}

type simHTTPError struct {
	status int
	msg    string
	offset *int
}

func (e *simHTTPError) Error() string { return e.msg }

func simErr(status int, format string, args ...interface{}) *simHTTPError {
	return &simHTTPError{status: status, msg: fmt.Sprintf(format, args...)}
}

// hexBodyError reports where a hex body stopped being valid.
func hexBodyError(msg string, offset int) *simHTTPError {
	return &simHTTPError{status: http.StatusBadRequest, msg: fmt.Sprintf("%s at offset %d", msg, offset), offset: &offset}
}

// parseHexBody decodes hex digits with optional whitespace. Errors carry the
// character offset (0-based rune index in the text as sent) of the first
// problem and name the offending character.
func parseHexBody(s string) ([]byte, *simHTTPError) {
	out := make([]byte, 0, len(s)/2)
	var hi byte
	have := false
	lastDigit, idx := 0, -1
	for _, c := range s {
		idx++
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = byte(c - '0')
		case c >= 'a' && c <= 'f':
			v = byte(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = byte(c-'A') + 10
		default:
			return nil, hexBodyError(fmt.Sprintf("invalid hex character %q", c), idx)
		}
		lastDigit = idx
		if !have {
			hi, have = v, true
			continue
		}
		out = append(out, hi<<4|v)
		have = false
		if len(out) > maxInspectableRequestBodyBytes {
			return nil, &simHTTPError{status: http.StatusRequestEntityTooLarge, msg: fmt.Sprintf("body is larger than %d bytes", maxInspectableRequestBodyBytes)}
		}
	}
	if have {
		return nil, hexBodyError("odd number of hex digits", lastDigit)
	}
	return out, nil
}

var simRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// simActions are the supported action-mode actions.
var simActions = map[string]bool{
	"clone": true, "push": true, "force_or_delete": true, "create_branch": true,
	"open_pr_into": true, "merge_pr": true, "approve_pr": true, "create_tag": true,
	"dispatch": true, "release": true,
}

const (
	simOIDOld = "0123456789abcdef0123456789abcdef01234567"
	simOIDNew = "89abcdef0123456789abcdef0123456789abcdef"
	simOIDZro = "0000000000000000000000000000000000000000"
)

// simReq is a synthetic request, as the proxy sees it after routing.
type simReq struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
}

func jsonBytes(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// receivePackBody builds a git-receive-pack request body: real pkt-line
// framing for the command section (capabilities after NUL on the first
// command), a flush packet, then an empty pack.
func receivePackBody(cmds []receivePackCommand, caps string) []byte {
	var b bytes.Buffer
	for i, c := range cmds {
		line := c.Old + " " + c.New + " " + c.Ref
		if i == 0 {
			line += "\x00" + caps
		}
		pktLine(&b, line)
	}
	b.WriteString("0000")
	b.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x00")
	b.Write(make([]byte, 20))
	return b.Bytes()
}

func branchRef(ref string) string {
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

// buildActionRequest turns an action into the request an agent would send.
func buildActionRequest(req simulateRequest) (*simReq, *simHTTPError) {
	if !simActions[req.Action] {
		return nil, simErr(http.StatusBadRequest, "unknown action %q", req.Action)
	}
	repo := strings.TrimSpace(req.Repo)
	if !simRepoPattern.MatchString(repo) {
		return nil, simErr(http.StatusBadRequest, "repo must be owner/name")
	}
	ref := strings.TrimSpace(req.Ref)
	needsRef := req.Action != "clone" && req.Action != "merge_pr" && req.Action != "approve_pr"
	if needsRef && ref == "" {
		return nil, simErr(http.StatusBadRequest, "ref is required for this action")
	}
	if len(ref) > 1024 || strings.ContainsRune(ref, 0) {
		return nil, simErr(http.StatusBadRequest, "ref is invalid")
	}
	hdr := http.Header{}
	jsonReq := func(method, path string, body interface{}) *simReq {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		return &simReq{Method: method, Path: path, Header: h, Body: jsonBytes(body)}
	}
	switch req.Action {
	case "clone":
		return &simReq{Method: http.MethodGet, Path: "/" + repo + ".git/info/refs", RawQuery: "service=git-upload-pack", Header: hdr}, nil
	case "push", "force_or_delete":
		cmd := receivePackCommand{Old: simOIDOld, New: simOIDNew, Ref: branchRef(ref)}
		caps := "report-status side-band-64k agent=git/2.45.0"
		if req.Action == "force_or_delete" {
			// A forced update looks like any update to the proxy; a delete is
			// the form it can tell apart. Both need the same role.
			cmd.New = simOIDZro
			caps = "report-status delete-refs side-band-64k agent=git/2.45.0"
		}
		h := http.Header{}
		h.Set("Content-Type", "application/x-git-receive-pack-request")
		return &simReq{Method: http.MethodPost, Path: "/" + repo + ".git/git-receive-pack", Header: h,
			Body: receivePackBody([]receivePackCommand{cmd}, caps)}, nil
	case "create_branch":
		return jsonReq(http.MethodPost, "/repos/"+repo+"/git/refs", map[string]string{"ref": branchRef(ref), "sha": simOIDOld}), nil
	case "create_tag":
		tag := strings.TrimPrefix(ref, "refs/tags/")
		return jsonReq(http.MethodPost, "/repos/"+repo+"/git/refs", map[string]string{"ref": "refs/tags/" + tag, "sha": simOIDOld}), nil
	case "open_pr_into":
		return jsonReq(http.MethodPost, "/repos/"+repo+"/pulls", map[string]string{"title": "Simulated pull request", "head": "agent/sim", "base": strings.TrimPrefix(ref, "refs/heads/")}), nil
	case "merge_pr":
		return jsonReq(http.MethodPut, "/repos/"+repo+"/pulls/1/merge", map[string]string{}), nil
	case "approve_pr":
		return jsonReq(http.MethodPost, "/repos/"+repo+"/pulls/1/reviews", map[string]string{"event": "APPROVE"}), nil
	case "dispatch":
		wf := strings.TrimSpace(req.Workflow)
		if wf == "" {
			wf = simDefaultWorkflow
		}
		if len(wf) > 255 || strings.ContainsAny(wf, "/?#% \x00\r\n\t") {
			return nil, simErr(http.StatusBadRequest, "workflow is invalid")
		}
		return jsonReq(http.MethodPost, "/repos/"+repo+"/actions/workflows/"+wf+"/dispatches", map[string]string{"ref": ref}), nil
	case "release":
		tag := strings.TrimPrefix(ref, "refs/tags/")
		return jsonReq(http.MethodPost, "/repos/"+repo+"/releases", map[string]string{"tag_name": tag, "name": tag}), nil
	}
	return nil, simErr(http.StatusBadRequest, "unknown action %q", req.Action)
}

var headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

// buildRawRequest validates a raw-mode request the way the proxy validates a
// live one (method, path), returning the upstream path and query.
func buildRawRequest(req simulateRequest, serviceName string) (*simReq, *simHTTPError) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if !validProxyMethod(method) {
		return nil, simErr(http.StatusBadRequest, "invalid request method")
	}
	raw := strings.TrimSpace(req.Path)
	query := req.Query
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		if query != "" {
			return nil, simErr(http.StatusBadRequest, "give the query either in path or in query, not both")
		}
		raw, query = raw[:i], raw[i+1:]
	}
	if raw == "" || !strings.HasPrefix(raw, "/") || len(raw) > simMaxPathLength || len(query) > simMaxPathLength {
		return nil, simErr(http.StatusBadRequest, "path must start with / and be at most %d bytes", simMaxPathLength)
	}
	if strings.ContainsAny(raw+query, " \t\r\n\x00") {
		return nil, simErr(http.StatusBadRequest, "path must contain no whitespace")
	}
	if strings.Contains(raw+query, "#") {
		return nil, simErr(http.StatusBadRequest, "a # starts a URL fragment, which a client never sends; remove it (write %%23 for a literal #)")
	}
	prefix := "/proxy/" + serviceName
	if strings.HasPrefix(raw, "/proxy/") {
		if raw != prefix && !strings.HasPrefix(raw, prefix+"/") {
			return nil, simErr(http.StatusBadRequest, "path is for another service")
		}
		raw = strings.TrimPrefix(raw, prefix)
		if raw == "" {
			raw = "/"
		}
	}
	u := "http://simulator.invalid" + prefix + raw
	if query != "" {
		u += "?" + query
	}
	hr, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, simErr(http.StatusBadRequest, "path is not a valid URL path")
	}
	if msg := validateProxiedPath(hr); msg != "" {
		return nil, simErr(http.StatusBadRequest, "%s", msg)
	}
	// The gateway's ServeMux redirects a path that needs cleaning (. and ..
	// segments, //) before any handler, and so before any check, runs.
	if cleaned := muxCleanPath(hr.URL.Path); cleaned != hr.URL.Path {
		return nil, simErr(http.StatusBadRequest, "the proxy would redirect this path to %s before checking it; send the cleaned path", strings.TrimPrefix(cleaned, prefix))
	}
	if hasEncodedSlash(hr) {
		return nil, simErr(http.StatusBadRequest, "encoded slashes in the path are not supported by the simulator")
	}
	if len(req.Headers) > simMaxHeaders {
		return nil, simErr(http.StatusBadRequest, "too many headers (at most %d)", simMaxHeaders)
	}
	hdr := http.Header{}
	for k, v := range req.Headers {
		if !headerNamePattern.MatchString(k) || len(k) > 256 || len(v) > 8192 || strings.ContainsAny(v, "\r\n\x00") {
			return nil, simErr(http.StatusBadRequest, "invalid header %q", k)
		}
		hdr.Set(k, v)
	}
	var body []byte
	switch {
	case req.BodyText != nil && req.BodyHex != nil:
		return nil, simErr(http.StatusBadRequest, "give body_text or body_hex, not both")
	case req.BodyHex != nil:
		b, herr := parseHexBody(*req.BodyHex)
		if herr != nil {
			return nil, herr
		}
		body = b
	case req.BodyText != nil:
		if len(*req.BodyText) > maxInspectableRequestBodyBytes {
			return nil, simErr(http.StatusRequestEntityTooLarge, "body is larger than %d bytes", maxInspectableRequestBodyBytes)
		}
		body = []byte(*req.BodyText)
	}
	upstream := strings.TrimPrefix(hr.URL.Path, prefix)
	if upstream == "" {
		upstream = "/"
	}
	return &simReq{Method: method, Path: upstream, RawQuery: hr.URL.RawQuery, Header: hdr, Body: body}, nil
}

// muxCleanPath is net/http's ServeMux path cleaning.
func muxCleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

func writeSimError(w http.ResponseWriter, e *simHTTPError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	out := map[string]interface{}{"error": e.msg}
	if e.offset != nil {
		out["offset"] = *e.offset
	}
	_ = json.NewEncoder(w).Encode(out)
}

// simCandidate is one credential the placeholder can resolve to.
type simCandidate struct {
	ID, Name, ACL string
	Active        bool
	key           models.APIKey
}

// credProblem is what the proxy answers when key resolution cannot produce a
// credential: the status and message the agent sees, plus a step detail.
type credProblem struct {
	status int
	msg    string
	detail string
}

// candidateKeys lists, read-only, the keys the proxy's resolver could pick for
// the placeholder: the assigned key, or the active members of its key group.
// It reads exactly what the resolver reads (a group's members come from
// GetMembers, as in KeyResolver.resolveFromGroup) but never advances a
// round-robin index or bumps a usage counter. When resolution would fail it
// returns the problem the proxy reports instead (a missing or empty key is a
// 500 "key resolution failed"; no key at all is a 403).
func (a *AccessSimulator) candidateKeys(ph *models.PlaceholderKey) (cands []simCandidate, truncated bool, problem *credProblem, err error) {
	failed := func(detail string) *credProblem {
		return &credProblem{status: http.StatusInternalServerError, msg: "key resolution failed", detail: detail}
	}
	switch {
	case ph.APIKeyID != nil:
		k, gerr := a.apiKeys.GetByID(*ph.APIKeyID)
		if gerr != nil {
			if errors.Is(gerr, sql.ErrNoRows) {
				return nil, false, failed("The assigned credential no longer exists"), nil
			}
			return nil, false, nil, gerr
		}
		return []simCandidate{{ID: k.ID, Name: k.Name, ACL: k.ACL, Active: k.IsActive, key: *k}}, false, nil, nil
	case ph.GroupID != nil:
		if a.groups == nil {
			return nil, false, failed("The key group cannot be read"), nil
		}
		members, gerr := a.groups.GetMembers(*ph.GroupID)
		if gerr != nil {
			return nil, false, nil, gerr
		}
		if len(members) == 0 {
			return nil, false, failed("The key group has no active members"), nil
		}
		if len(members) > simMaxCandidates {
			members, truncated = members[:simMaxCandidates], true
		}
		out := make([]simCandidate, 0, len(members))
		for _, k := range members {
			out = append(out, simCandidate{ID: k.ID, Name: k.Name, ACL: k.ACL, Active: k.IsActive, key: k})
		}
		return out, truncated, nil, nil
	}
	return nil, false, &credProblem{status: http.StatusForbidden, msg: "placeholder has no key or group assigned", detail: "No credential or key group is assigned"}, nil
}

func simTitle(verdict, kind string) string {
	switch verdict {
	case GatewayAllowed:
		return "Allowed"
	case GatewayHeld:
		return "Held for approval"
	}
	switch kind {
	case queries.KindServiceACL:
		return "Refused by the service rules"
	case queries.KindAPIKeyACL:
		return "Refused by the credential's rules"
	case queries.KindTokenACL:
		return "Refused by the token's rules"
	case queries.KindCapability:
		return "Refused: the grant does not include this capability"
	case queries.KindBranchRules:
		return "Refused by branch rules"
	case queries.KindInvalidPolicy:
		return "Refused: the branch policy is invalid"
	case queries.KindUncheckable:
		return "Refused: the request cannot be checked"
	case queries.KindCredential:
		return "Refused: no usable credential"
	}
	return "Refused"
}

// defaultBranchFor resolves a repository's default branch with one candidate
// credential, exactly as the proxy does once it has its upstream credential.
func (a *AccessSimulator) defaultBranchFor(ctx context.Context, svc *models.Service, cand simCandidate, tokenConfig string, sr *simReq, repo string) defaultBranchFn {
	return func() (string, error) {
		if a.crypto == nil {
			return "", errors.New("no credential access")
		}
		realKey, err := a.crypto.Decrypt(cand.key.KeyEncrypted)
		if err != nil {
			return "", errors.New("credential unreadable")
		}
		proxyURL, err := services.DecryptUpstreamProxyURL(a.crypto, cand.key.UpstreamProxyURL)
		if err != nil {
			return "", errors.New("upstream proxy unreadable")
		}
		var appCred *githubAppCredential
		if c, ok, perr := parseGitHubAppCredential(realKey); perr != nil {
			return "", errors.New("invalid github app credential")
		} else if ok {
			if verr := services.ValidateGitHubRepoScopePermissionConfig(tokenConfig); verr != nil {
				return "", errors.New("github app assignment requires a valid repository policy")
			}
			appCred = c
			realKey, err = a.proxy.mintGitHubInstallationToken(ctx, c, sr.Method, sr.Path, sr.RawQuery, tokenConfig)
			if err != nil {
				return "", errors.New("github app token mint failed")
			}
		}
		authType, authHeader, authPrefix := svc.AuthType, svc.AuthHeader, svc.AuthPrefix
		if cand.key.RefreshToken != "" {
			authType, authHeader, authPrefix = "bearer", "Authorization", "Bearer "
		}
		client, err := a.proxy.httpClientForUpstream(proxyURL)
		if err != nil {
			return "", err
		}
		return a.proxy.githubDefaultBranchFor(ctx, GitHubDefaultBranchRequest{
			Client: client, APIBase: svc.UpstreamURL, Repo: repo,
			CredentialID: githubDefaultCredentialID(appCred, realKey),
			AuthType:     authType, AuthHeader: authHeader, AuthPrefix: authPrefix, Key: realKey,
		})
	}
}

// simOutcome is the evaluation of one candidate key.
type simOutcome struct {
	cand    simCandidate
	d       *GatewayDecision
	steps   []DecisionStep
	depends bool
}

// evalKey runs the gateway decision for one candidate key. A pool member's
// own rules are not evaluated, as in the proxy (see candidateKeys).
func (a *AccessSimulator) evalKey(ctx context.Context, svc *models.Service, tokenConfig string, cand simCandidate, pool bool, sr *simReq, pre []DecisionStep) simOutcome {
	out := simOutcome{cand: cand}
	if !cand.Active {
		out.steps = append(append([]DecisionStep{}, pre...),
			DecisionStep{Status: queries.StepStop, Label: "Credential status", Detail: "The credential is inactive"})
		out.d = &GatewayDecision{Verdict: GatewayRefused, Kind: queries.KindCredential, Reason: "api key is inactive", Refs: []DecisionRef{}, Sees: jsonErrorBody("api key is inactive")}
		return out
	}
	st := &gatewayState{in: gatewayInput{
		Service: svc.Name, Method: sr.Method, Path: sr.Path, RawQuery: sr.RawQuery,
		Layers:           []proxyACLLayer{{queries.LayerService, svc.DefaultACL}, {queries.LayerAPIKey, cand.ACL}, {queries.LayerPlaceholder, tokenConfig}},
		PermissionConfig: tokenConfig,
		ContentEncoding:  sr.Header.Get("Content-Encoding"),
		Check:            dryACLCheck(a.pc),
		Body:             gatewayBody{Bytes: sr.Body, InHand: true},
	}}
	d := st.decide()
	repo := ""
	if st.guard != nil {
		repo = st.guard.repo
	}
	def := a.defaultBranchFor(ctx, svc, cand, tokenConfig, sr, repo)
	switch {
	case d != nil:
		st.enrich(d, def)
	case st.Held() != nil:
		// An approve rule holds the request before the guard's final check,
		// which is where @default would be looked up.
		d = st.Held()
		if st.guard != nil && st.DependsOnDefault() {
			st.step(queries.StepSkip, stepBranch, "Held before the default branch is looked up; branch rules that need it run after approval")
		}
	default:
		d = st.finish(def)
	}
	out.d, out.depends = d, st.DependsOnDefault()
	out.steps = append(append([]DecisionStep{}, pre...), st.Steps()...)
	if pool {
		for i := range out.steps {
			if out.steps[i].Label == stepCredential {
				out.steps[i] = DecisionStep{Status: queries.StepSkip, Label: stepCredential,
					Detail: "The pool key's own rules are not evaluated: the proxy does not apply them to key pool members"}
			}
		}
	}
	return out
}

func outcomeKey(o simOutcome) string {
	d := o.d
	return strings.Join([]string{d.Verdict, d.Kind, d.Layer, d.Rule, d.Reason, d.Sees}, "\x00")
}

func restrictiveness(o simOutcome) int {
	switch o.d.Verdict {
	case GatewayRefused:
		return 2
	case GatewayHeld:
		return 1
	}
	return 0
}

func echoRequest(sr *simReq) simRequestEcho {
	e := simRequestEcho{Method: sr.Method, Path: sr.Path, Query: sr.RawQuery, BodyHex: hex.EncodeToString(sr.Body), BodySize: len(sr.Body)}
	if utf8.Valid(sr.Body) {
		e.BodyText = string(sr.Body)
	}
	return e
}

// Simulate handles POST /api/phantom-tokens/{id}/simulate.
func (a *AccessSimulator) Simulate(w http.ResponseWriter, r *http.Request) {
	ph, err := a.placeholders.GetByID(r.PathValue("id"))
	if err != nil || ph == nil {
		jsonError(w, "phantom token not found", http.StatusNotFound)
		return
	}
	var req simulateRequest
	r.Body = http.MaxBytesReader(w, r.Body, simMaxJSONBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, "request is too large", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	svc, err := a.services.GetByID(ph.ServiceID)
	if err != nil || svc == nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}

	var sr *simReq
	var herr *simHTTPError
	switch req.Mode {
	case "action":
		if svc.Name != "github" {
			jsonError(w, "action mode is only available for GitHub tokens", http.StatusBadRequest)
			return
		}
		sr, herr = buildActionRequest(req)
	case "raw":
		sr, herr = buildRawRequest(req, svc.Name)
	default:
		jsonError(w, `mode must be "action" or "raw"`, http.StatusBadRequest)
		return
	}
	if herr != nil {
		writeSimError(w, herr)
		return
	}
	// The default-branch lookups are the only outbound calls; bound them.
	ctx, cancel := context.WithTimeout(r.Context(), simEvalTimeout)
	defer cancel()
	resp, herr := a.evaluate(ctx, ph, svc, sr)
	if herr != nil {
		writeSimError(w, herr)
		return
	}
	jsonResponse(w, resp)
}

func (a *AccessSimulator) evaluate(ctx context.Context, ph *models.PlaceholderKey, svc *models.Service, sr *simReq) (*simulateResponse, *simHTTPError) {
	resp := &simulateResponse{Request: echoRequest(sr), Refs: []DecisionRef{}, Steps: []DecisionStep{}}
	finish := func(d *GatewayDecision, steps []DecisionStep) *simulateResponse {
		resp.Verdict, resp.Kind, resp.Layer, resp.Rule, resp.Reason, resp.Sees = d.Verdict, d.Kind, d.Layer, d.Rule, d.Reason, d.Sees
		if d.Verdict == GatewayAllowed {
			resp.Kind = ""
		}
		resp.Title = simTitle(d.Verdict, d.Kind)
		resp.Steps = steps
		if d.Refs != nil {
			resp.Refs = d.Refs
		}
		return resp
	}
	skipAll := func(labels ...string) []DecisionStep {
		out := []DecisionStep{}
		for _, l := range labels {
			out = append(out, DecisionStep{Status: queries.StepSkip, Label: l})
		}
		return out
	}
	allStages := stageOrder
	const lblStatus, lblWide, lblHold = "Token status", "Token-wide approval", "Approval hold"

	if !ph.IsActive {
		d := &GatewayDecision{Verdict: GatewayRefused, Kind: queries.KindCredential, Reason: "placeholder key is inactive", Refs: []DecisionRef{}, Sees: jsonErrorBody("placeholder key is inactive")}
		steps := append([]DecisionStep{{Status: queries.StepStop, Label: lblStatus, Detail: "The phantom token is inactive"}}, skipAll(append([]string{lblWide}, allStages...)...)...)
		return finish(d, append(steps, DecisionStep{Status: queries.StepSkip, Label: lblHold})), nil
	}
	pre := []DecisionStep{{Status: queries.StepPass, Label: lblStatus, Detail: "The phantom token is active"}}
	if ph.RequiresApproval {
		// Held means "held until an admin approves": the production proxy always
		// has an approval store (without one it would refuse outright). Whether
		// an approval for this exact request already exists cannot be known here
		// (scoped approvals are keyed to the full request body), so a gating
		// approve rule is always reported as held.
		approved := false
		if a.approvals != nil {
			if _, aerr := a.approvals.GetValidApproval(ph.ID); aerr == nil {
				approved = true
			} else if !errors.Is(aerr, sql.ErrNoRows) {
				return nil, simErr(http.StatusInternalServerError, "failed to check approval")
			}
		}
		if !approved {
			d := &GatewayDecision{Verdict: GatewayHeld, Kind: queries.KindApproval, Reason: "this token requires an admin's approval before requests go through", Refs: []DecisionRef{}}
			steps := append(pre, DecisionStep{Status: queries.StepStop, Label: lblWide, Detail: d.Reason})
			steps = append(steps, skipAll(allStages...)...)
			return finish(d, append(steps, DecisionStep{Status: queries.StepStop, Label: lblHold, Detail: "Held until an admin approves"})), nil
		}
		pre = append(pre, DecisionStep{Status: queries.StepPass, Label: lblWide, Detail: "A token-wide approval is active"})
	} else {
		pre = append(pre, DecisionStep{Status: queries.StepSkip, Label: lblWide, Detail: "This token does not require approval"})
	}

	tokenConfig := ""
	if ph.PermissionConfig != nil {
		tokenConfig = *ph.PermissionConfig
	}
	cands, truncated, problem, err := a.candidateKeys(ph)
	if err != nil {
		return nil, simErr(http.StatusInternalServerError, "failed to list the token's keys")
	}
	if problem != nil {
		d := &GatewayDecision{Verdict: GatewayRefused, Kind: queries.KindCredential, Reason: problem.msg, Refs: []DecisionRef{}, Sees: jsonErrorBody(problem.msg), status: problem.status}
		steps := append(pre, DecisionStep{Status: queries.StepStop, Label: "Credential status", Detail: problem.detail})
		steps = append(steps, skipAll(allStages...)...)
		return finish(d, append(steps, DecisionStep{Status: queries.StepSkip, Label: lblHold})), nil
	}

	var outs []simOutcome
	if strings.HasPrefix(svc.UpstreamURL, "internal://") && cands[0].Active {
		// The proxy answers an internal service itself: after key resolution
		// (so after an inactive-key refusal) but before any ACL.
		d := &GatewayDecision{Verdict: GatewayAllowed, Refs: []DecisionRef{}}
		return finish(d, append(pre, skipAll(allStages...)...)), nil
	}
	for _, c := range cands {
		outs = append(outs, a.evalKey(ctx, svc, tokenConfig, c, ph.GroupID != nil, sr, pre))
	}
	same := true
	for _, o := range outs[1:] {
		if outcomeKey(o) != outcomeKey(outs[0]) {
			same = false
			break
		}
	}
	pick := outs[0]
	if !same {
		sorted := append([]simOutcome(nil), outs...)
		sort.SliceStable(sorted, func(i, j int) bool { return restrictiveness(sorted[i]) > restrictiveness(sorted[j]) })
		pick = sorted[0]
		for _, o := range outs {
			resp.PerKey = append(resp.PerKey, simKeyResult{
				KeyID: o.cand.ID, KeyName: o.cand.Name, Verdict: o.d.Verdict, Kind: o.d.Kind,
				Title: simTitle(o.d.Verdict, o.d.Kind), Layer: o.d.Layer, Rule: o.d.Rule, Reason: o.d.Reason, Sees: o.d.Sees,
			})
		}
	}
	steps := pick.steps
	switch pick.d.Verdict {
	case GatewayHeld:
		steps = append(steps, DecisionStep{Status: queries.StepStop, Label: lblHold, Detail: "Held until an admin approves this exact request"})
	case GatewayAllowed:
		steps = append(steps, DecisionStep{Status: queries.StepPass, Label: lblHold, Detail: "No approval needed"})
	default:
		steps = append(steps, DecisionStep{Status: queries.StepSkip, Label: lblHold})
	}
	for _, o := range outs {
		if o.depends {
			resp.DependsOnDefaultBranch = true
		}
	}
	out := finish(pick.d, steps)
	if truncated {
		out.Steps = append(out.Steps, DecisionStep{Status: queries.StepSkip, Label: "Key pool", Detail: fmt.Sprintf("Only the first %d keys of the pool were evaluated", simMaxCandidates)})
	}
	return out, nil
}
