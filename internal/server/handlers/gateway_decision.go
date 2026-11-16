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
	Kind    string // one of queries.DenyKinds, or queries.KindCredential (simulator only)
	Layer   string // queries.Layer* when an ACL layer decided
	Rule    string // rule name, "pattern \u2192 role" for a branch rule, tag patterns or capability name
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

// gatewayErrPrefix starts every refusal message the guard sends the agent;
// decisions keep the bare reason and add it once, in guardMessage.
const gatewayErrPrefix = "duckway: "

func guardMessage(reason string) string { return gatewayErrPrefix + reason }

// Stage labels of the decision path (the simulator adds the approval ones).
const (
	stepService    = "Service rules"
	stepCredential = "Credential rules"
	stepToken      = "Token rules"
	stepCapability = "Capability toggles"
	stepBranch     = "Branch rules"
)

// stageOrder is the canonical order of the ACL and guard stages.
var stageOrder = []string{stepService, stepCredential, stepToken, stepCapability, stepBranch}

// layerSteps maps an ACL layer to its stage label.
var layerSteps = map[string]string{
	queries.LayerService: stepService, queries.LayerAPIKey: stepCredential, queries.LayerPlaceholder: stepToken,
}

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
	case queries.LayerService:
		return queries.KindServiceACL
	case queries.LayerAPIKey:
		return queries.KindAPIKeyACL
	}
	return queries.KindTokenACL
}

