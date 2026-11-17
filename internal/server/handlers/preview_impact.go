package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ImpactPreview answers "what would this rule change have done to the last
// week of traffic?" for a draft of a token's rules, a credential's ACL or a
// service's default ACL. It replays the distinct requests of the request log
// through the same side-effect-free decision path the access simulator uses
// (gatewayState with dryACLCheck), once with the stored configuration and once
// with the draft. It never writes a request-log row, creates an approval or
// counts a rate limit, and never uses the enforcing ACL check.
type ImpactPreview struct {
	sim          *AccessSimulator
	ph           *PlaceholderHandler
	placeholders *queries.PlaceholderQueries
	services     *queries.ServiceQueries
	apiKeys      *queries.APIKeyQueries
	groups       *queries.GroupQueries
	log          *queries.RequestLogQueries
	timeout      time.Duration
	running      chan struct{} // one preview at a time: each replays up to 5,000 groups
}

// NewImpactPreview wires the preview. groups may be nil (group tokens are then
// not fanned out for a key preview).
func NewImpactPreview(sim *AccessSimulator, ph *PlaceholderHandler, placeholders *queries.PlaceholderQueries, svcs *queries.ServiceQueries, apiKeys *queries.APIKeyQueries, groups *queries.GroupQueries, log *queries.RequestLogQueries) *ImpactPreview {
	return &ImpactPreview{sim: sim, ph: ph, placeholders: placeholders, services: svcs, apiKeys: apiKeys, groups: groups, log: log, timeout: previewTimeout, running: make(chan struct{}, 1)}
}

// WithTimeout overrides the evaluation deadline (tests).
func (p *ImpactPreview) WithTimeout(d time.Duration) *ImpactPreview {
	p.timeout = d
	return p
}

const (
	previewWindowDays = queries.ReplayWindowDays
	previewMaxGroups  = 5000
	previewItemCap    = 200
	previewTimeout    = 5 * time.Second
	previewMaxBody    = 1 << 20
	previewRefRows    = 5000
)

// Buckets of the preview result.
const (
	BucketNewlyRefused = "newly_refused"
	BucketNewlyAllowed = "newly_allowed"
	BucketNewlyHeld    = "newly_held"
	BucketNoLongerHeld = "no_longer_held"
	BucketUnknown      = "unknown"
	BucketUnchanged    = "unchanged"
)

var previewBuckets = []string{BucketNewlyRefused, BucketNewlyAllowed, BucketNewlyHeld, BucketNoLongerHeld, BucketUnknown, BucketUnchanged}

// Reasons a group cannot be decided from the log alone.
const (
	ReasonBodyConstraints = "body_constraints"
	ReasonQuery           = "query_not_logged"
	ReasonPushRefs        = "push_refs_not_logged"
	ReasonBody            = "body_depends"
	ReasonDefaultBranch   = "default_branch_unavailable"
)

var previewReasonText = map[string]string{
	ReasonBodyConstraints: "A rule that matches this request constrains its body, and the log does not keep bodies.",
	ReasonQuery:           "This git discovery request is decided by its query string, and the log does not keep it.",
	ReasonPushRefs:        "The log keeps the refs of a push only when it was refused, so this push cannot be re-checked.",
	ReasonBody:            "The branch rules decide this request from its body, and the log does not keep bodies.",
	ReasonDefaultBranch:   "The repository's default branch could not be looked up, so the branch rules cannot be applied.",
}

type previewVerdict struct {
	Verdict string `json:"verdict"` // allowed | refused | held
	Kind    string `json:"kind"`
	Rule    string `json:"rule"`
}

type previewRef struct {
	Ref    string         `json:"ref"`
	Before previewVerdict `json:"before"`
	After  previewVerdict `json:"after"`
}

type previewItem struct {
	Bucket        string         `json:"bucket"`
	PlaceholderID string         `json:"placeholder_id"`
	TokenName     string         `json:"token_name"`
	Method        string         `json:"method"`
	Path          string         `json:"path"`
	Client        string         `json:"client"`
	Hits          int64          `json:"hits"`
	Before        previewVerdict `json:"before"`
	After         previewVerdict `json:"after"`
	Reason        string         `json:"reason,omitempty"`
	ReasonCode    string         `json:"reason_code,omitempty"`
	Refs          []previewRef   `json:"refs,omitempty"`
	SimulateURL   string         `json:"simulate_url"`
}

