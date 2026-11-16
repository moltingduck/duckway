package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/services"
)

// GatewayDecision is the verdict the gateway reaches for one request. The
// proxy and the access simulator both obtain it from the same functions in
// this file (gatewayState), which perform no side effects: they never write
// the request log, create approvals, lease or rotate a key, or forward
// anything. The proxy then acts on the decision (logs, answers, holds or
// forwards); the simulator only reports it.
type GatewayDecision struct {
	Verdict string // GatewayAllowed | GatewayRefused | GatewayHeld
	Kind    string // one of queries.DenyKinds, or "credential" for simulator-only credential problems
	Layer   string // ACL layer (service | api_key | placeholder) when a layer decided
	Rule    string // rule name, or "pattern (role)" for a branch rule
	Reason  string // the text sent to the agent today (without the "duckway: " prefix)
	Refs    []DecisionRef
	Sees    string // exact client-visible body (or the report-status lines of a push), as text

	// Transport details the proxy needs to answer exactly as before.
	status    int // HTTP status of a JSON refusal
	jsonMsg   string
	receive   bool // answer with a receive-pack refusal
	rp        *receivePackRequest
	reasons   []string
	fallback  string
	drain     bool  // the unread request body should be drained before answering
	logDenied bool  // the proxy records an acl_denied row
	internal  error // detail the proxy logs (never sent to the agent)
}

// Verdicts of a GatewayDecision.
const (
	GatewayAllowed = "allowed"
	GatewayRefused = "refused"
	GatewayHeld    = "held"
)

// DecisionRef is the verdict for one ref of a push.
type DecisionRef struct {
	Ref     string `json:"ref"`
	Verdict string `json:"verdict"` // allowed | refused
	Pattern string `json:"pattern"`
	Role    string `json:"role"`
	Reason  string `json:"reason"`
}