// guardKind classifies a branch-rule refusal message.
func guardKind(msg string) string {
	switch {
	case msg == msgMergeNotPermitted || msg == msgApproveNotPermitted:
		return queries.KindCapability
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
		return queries.KindUncheckable
	}
	return queries.KindBranchRules
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

// Steps returns the decision path in canonical stage order, every stage not
// reached marked skipped.
func (st *gatewayState) Steps() []DecisionStep {
	by := map[string]DecisionStep{}
	for _, s := range st.steps {
		by[s.Label] = s
	}
	out := make([]DecisionStep, 0, len(stageOrder))
	for _, l := range stageOrder {
		if s, ok := by[l]; ok {
			out = append(out, s)
		} else {
			out = append(out, DecisionStep{Status: queries.StepSkip, Label: l})
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
	d := &GatewayDecision{Verdict: GatewayRefused, Kind: kind, Layer: queries.LayerPlaceholder, Reason: reason, Refs: []DecisionRef{}, logDenied: true}
	rule := ""
	if len(reasons) == 0 {
		rule = st.ruleBehind(kind, reason)
	}
	if strings.EqualFold(st.in.Method, http.MethodPost) && strings.HasSuffix(st.in.Path, ".git/git-receive-pack") {
		d.receive, d.rp, d.reasons, d.fallback, d.drain = true, rp, reasons, reason, drain
		d.Sees = receivePackSees(rp, reasons, reason)
		if rp != nil && len(reasons) == len(rp.Commands) {
			d.Refs = st.describeRefs(e, rp.Commands, reasons)
			rule = firstRefusedRule(d.Refs)
			if rule == "" {
				for _, r := range d.Refs {
					if r.Verdict == refVerdictRefused {
						rule = causeRule(r.Reason)
						break
					}
				}
			}
		}
	} else {
		d.status, d.jsonMsg = http.StatusForbidden, guardMessage(reason)
		d.Sees = jsonErrorBody(d.jsonMsg)
	}
	d.Rule = rule
	return d
}

// ruleBehind names what decided a REST refusal, but only when that is known:
// the governing branch rule (by the quoted branch, without the default branch),
// the tag-create patterns, the capability toggle, or a plain cause such as an
// invalid ref name. It is empty when nothing specific decided it.
func (st *gatewayState) ruleBehind(kind, reason string) string {
	switch {
	case kind == queries.KindCapability:
		return capabilityOf(reason)
	case st.guard != nil && isTagRuleReason(reason):
		return st.tagRuleText()
	case st.guard != nil && kind == queries.KindBranchRules && isBranchRuleReason(reason):
		branch := quotedBranch(reason)
		if rule, matched := st.guard.policy.BranchPolicy.SelectRule(branch, ""); matched && rule.Branches != services.GitHubDefaultBranchToken {
			return ruleText(rule.Branches, rule.Role)
		}
		return ruleText("(no rule)", roleInReason(reason))
	case kind == queries.KindBranchRules:
		return causeRule(reason)
	}
	return ""
}

// A rule-bearing refusal says which rule decided it; every other refusal
// (invalid names, existing tags, other namespaces) is decided by no rule.
func isBranchRuleReason(reason string) bool { return strings.Contains(reason, "\" has role \"") }

func isTagRuleReason(reason string) bool {
	return strings.Contains(reason, "may not be created under the repository's branch rules")
}

func roleInReason(reason string) string {
	const k = "has role \""
	i := strings.Index(reason, k)
	if i < 0 {
		return ""
	}
	rest := reason[i+len(k):]
	if m := strings.Index(rest, "\""); m >= 0 {
		return rest[:m]
	}
	return ""
}

// causeRule names the actual cause of a refusal that no rule decided; "" when
// there is nothing more specific than the reason itself.
func causeRule(reason string) string {
	switch {
	case strings.Contains(reason, "can never be moved or deleted"):
		return "existing tags never move"
	case strings.Contains(reason, "is outside the branches and tags"):
		return "refs outside branches and tags"
	case strings.HasPrefix(reason, "invalid ref"), strings.HasPrefix(reason, "invalid tag name"),
		strings.HasPrefix(reason, "invalid branch name"), strings.HasPrefix(reason, "branch name "),
		strings.HasPrefix(reason, "ref name is longer"), strings.HasPrefix(reason, "tag name is longer"),
		strings.HasPrefix(reason, "empty branch name"):
		return "invalid ref name"
	case strings.Contains(reason, "is neither a branch nor a tag"), strings.Contains(reason, "is not a branch"),
		strings.HasPrefix(reason, "only branches may be changed"), strings.HasPrefix(reason, "workflows may only be dispatched on branches"):
		return "ref type not allowed"
	}
	return ""
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
		r.Pattern, r.Role = st.refRule(e, r)
		out = append(out, r)
	}
	return out
}

// refRule is describeRule, except that a ref refused for a reason no rule
// decided (invalid name, existing tag, other namespace, ref type) names none.
func (st *gatewayState) refRule(e *guardEval, r DecisionRef) (pattern, role string) {
	refused := r.Verdict == refVerdictRefused
	if refused && !isBranchRuleReason(r.Reason) && !isTagRuleReason(r.Reason) {
		return "", ""
	}
	return st.describeRule(e, r.Ref, refused)
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
		label := layerSteps[layer.name]
		if layer.config == "" {
			st.step(queries.StepSkip, label, "No rules configured")
			continue
		}
		res := in.Check(layer.config, st.aclMethod, st.aclPath, st.body)
		if res.NeedsApproval {
			if st.approveLayer == "" {
				st.approveLayer, st.approveRule = layer.name, res.RuleName
			}
			st.step(queries.StepPass, label, "Matched approve rule "+quoteRule(res.RuleName, res.RuleIndex)+": needs an admin's approval")
			continue
		}
		if !res.Allowed {
			st.step(queries.StepStop, label, res.Reason)
			return st.refuseJSON(layerKind(layer.name), layer.name, ruleLabel(res.RuleName, res.RuleIndex), res.Reason,
				http.StatusForbidden, "permission denied ("+layer.name+"): "+res.Reason)
		}
		detail := "Allowed"
		if res.RuleName != "" || res.RuleIndex >= 0 {
			detail = "Allowed by " + quoteRule(res.RuleName, res.RuleIndex)
		}
		st.step(queries.StepPass, label, detail)
	}
	if in.Service != "github" {
		return nil
	}
	g, gerr := newGitHubGuard(in.PermissionConfig, in.Method, in.Path, in.RawQuery)
	if gerr != nil {
		const reason = "the branch policy of this grant is invalid"
		st.step(queries.StepStop, stepBranch, "The branch policy of this grant is invalid")
		d := st.refuseJSON(queries.KindInvalidPolicy, queries.LayerPlaceholder, "", reason, http.StatusForbidden, guardMessage(reason))
		d.internal = gerr
		return d
	}
	st.guard = g
	if g == nil {
		st.step(queries.StepSkip, stepCapability, "No branch rules apply to this request")
		st.step(queries.StepSkip, stepBranch, "No branch rules apply to this request")
		return nil
	}
	body := &in.Body
	switch {
	case g.deny != "":
		st.step(queries.StepStop, stepBranch, g.deny)
		return st.refuseGuard(nil, nil, g.deny, false, nil)
	case g.receivePack:
		if enc := strings.ToLower(strings.TrimSpace(in.ContentEncoding)); enc != "" && enc != "identity" {
			msg := "pushes with a Content-Encoding cannot be checked against branch rules"
			st.step(queries.StepStop, stepBranch, msg)
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
			st.step(queries.StepStop, stepBranch, perr.Error())
			return st.refuseGuard(nil, nil, perr.Error(), !st.inHand, nil)
		}
		g.rp = rp
		if !st.inHand {
			st.streamPrefix = rp.Prefix
		}
	case !g.restOK && g.method != http.MethodGet && g.method != http.MethodHead:
		const reason = "request path cannot be checked against branch rules"
		st.step(queries.StepStop, stepBranch, reason)
		return st.refuseJSON(queries.KindUncheckable, queries.LayerPlaceholder, "", reason, http.StatusForbidden, guardMessage(reason))
	case g.needsJSONBody():
		if !st.inHand && body.Stream != nil && body.ReadAll != nil {
			b, rerr := body.ReadAll(body.Stream)
			if rerr != nil {
				msg := "request body too large or unreadable for branch rule check"
				st.step(queries.StepStop, stepBranch, msg)
				return st.refuseJSON(queries.KindUncheckable, queries.LayerPlaceholder, "", msg, http.StatusRequestEntityTooLarge, msg)
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
		return "\u201c" + l + "\u201d"
	}
	return "a rule"
}

func (st *gatewayState) guardStopStep(d *GatewayDecision) {
	if d.Kind == "capability" {
		st.step(queries.StepStop, stepCapability, d.Reason)
		return
	}
	st.step(queries.StepPass, stepCapability, "Merge and approve toggles do not block this request")
	st.step(queries.StepStop, stepBranch, d.Reason)
}

// Held reports the decision of a request an approve rule gates, once phase
// one has not refused it.
func (st *gatewayState) Held() *GatewayDecision {
	if st.approveLayer == "" {
		return nil
	}
	return &GatewayDecision{
		Verdict: GatewayHeld, Kind: queries.KindApproval, Layer: st.approveLayer, Rule: st.approveRule,
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
	e := &guardEval{policy: g.policy, def: wrapped}
	if g.receivePack {
		reasons := e.checkReceivePack(g.rp.Commands)
		if anyRefused(reasons) {
			d := st.refuseGuard(g.rp, reasons, firstReason(reasons), !st.inHand, e)
			st.guardStopStep(d)
			return d
		}
		allowed.Refs = st.describeRefs(e, g.rp.Commands, reasons)
	} else if msg := e.checkREST(g.method, g.rest, g.body); msg != "" {
		d := st.refuseGuard(nil, nil, msg, false, e)
		st.enrich(d, def)
		st.guardStopStep(d)
		return d
	}
	st.step(queries.StepPass, stepCapability, "Merge and approve toggles do not block this request")
	st.step(queries.StepPass, stepBranch, "The branch rules allow this request")
	return allowed
}

// enrich refines which rule governed a refused request once the default
// branch may be known: it resolves @default through def and names the pattern
// and role per ref (or the rule of a REST refusal). The refusal itself is
// never changed. def must be read-only: the proxy passes a cache-only peek so a
// refusal never waits on the network, the simulator its real (cached) lookup.
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
		d.Refs[i].Pattern, d.Refs[i].Role = st.refRule(e, d.Refs[i])
	}
	if rt := firstRefusedRule(d.Refs); rt != "" {
		d.Rule = rt
		return
	}
	if d.Kind == queries.KindBranchRules && len(d.Refs) == 0 && isBranchRuleReason(d.Reason) {
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