type previewToken struct {
	PlaceholderID string           `json:"placeholder_id"`
	TokenName     string           `json:"token_name"`
	Client        string           `json:"client"`
	Distinct      int              `json:"distinct"`
	Buckets       map[string]int   `json:"buckets"`
	Hits          map[string]int64 `json:"bucket_hits"`
}

type previewResponse struct {
	WindowDays    int              `json:"window_days"`
	TotalRequests int64            `json:"total_requests"`
	Distinct      int              `json:"distinct"`
	Truncated     bool             `json:"truncated"`
	Buckets       map[string]int   `json:"buckets"`
	BucketHits    map[string]int64 `json:"bucket_hits"`
	Items         []previewItem    `json:"items"`
	Tokens        []previewToken   `json:"tokens,omitempty"`
}

// side is one configuration under evaluation: the stored one or the draft.
type previewSide struct {
	svcACL           string
	keyID            string // the key whose ACL is overridden ("" = none)
	keyACL           string
	tokenConfig      string
	requiresApproval bool
}

func (s previewSide) aclOf(c simCandidate) string {
	if s.keyID != "" && c.ID == s.keyID {
		return s.keyACL
	}
	return c.ACL
}

// sideResult is the outcome of one side for one group.
type sideResult struct {
	Verdict string
	Kind    string
	Rule    string
	Unknown string // reason code when the log cannot decide it
	Refs    []DecisionRef
}

func (r sideResult) verdict() previewVerdict {
	return previewVerdict{Verdict: r.Verdict, Kind: r.Kind, Rule: r.Rule}
}

func restrictRank(v string) int {
	switch v {
	case GatewayRefused:
		return 2
	case GatewayHeld:
		return 1
	}
	return 0
}

type previewTarget struct {
	ph      *models.PlaceholderKey
	svc     *models.Service
	cands   []simCandidate
	problem *credProblem
	before  previewSide
	after   previewSide
}

type previewDraft struct {
	// token draft
	tokenID          string
	tokenConfig      *string
	requiresApproval *bool
	// key draft
	keyID  string
	keyACL *string
	// service draft
	serviceID string
	svcACL    *string
}

// ---- endpoints ----