// DecisionStep is one stage of the decision path, in evaluation order.
type DecisionStep struct {
	Status string `json:"status"` // pass | stop | skip
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// Stage labels of the decision path (the simulator adds the approval ones).
const (
	stepService    = "Service rules"
	stepCredential = "Credential rules"
	stepToken      = "Token rules"
	stepCapability = "Capability toggles"
	stepBranch     = "Branch rules"
)

const (
	refVerdictAllowed = "allowed"
	refVerdictRefused = "refused"
	collateralReason  = "another ref in this push was refused"
)

// Messages that identify refusals by a capability toggle rather than a rule.
const (
	msgMergeNotPermitted   = "merging pull requests is not permitted for this grant"
	msgApproveNotPermitted = "submitting approving reviews is not permitted for this grant"
)

// ruleText is the "Decided by" text for a branch rule or tag patterns.
func ruleText(pattern, role string) string {
	switch {
	case role == "tag":
		return pattern
	case pattern == "":
		return ""
	}
	return pattern + " \u2192 " + role
}

// capabilityOf names the capability toggle behind a capability refusal.
func capabilityOf(msg string) string {
	switch msg {
	case msgMergeNotPermitted:
		return "pull_requests_merge"
	case msgApproveNotPermitted:
		return "pull_requests_approve"
	}
	return ""
}

func (st *gatewayState) tagRuleText() string {
	pats := []string{}
	if st.guard != nil {
		pats = st.guard.policy.BranchPolicy.Tags.Create
	}
	if len(pats) == 0 {
		return "tag create patterns: (none)"
	}
	return "tag create patterns: " + strings.Join(pats, ", ")
}

func layerKind(layer string) string {
	switch layer {
	case "service":
		return "service_acl"
	case "api_key":
		return "api_key_acl"
	}
	return "token_acl"
}

// guardKind classifies a branch-rule refusal message.
func guardKind(msg string) string {
	switch {
	case msg == msgMergeNotPermitted || msg == msgApproveNotPermitted:
		return "capability"
	case strings.HasPrefix(msg, "the repository's default branch could not be determined"),
		strings.HasPrefix(msg, "endpoint not allowed under branch rules"),
		strings.HasPrefix(msg, "unsupported write request"),
		strings.HasPrefix(msg, "request body must be a JSON object"),
		strings.HasPrefix(msg, "field \""),
		strings.HasPrefix(msg, "ref path cannot be determined"),
		strings.Contains(msg, "cannot be checked"),
		strings.Contains(msg, "git-receive-pack"),
		strings.Contains(msg, "signed pushes"),
		strings.Contains(msg, "method override"),
		strings.HasPrefix(msg, "pushes with a Content-Encoding"),
		strings.Contains(msg, "truncated git-receive-pack"):
		return "uncheckable"
	}
	return "branch_rules"
}

// aclResult is one ACL layer's verdict.
type aclResult struct {
	Allowed       bool
	NeedsApproval bool
	Reason        string
	RuleName      string
	RuleIndex     int // -1 when no rule decided
}

// aclCheckFn evaluates one ACL layer. The proxy's version counts rate limits
// (it must); the simulator's never does.
type aclCheckFn func(config, method, path string, body []byte) aclResult

func ruleLabel(name string, idx int) string {
	if name != "" {
		return name
	}
	if idx >= 0 {
		return fmt.Sprintf("rule %d", idx+1)
	}
	return ""
}

// enforcingACLCheck is the proxy's ACL evaluation (rate limits are counted).
// The rule name of a denial is looked up with the side-effect-free Explain.
func (h *ProxyHandler) enforcingACLCheck(placeholderID string) aclCheckFn {
	return func(config, method, path string, body []byte) aclResult {
		r := h.permissions.Check(config, placeholderID, method, path, body)
		res := aclResult{Allowed: r.Allowed, NeedsApproval: r.NeedsApproval, Reason: r.Reason, RuleName: r.RuleName, RuleIndex: -1}
		if !r.Allowed && !r.NeedsApproval {
			if ex := h.permissions.Explain(config, method, path, body); ex.RuleIndex >= 0 {
				res.RuleName, res.RuleIndex = ex.RuleName, ex.RuleIndex
			}
		}
		return res
	}
}

// dryACLCheck is the simulator's ACL evaluation: Explain never touches a
// rate-limit counter.
func dryACLCheck(pc *services.PermissionChecker) aclCheckFn {
	return func(config, method, path string, body []byte) aclResult {
		ex := pc.Explain(config, method, path, body)
		return aclResult{Allowed: ex.Allowed, NeedsApproval: ex.NeedsApproval, Reason: ex.Reason, RuleName: ex.RuleName, RuleIndex: ex.RuleIndex}
	}
}

// gatewayBody is how a decision reads the request body: either already in
// hand (Bytes, InHand) or as a stream that the decision reads only as far as
// it must (receive-pack command section) or via ReadAll (a bounded read).
type gatewayBody struct {
	Bytes   []byte
	InHand  bool
	Stream  io.Reader
	ReadAll func(io.Reader) ([]byte, error)
}

// gatewayInput is everything a decision needs about the request and the
// resolved grant. Nothing here is a handle that could cause a side effect
// except Stream, which is only read.
type gatewayInput struct {
	Service          string
	Method           string // upper-case, as sent
	Path             string // upstream path (decoded)
	RawQuery         string
	Layers           []proxyACLLayer
	PermissionConfig string
	ContentEncoding  string
	Check            aclCheckFn
	Body             gatewayBody
}

// gatewayState carries a decision across its two phases. Phase one (decide)
// needs no upstream credential; between the phases the proxy may hold the
// request for approval; phase two (finish) resolves @default and so needs the
// credential.
type gatewayState struct {
	in           gatewayInput
	guard        *githubGuard
	approveLayer string
	approveRule  string
	body         []byte
	inHand       bool
	streamPrefix []byte
	defaultUsed  bool
	steps        []DecisionStep
	aclMethod    string
	aclPath      string
}

func (st *gatewayState) step(status, label, detail string) {
	st.steps = append(st.steps, DecisionStep{Status: status, Label: label, Detail: detail})
}

// Steps returns the decision path with every stage not reached marked skipped.
func (st *gatewayState) Steps() []DecisionStep {
	out := append([]DecisionStep(nil), st.steps...)
	seen := map[string]bool{}
	for _, s := range out {
		seen[s.Label] = true
	}
	for _, l := range []string{stepService, stepCredential, stepToken, stepCapability, stepBranch} {
		if !seen[l] {
			out = append(out, DecisionStep{Status: "skip", Label: l})
		}
	}
	return out
}

func jsonErrorBody(msg string) string {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(map[string]string{"error": msg})
	return b.String()
}

func (st *gatewayState) refuseJSON(kind, layer, rule, reason string, status int, msg string) *GatewayDecision {
	return &GatewayDecision{
		Verdict: GatewayRefused, Kind: kind, Layer: layer, Rule: rule, Reason: reason,
		Refs: []DecisionRef{}, Sees: jsonErrorBody(msg),
		status: status, jsonMsg: msg, logDenied: true,
	}
}

// refuseGuard mirrors the proxy's branch-rule refusal answer: a report-status
// rejection for receive-pack, 403 JSON for everything else.
func (st *gatewayState) refuseGuard(rp *receivePackRequest, reasons []string, reason string, drain bool, e *guardEval) *GatewayDecision {
	kind := guardKind(reason)
	rule := ""
	d := &GatewayDecision{Verdict: GatewayRefused, Kind: kind, Layer: "placeholder", Reason: reason, Refs: []DecisionRef{}, logDenied: true}
	switch {
	case kind == "capability":
		rule = capabilityOf(reason)
	case st.guard != nil && strings.Contains(reason, "tag") && strings.Contains(reason, "may not be created"):
		rule = st.tagRuleText()
	case st.guard != nil && kind == "branch_rules" && len(reasons) == 0:
		rule = st.guardRuleHint(reason)
	}
	if st.in.Method == http.MethodPost && strings.HasSuffix(st.in.Path, ".git/git-receive-pack") {
		d.receive, d.rp, d.reasons, d.fallback, d.drain = true, rp, reasons, reason, drain
		d.Sees = receivePackSees(rp, reasons, reason)
		if rp != nil && len(reasons) == len(rp.Commands) {
			d.Refs = st.describeRefs(e, rp.Commands, reasons)
			rule = firstRefusedRule(d.Refs)
		}
	} else {
		d.status, d.jsonMsg = http.StatusForbidden, "duckway: "+reason
		d.Sees = jsonErrorBody(d.jsonMsg)
	}
	d.Rule = rule
	return d
}

// firstRefusedRule is the rule text of the first refused ref that has one.
func firstRefusedRule(refs []DecisionRef) string {
	for _, r := range refs {
		if r.Verdict == refVerdictRefused && r.Pattern != "" {
			return ruleText(r.Pattern, r.Role)
		}
	}
	return ""
}

// guardRuleHint names the branch rule behind a REST refusal when the message
// quotes a branch ("branch "x" has role "r" ...").
func (st *gatewayState) guardRuleHint(reason string) string {
	i := strings.Index(reason, "branch \"")
	if i < 0 {
		return ""
	}
	rest := reason[i+len("branch \""):]
	j := strings.Index(rest, "\"")
	if j < 0 {
		return ""
	}
	branch := rest[:j]
	role := ""
	if k := strings.Index(rest, "has role \""); k >= 0 {
		r2 := rest[k+len("has role \""):]
		if m := strings.Index(r2, "\""); m >= 0 {
			role = r2[:m]
		}
	}
	rule, matched := st.guard.policy.BranchPolicy.SelectRule(branch, "")
	if matched && rule.Branches != services.GitHubDefaultBranchToken {
		return ruleText(rule.Branches, rule.Role)
	}
	if role != "" {
		return ruleText("(no rule)", role)
	}
	return ""
}

// receivePackSees is the report-status text the git client receives, or the
// plain message when it did not ask for report-status.
func receivePackSees(rp *receivePackRequest, reasons []string, fallback string) string {
	if rp == nil || (!rp.Caps["report-status"] && !rp.Caps["report-status-v2"]) {
		return "duckway: " + fallback + "\n"
	}
	var b strings.Builder
	b.WriteString("unpack ok\n")
	for i, c := range rp.Commands {
		reason := ""
		if i < len(reasons) {
			reason = reasons[i]
		}
		if reason == "" {
			reason = collateralReason
		}
		b.WriteString(receivePackNgLine(c.Ref, reason))
	}
	return b.String()
}

// describeRefs gives, per push command, the verdict and the rule behind it.
func (st *gatewayState) describeRefs(e *guardEval, cmds []receivePackCommand, reasons []string) []DecisionRef {
	out := make([]DecisionRef, 0, len(cmds))
	for i, c := range cmds {
		r := DecisionRef{Ref: c.Ref, Verdict: refVerdictAllowed}
		if i < len(reasons) && reasons[i] != "" {
			r.Verdict, r.Reason = refVerdictRefused, reasons[i]
		}
		r.Pattern, r.Role = st.describeRule(e, c.Ref, r.Verdict == refVerdictRefused)
		out = append(out, r)
	}
	return out
}

// describeRule names the branch rule (pattern, role) governing a ref, or the
// matching tag pattern. It is empty when that depends on an unknown default.
func (st *gatewayState) describeRule(e *guardEval, ref string, refused bool) (pattern, role string) {
	if st.guard == nil {
		return "", ""
	}
	pol := st.guard.policy.BranchPolicy
	switch {
	case strings.HasPrefix(ref, "refs/heads/") && len(ref) > len("refs/heads/") && len(ref) <= maxGitHubRefNameBytes:
		b := strings.TrimPrefix(ref, "refs/heads/")
		def := ""
		if pol.HasDefaultRule() && e != nil {
			d, err := e.defaultBranch()
			if err != nil {
				// Without the default branch the rule is only known when it is
				// the same either way.
				ro, okO := pol.SelectRule(b, "")
				rd, okD := pol.SelectRule(b, b)
				if (okO != okD || ro.Branches != rd.Branches || ro.Role != rd.Role) && !refused {
					return "", ""
				}
				// A refused ref is refused whether or not it is the default
				// branch, so the most specific non-@default rule is the one
				// that decided it.
				if !okO {
					return "(no rule)", services.GitHubRoleRead
				}
				return ro.Branches, ro.Role
			}
			def = d
		}
		rule, matched := pol.SelectRule(b, def)
		if !matched {
			return "(no rule)", services.GitHubRoleRead
		}
		return rule.Branches, rule.Role
	case strings.HasPrefix(ref, "refs/tags/") && len(ref) > len("refs/tags/") && len(ref) <= maxGitHubRefNameBytes:
		tag := strings.TrimPrefix(ref, "refs/tags/")
		for _, pat := range pol.Tags.Create {
			if services.GitHubGlobMatch(strings.TrimPrefix(pat, "refs/tags/"), tag) {
				return "tag create patterns: " + pat, "tag"
			}
		}
		return st.tagRuleText(), "tag"
	}
	return "", ""
}

// decide runs phase one: the three ACL layers, then the GitHub guard's
// checks that need no upstream credential. A non-nil decision is final
// (refused); otherwise the caller may hold the request for approval
// (approveLayer != "") and must then call finish.
func (st *gatewayState) decide() *GatewayDecision {
	in := st.in
	st.body, st.inHand = in.Body.Bytes, in.Body.InHand
	st.aclMethod, st.aclPath = proxyACLRequest(in.Method, in.Path, in.RawQuery)
	for _, layer := range in.Layers {
		label := map[string]string{"service": stepService, "api_key": stepCredential, "placeholder": stepToken}[layer.name]
		if layer.config == "" {
			st.step("skip", label, "No rules configured")
			continue
		}
		res := in.Check(layer.config, st.aclMethod, st.aclPath, st.body)
		if res.NeedsApproval {
			if st.approveLayer == "" {
				st.approveLayer, st.approveRule = layer.name, res.RuleName
			}
			st.step("pass", label, "Matched approve rule "+quoteRule(res.RuleName, res.RuleIndex)+": needs an admin's approval")
			continue
		}
		if !res.Allowed {
			st.step("stop", label, res.Reason)
			return st.refuseJSON(layerKind(layer.name), layer.name, ruleLabel(res.RuleName, res.RuleIndex), res.Reason,
				http.StatusForbidden, "permission denied ("+layer.name+"): "+res.Reason)
		}
		detail := "Allowed"
		if res.RuleName != "" || res.RuleIndex >= 0 {
			detail = "Allowed by " + quoteRule(res.RuleName, res.RuleIndex)
		}
		st.step("pass", label, detail)
	}
	if in.Service != "github" {
		return nil
	}
	g, gerr := newGitHubGuard(in.PermissionConfig, in.Method, in.Path, in.RawQuery)
	if gerr != nil {
		st.step("stop", stepBranch, "The branch policy of this grant is invalid")
		d := st.refuseJSON("invalid_policy", "placeholder", "", "the branch policy of this grant is invalid",
			http.StatusForbidden, "duckway: the branch policy of this grant is invalid")
		d.internal = gerr
		return d
	}
	st.guard = g
	if g == nil {
		st.step("skip", stepCapability, "No branch rules apply to this request")
		st.step("skip", stepBranch, "No branch rules apply to this request")
		return nil
	}
	body := &in.Body
	switch {
	case g.deny != "":
		st.step("stop", stepBranch, g.deny)
		return st.refuseGuard(nil, nil, g.deny, false, nil)
	case g.receivePack:
		if enc := strings.ToLower(strings.TrimSpace(in.ContentEncoding)); enc != "" && enc != "identity" {
			msg := "pushes with a Content-Encoding cannot be checked against branch rules"
			st.step("stop", stepBranch, msg)
			return st.refuseGuard(nil, nil, msg, false, nil)
		}
		src := body.Stream
		if st.inHand {
			src = bytes.NewReader(st.body)
		}
		if src == nil {
			src = bytes.NewReader(nil)
		}
		rp, perr := parseReceivePackCommands(src, receivePackMaxCommandBytes, receivePackMaxCommands)
		if perr != nil {
			st.step("stop", stepBranch, perr.Error())
			return st.refuseGuard(nil, nil, perr.Error(), !st.inHand, nil)
		}
		g.rp = rp
		if !st.inHand {
			st.streamPrefix = rp.Prefix
		}
	case !g.restOK && g.method != http.MethodGet && g.method != http.MethodHead:
		msg := "duckway: request path cannot be checked against branch rules"
		st.step("stop", stepBranch, strings.TrimPrefix(msg, "duckway: "))
		d := st.refuseJSON("uncheckable", "placeholder", "", strings.TrimPrefix(msg, "duckway: "), http.StatusForbidden, msg)
		return d
	case g.needsJSONBody():
		if !st.inHand && body.Stream != nil && body.ReadAll != nil {
			b, rerr := body.ReadAll(body.Stream)
			if rerr != nil {
				msg := "request body too large or unreadable for branch rule check"
				st.step("stop", stepBranch, msg)
				return st.refuseJSON("uncheckable", "placeholder", "", msg, http.StatusRequestEntityTooLarge, msg)
			}
			st.body, st.inHand = b, true
		}
		g.body = st.body
	}
	// A refusal that does not depend on the default branch is final: decided
	// before an approval rule can hold the request for an admin.
	if definite, msg, reasons := g.precheck(); definite {
		d := st.refuseGuard(g.rp, reasons, msg, g.receivePack && !st.inHand, &guardEval{policy: g.policy, unknownDefault: true, def: func() (string, error) { return "", errDefaultDeferred }})
		st.guardStopStep(d)
		return d
	}
	return nil
}

func quoteRule(name string, idx int) string {
	if l := ruleLabel(name, idx); l != "" {
		return "“" + l + "”"
	}
	return "a rule"
}

func (st *gatewayState) guardStopStep(d *GatewayDecision) {
	if d.Kind == "capability" {
		st.step("stop", stepCapability, d.Reason)
		return
	}
	st.step("pass", stepCapability, "Merge and approve toggles do not block this request")
	st.step("stop", stepBranch, d.Reason)
}

// Held reports the decision of a request an approve rule gates, once phase
// one has not refused it.
func (st *gatewayState) Held() *GatewayDecision {
	if st.approveLayer == "" {
		return nil
	}
	return &GatewayDecision{
		Verdict: GatewayHeld, Kind: "approval", Layer: st.approveLayer, Rule: st.approveRule,
		Reason: "an approve rule matched; an admin must approve this exact request", Refs: []DecisionRef{},
	}
}

// DependsOnDefault reports whether the decision needs the repository's
// default branch (the lookup happens in finish, or would after an approval).
func (st *gatewayState) DependsOnDefault() bool {
	return st.defaultUsed || (st.guard != nil && st.guard.policy.BranchPolicy.HasDefaultRule())
}

// finish runs phase two: the guard's final check, resolving @default through
// def (read-only, cached, fails closed). It returns an allowed or refused
// decision; with no guard the request is allowed.
func (st *gatewayState) finish(def defaultBranchFn) *GatewayDecision {
	g := st.guard
	allowed := &GatewayDecision{Verdict: GatewayAllowed, Refs: []DecisionRef{}}
	if g == nil {
		return allowed
	}
	wrapped := func() (string, error) {
		st.defaultUsed = true
		return def()
	}
	if g.receivePack {
		e := &guardEval{policy: g.policy, def: wrapped}
		reasons := e.checkReceivePack(g.rp.Commands)
		if anyRefused(reasons) {
			d := st.refuseGuard(g.rp, reasons, firstReason(reasons), !st.inHand, e)
			st.guardStopStep(d)
			return d
		}
		allowed.Refs = st.describeRefs(e, g.rp.Commands, reasons)
	} else {
		e := &guardEval{policy: g.policy, def: wrapped}
		if msg := e.checkREST(g.method, g.rest, g.body); msg != "" {
			d := st.refuseGuard(nil, nil, msg, false, e)
			st.enrich(d, def)
			st.guardStopStep(d)
			return d
		}
	}
	st.step("pass", stepCapability, "Merge and approve toggles do not block this request")
	st.step("pass", stepBranch, "The branch rules allow this request")
	return allowed
}

// enrich fills in which rule governed a refused request when phase one could
// only decide without the default branch: it resolves @default through def
// (read-only, best effort) and names the pattern and role per ref (or the rule
// of a REST refusal). The refusal itself is never changed. The proxy does not
// call this; the simulator does, to explain.
func (st *gatewayState) enrich(d *GatewayDecision, def defaultBranchFn) {
	g := st.guard
	if g == nil || d == nil || d.Verdict != GatewayRefused || !g.policy.BranchPolicy.HasDefaultRule() {
		return
	}
	wrapped := func() (string, error) {
		st.defaultUsed = true
		return def()
	}
	e := &guardEval{policy: g.policy, def: wrapped}
	for i := range d.Refs {
		d.Refs[i].Pattern, d.Refs[i].Role = st.describeRule(e, d.Refs[i].Ref, d.Refs[i].Verdict == refVerdictRefused)
	}
	if rt := firstRefusedRule(d.Refs); rt != "" {
		d.Rule = rt
		return
	}
	if d.Kind == "branch_rules" && len(d.Refs) == 0 {
		if branch := quotedBranch(d.Reason); branch != "" {
			if dflt, err := e.defaultBranch(); err == nil {
				if rule, ok := g.policy.BranchPolicy.SelectRule(branch, dflt); ok {
					d.Rule = ruleText(rule.Branches, rule.Role)
				}
			}
		}
	}
}

// quotedBranch returns the branch named in a `branch "x" has role ...`
// refusal ("" otherwise).
func quotedBranch(reason string) string {
	i := strings.Index(reason, "branch \"")
	if i < 0 {
		return ""
	}
	rest := reason[i+len("branch \""):]
	j := strings.Index(rest, "\"")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// detailJSON is the refusal detail stored with an acl_denied log row.
func (d *GatewayDecision) detailJSON() string {
	dd := queries.DenyDetail{Kind: d.Kind, Layer: d.Layer, Rule: d.Rule, Reason: d.Reason, Sees: d.Sees}
	for _, r := range d.Refs {
		dd.Refs = append(dd.Refs, queries.DenyRef(r))
	}
	return dd.Marshal()
}