// Token handles POST /api/placeholders/{id}/preview-impact.
func (p *ImpactPreview) Token(w http.ResponseWriter, r *http.Request) {
	ph, err := p.placeholders.GetByID(r.PathValue("id"))
	if err != nil || ph == nil {
		jsonError(w, "phantom token not found", http.StatusNotFound)
		return
	}
	var req struct {
		PermissionConfig   *string `json:"permission_config"`
		RequiresApproval   *bool   `json:"requires_approval"`
		ApprovalTTLMinutes *int    `json:"approval_ttl_minutes"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, previewMaxBody)
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	d := previewDraft{tokenID: ph.ID, requiresApproval: req.RequiresApproval}
	if req.PermissionConfig != nil {
		normalized, status, verr := p.ph.normalizeDraftPermissionConfig(ph, *req.PermissionConfig)
		if verr != nil {
			jsonError(w, verr.Error(), status)
			return
		}
		d.tokenConfig = &normalized
	}
	p.respond(w, r, []models.PlaceholderKey{*ph}, d)
}

// Key handles POST /api/keys/{id}/preview-impact.
func (p *ImpactPreview) Key(w http.ResponseWriter, r *http.Request) {
	key, err := p.apiKeys.GetByID(r.PathValue("id"))
	if err != nil || key == nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}
	var req struct {
		ACL *string `json:"acl"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, previewMaxBody)
	if err := parseRequest(r, &req); err != nil || req.ACL == nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	// The same checks SetACL makes before it stores the ACL.
	if err := svc.ValidatePermissionConfig(*req.ACL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := svc.ValidateNoBranchPolicies(*req.ACL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	all, err := p.placeholders.List("", key.ServiceID)
	if err != nil {
		jsonError(w, "failed to list tokens", http.StatusInternalServerError)
		return
	}
	// Tokens reach a key only through placeholder_keys.api_key_id or a pool
	// (group_id): the proxy's resolver reads nothing else. Key-suite assignments
	// materialise as ordinary placeholder rows (suite_id) with the same two
	// columns, and the placeholder's key_group_id is not read by the resolver,
	// so this fan-out covers every token that can resolve to the key.
	memberOf := map[string]bool{} // group id -> includes the key
	var tokens []models.PlaceholderKey
	for _, ph := range all {
		switch {
		case ph.APIKeyID != nil && *ph.APIKeyID == key.ID:
			tokens = append(tokens, ph)
		case ph.GroupID != nil && p.groups != nil:
			in, seen := memberOf[*ph.GroupID]
			if !seen {
				members, gerr := p.groups.GetMembers(*ph.GroupID)
				if gerr != nil {
					jsonError(w, "failed to read key group", http.StatusInternalServerError)
					return
				}
				for _, m := range members {
					if m.ID == key.ID {
						in = true
					}
				}
				memberOf[*ph.GroupID] = in
			}
			if in {
				tokens = append(tokens, ph)
			}
		}
	}
	p.respond(w, r, tokens, previewDraft{keyID: key.ID, keyACL: req.ACL})
}

// Service handles POST /api/services/{id}/preview-impact.
func (p *ImpactPreview) Service(w http.ResponseWriter, r *http.Request) {
	s, err := p.services.GetByID(r.PathValue("id"))
	if err != nil || s == nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}
	var req struct {
		DefaultACL *string `json:"default_acl"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, previewMaxBody)
	if err := parseRequest(r, &req); err != nil || req.DefaultACL == nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	// The same checks services Update makes before it stores default_acl.
	if err := svc.ValidatePermissionConfig(*req.DefaultACL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := svc.ValidateNoBranchPolicies(*req.DefaultACL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	tokens, err := p.placeholders.List("", s.ID)
	if err != nil {
		jsonError(w, "failed to list tokens", http.StatusInternalServerError)
		return
	}
	p.respond(w, r, tokens, previewDraft{serviceID: s.ID, svcACL: req.DefaultACL})
}

func (p *ImpactPreview) respond(w http.ResponseWriter, r *http.Request, tokens []models.PlaceholderKey, d previewDraft) {
	select {
	case p.running <- struct{}{}:
		defer func() { <-p.running }()
	default:
		w.Header().Set("Retry-After", "2")
		jsonError(w, "a preview is already running", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	resp, err := p.run(ctx, tokens, d)
	if err != nil {
		jsonError(w, "failed to preview the change", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, resp)
}

// ---- engine ----

func (p *ImpactPreview) prepare(ph *models.PlaceholderKey, d previewDraft) (*previewTarget, error) {
	s, err := p.services.GetByID(ph.ServiceID)
	if err != nil || s == nil {
		return nil, errors.New("service not found")
	}
	tok := ""
	if ph.PermissionConfig != nil {
		tok = *ph.PermissionConfig
	}
	t := &previewTarget{ph: ph, svc: s}
	t.before = previewSide{svcACL: s.DefaultACL, tokenConfig: tok, requiresApproval: ph.RequiresApproval}
	t.after = t.before
	switch {
	case d.tokenID != "":
		if d.tokenConfig != nil {
			t.after.tokenConfig = *d.tokenConfig
		}
		if d.requiresApproval != nil {
			t.after.requiresApproval = *d.requiresApproval
		}
	case d.keyID != "":
		t.after.keyID, t.after.keyACL = d.keyID, *d.keyACL
	case d.serviceID != "":
		t.after.svcACL = *d.svcACL
	}
	if ph.IsActive && !strings.HasPrefix(s.UpstreamURL, "internal://") {
		cands, _, problem, cerr := p.sim.candidateKeys(ph)
		if cerr != nil {
			return nil, cerr
		}
		t.cands, t.problem = cands, problem
	}
	return t, nil
}

func (p *ImpactPreview) run(ctx context.Context, tokens []models.PlaceholderKey, d previewDraft) (*previewResponse, error) {
	resp := &previewResponse{WindowDays: previewWindowDays, Buckets: map[string]int{}, BucketHits: map[string]int64{}, Items: []previewItem{}}
	for _, b := range previewBuckets {
		resp.Buckets[b], resp.BucketHits[b] = 0, 0
	}
	if len(tokens) == 0 {
		return resp, nil
	}
	ids := make([]string, len(tokens))
	byID := map[string]*previewTarget{}
	for i := range tokens {
		ids[i] = tokens[i].ID
	}
	groups, total, truncated, err := p.log.ReplayGroups(ctx, ids, previewWindowDays, previewMaxGroups)
	if err != nil {
		return nil, err
	}
	if err := p.log.AttachReplayRefs(ctx, groups, previewWindowDays, previewRefRows); err != nil {
		return nil, err
	}
	resp.TotalRequests, resp.Truncated = total, truncated
	toks := map[string]models.PlaceholderKey{}
	for _, t := range tokens {
		toks[t.ID] = t
	}
	perToken := map[string]*previewToken{}
	var order []string
	var all []previewItem
	for _, g := range groups {
		if ctx.Err() != nil {
			resp.Truncated = true
			break
		}
		t := byID[g.PlaceholderID]
		if t == nil {
			ph := toks[g.PlaceholderID]
			var perr error
			if t, perr = p.prepare(&ph, d); perr != nil {
				return nil, perr
			}
			byID[g.PlaceholderID] = t
		}
		item := p.evalGroup(ctx, t, g)
		if ctx.Err() != nil {
			// The deadline passed during this group: its lookups may have
			// failed for that reason alone, so it is not reported.
			resp.Truncated = true
			break
		}
		resp.Distinct++
		resp.Buckets[item.Bucket]++
		resp.BucketHits[item.Bucket] += item.Hits
		all = append(all, item)
		pt := perToken[g.PlaceholderID]
		if pt == nil {
			pt = &previewToken{PlaceholderID: g.PlaceholderID, TokenName: t.ph.EnvName, Client: t.ph.ClientName, Buckets: map[string]int{}, Hits: map[string]int64{}}
			perToken[g.PlaceholderID] = pt
			order = append(order, g.PlaceholderID)
		}
		pt.Distinct++
		pt.Buckets[item.Bucket]++
		pt.Hits[item.Bucket] += item.Hits
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Hits > all[j].Hits })
	n := map[string]int{}
	for _, it := range all {
		if n[it.Bucket] >= previewItemCap {
			continue
		}
		n[it.Bucket]++
		resp.Items = append(resp.Items, it)
	}
	if d.keyID != "" || d.serviceID != "" {
		for _, id := range order {
			resp.Tokens = append(resp.Tokens, *perToken[id])
		}
	}
	return resp, nil
}

func simulateURL(id, method, path string) string {
	return "/admin/phantom-tokens/" + url.PathEscape(id) + "?tab=test&method=" + url.QueryEscape(method) + "&path=" + url.QueryEscape(path)
}

func (p *ImpactPreview) evalGroup(ctx context.Context, t *previewTarget, g queries.ReplayGroup) previewItem {
	item := previewItem{
		PlaceholderID: g.PlaceholderID, TokenName: t.ph.EnvName, Method: g.Method, Path: g.Path,
		Client: t.ph.ClientName, Hits: g.Hits, SimulateURL: simulateURL(g.PlaceholderID, g.Method, g.Path),
	}
	before := p.evalSide(ctx, t, t.before, g)
	after := p.evalSide(ctx, t, t.after, g)
	item.Before, item.After = before.verdict(), after.verdict()
	switch {
	case before.Unknown != "" || after.Unknown != "":
		item.Bucket = BucketUnknown
		item.ReasonCode = before.Unknown
		if item.ReasonCode == "" {
			item.ReasonCode = after.Unknown
		}
		item.Reason = previewReasonText[item.ReasonCode]
		item.Before, item.After = unknownVerdict(before), unknownVerdict(after)
	case len(g.Refs) > 0 && len(before.Refs) == len(g.Refs) && len(after.Refs) == len(g.Refs):
		item.Bucket = BucketUnchanged
		for i, r := range g.Refs {
			pr := previewRef{Ref: r.Ref,
				Before: previewVerdict{Verdict: before.Refs[i].Verdict, Kind: before.Kind, Rule: ruleText(before.Refs[i].Pattern, before.Refs[i].Role)},
				After:  previewVerdict{Verdict: after.Refs[i].Verdict, Kind: after.Kind, Rule: ruleText(after.Refs[i].Pattern, after.Refs[i].Role)}}
			item.Refs = append(item.Refs, pr)
			if b := bucketOf(pr.Before.Verdict, pr.After.Verdict); bucketRank(b) > bucketRank(item.Bucket) {
				item.Bucket = b
			}
		}
	default:
		item.Bucket = bucketOf(before.Verdict, after.Verdict)
	}
	return item
}

func unknownVerdict(r sideResult) previewVerdict {
	if r.Unknown != "" {
		return previewVerdict{Verdict: "unknown"}
	}
	return r.verdict()
}

func bucketRank(b string) int {
	switch b {
	case BucketNewlyRefused:
		return 5
	case BucketNewlyHeld:
		return 4
	case BucketNewlyAllowed:
		return 3
	case BucketNoLongerHeld:
		return 2
	}
	return 1
}

// bucketOf classifies a change of verdict.
func bucketOf(before, after string) string {
	switch {
	case before != GatewayRefused && after == GatewayRefused:
		return BucketNewlyRefused
	case before == GatewayRefused && after == GatewayAllowed:
		return BucketNewlyAllowed
	case before != GatewayHeld && after == GatewayHeld:
		return BucketNewlyHeld
	case before == GatewayHeld && after != GatewayHeld:
		return BucketNoLongerHeld
	}
	return BucketUnchanged
}

// evalSide is the verdict one configuration reaches for a group, as the proxy
// would reach it: a token-wide hold first, then the key the pool resolves to
// (the most restrictive member when it is a pool).
func (p *ImpactPreview) evalSide(ctx context.Context, t *previewTarget, sd previewSide, g queries.ReplayGroup) sideResult {
	switch {
	case !t.ph.IsActive:
		return sideResult{Verdict: GatewayRefused, Kind: queries.KindCredential, Rule: "token is inactive"}
	case sd.requiresApproval:
		return sideResult{Verdict: GatewayHeld, Kind: queries.KindApproval, Rule: "token-wide approval"}
	case strings.HasPrefix(t.svc.UpstreamURL, "internal://"):
		return sideResult{Verdict: GatewayAllowed}
	case t.problem != nil:
		return sideResult{Verdict: GatewayRefused, Kind: queries.KindCredential, Rule: t.problem.msg}
	}
	var best *sideResult
	unknown := ""
	for _, c := range t.cands {
		r := p.evalCandidate(ctx, t, sd, c, g)
		if r.Unknown != "" {
			if unknown == "" {
				unknown = r.Unknown
			}
			continue
		}
		if best == nil || restrictRank(r.Verdict) > restrictRank(best.Verdict) {
			rr := r
			best = &rr
		}
	}
	switch {
	case best != nil && (unknown == "" || best.Verdict == GatewayRefused):
		return *best
	case unknown != "":
		return sideResult{Unknown: unknown}
	}
	return sideResult{Verdict: GatewayAllowed}
}

func (p *ImpactPreview) evalCandidate(ctx context.Context, t *previewTarget, sd previewSide, cand simCandidate, g queries.ReplayGroup) sideResult {
	s := *t.svc
	s.DefaultACL = sd.svcACL
	cand.ACL = sd.aclOf(cand)
	sr := &simReq{Method: g.Method, Path: g.Path, Header: http.Header{}}
	if !cand.Active {
		return fromOutcome(p.sim.evalKey(ctx, &s, sd.tokenConfig, cand, sr, nil))
	}
	layers := []proxyACLLayer{{queries.LayerService, s.DefaultACL}, {queries.LayerAPIKey, cand.ACL}, {queries.LayerPlaceholder, sd.tokenConfig}}
	aclMethod, aclPath := proxyACLRequest(g.Method, g.Path, "")
	if strings.EqualFold(g.Method, http.MethodGet) && strings.HasSuffix(g.Path, ".git/info/refs") {
		// The query is not logged. The proxy rewrites GET .../info/refs?service=X to
		// POST .../git-X before the ACL check (proxyACLRequest), so evaluate both
		// forms it maps to: refused only when each is refused, otherwise unknown.
		base := strings.TrimSuffix(g.Path, ".git/info/refs")
		first := p.aclOnlyAt(http.MethodPost, base+".git/git-upload-pack", layers, ReasonQuery)
		second := p.aclOnlyAt(http.MethodPost, base+".git/git-receive-pack", layers, ReasonQuery)
		if first.Unknown == "" && second.Unknown == "" {
			return first
		}
		return sideResult{Unknown: ReasonQuery}
	}
	if requestBodyRequiredForProxyACL(layers, aclMethod, aclPath) {
		return sideResult{Unknown: ReasonBodyConstraints}
	}
	if s.Name == "github" {
		if guard, err := newGitHubGuard(sd.tokenConfig, g.Method, g.Path, ""); err == nil && guard != nil && guard.deny == "" {
			switch {
			case guard.receivePack && len(g.Refs) > 0:
				sr.Header.Set("Content-Type", "application/x-git-receive-pack-request")
				sr.Body = receivePackBody(pushCommands(g.Refs), "report-status side-band-64k")
			case guard.receivePack:
				return p.aclOnly(g, layers, ReasonPushRefs)
			case guard.needsJSONBody():
				return p.aclOnly(g, layers, ReasonBody)
			}
		}
	}
	return fromOutcome(p.sim.evalKey(ctx, &s, sd.tokenConfig, cand, sr, nil))
}

// pushCommands rebuilds the commands of a logged push: an update per ref (a
// ref whose logged reason names a deletion is rebuilt as one).
func pushCommands(refs []queries.DenyRef) []receivePackCommand {
	out := make([]receivePackCommand, 0, len(refs))
	for _, r := range refs {
		c := receivePackCommand{Old: simOIDOld, New: simOIDNew, Ref: r.Ref}
		if strings.Contains(strings.ToLower(r.Reason), "delet") {
			c.New = simOIDZro
		}
		out = append(out, c)
	}
	return out
}

// aclOnly runs just the three ACL layers (the branch rules need what the log
// lacks): a refusal there is known whatever the body says, anything else is
// unknown for reason. With no service name decide stops after the layers.
func (p *ImpactPreview) aclOnly(g queries.ReplayGroup, layers []proxyACLLayer, reason string) sideResult {
	return p.aclOnlyAt(g.Method, g.Path, layers, reason)
}

func (p *ImpactPreview) aclOnlyAt(method, path string, layers []proxyACLLayer, reason string) sideResult {
	st := &gatewayState{in: gatewayInput{
		Method: method, Path: path, Layers: layers, Check: dryACLCheck(p.sim.pc), Body: gatewayBody{InHand: true},
	}}
	if d := st.decide(); d != nil && d.Verdict == GatewayRefused {
		return fromDecision(d)
	}
	return sideResult{Unknown: reason}
}

func fromDecision(d *GatewayDecision) sideResult {
	r := sideResult{Verdict: d.Verdict, Kind: d.Kind, Rule: d.Rule, Refs: d.Refs}
	if d.Verdict == GatewayAllowed {
		r.Kind = ""
	}
	return r
}

func fromOutcome(o simOutcome) sideResult {
	d := o.d
	if d.Verdict == GatewayRefused && d.Kind == queries.KindUncheckable && strings.HasPrefix(d.Reason, "the repository's default branch could not be determined") {
		return sideResult{Unknown: ReasonDefaultBranch}
	}
	return fromDecision(d)
}
