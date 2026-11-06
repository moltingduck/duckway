package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

// Branch rule enforcement for the GitHub service (docs/github-branch-rules.md
// section 3). It runs after the ACL allow decision and only for repositories
// that carry a branch policy.

const githubDefaultBranchTTL = 5 * time.Minute

// GitHubDefaultBranchRequest describes one GET /repos/{owner}/{repo} lookup.
// CredentialID identifies the credential (never the secret) for caching.
type GitHubDefaultBranchRequest struct {
	Client       *http.Client
	APIBase      string
	Repo         string
	CredentialID string
	AuthType     string
	AuthHeader   string
	AuthPrefix   string
	Key          string
}

// GitHubDefaultBranchFetcher resolves a repository's default branch.
type GitHubDefaultBranchFetcher func(ctx context.Context, req GitHubDefaultBranchRequest) (string, error)

func fetchGitHubDefaultBranch(ctx context.Context, req GitHubDefaultBranchRequest) (string, error) {
	if req.Client == nil {
		return "", errors.New("no http client")
	}
	target := strings.TrimRight(req.APIBase, "/") + "/repos/" + req.Repo
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	hr.Header.Set("Accept", "application/vnd.github+json")
	hr.Header.Set("User-Agent", "duckway")
	switch req.AuthType {
	case "header":
		hr.Header.Set(req.AuthHeader, req.Key)
	case "query":
		q := hr.URL.Query()
		q.Set(req.AuthHeader, req.Key)
		hr.URL.RawQuery = q.Encode()
	default:
		header, prefix := req.AuthHeader, req.AuthPrefix
		if header == "" {
			header, prefix = "Authorization", "Bearer "
		}
		hr.Header.Set(header, prefix+req.Key)
	}
	resp, err := req.Client.Do(hr)
	if err != nil {
		return "", errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return "", fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	var doc struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&doc); err != nil {
		return "", errors.New("unreadable response")
	}
	if doc.DefaultBranch == "" {
		return "", errors.New("response has no default_branch")
	}
	return doc.DefaultBranch, nil
}

type githubDefaultBranchEntry struct {
	branch  string
	expires time.Time
}

type githubDefaultBranchCall struct {
	done   chan struct{}
	branch string
	err    error
}

// githubDefaultBranchCache caches successful lookups per credential+repo and
// shares one in-flight lookup between concurrent callers. Failures are never
// cached.
type githubDefaultBranchCache struct {
	mu      sync.Mutex
	entries map[string]githubDefaultBranchEntry
	calls   map[string]*githubDefaultBranchCall
	now     func() time.Time
}

func newGitHubDefaultBranchCache() *githubDefaultBranchCache {
	return &githubDefaultBranchCache{
		entries: map[string]githubDefaultBranchEntry{},
		calls:   map[string]*githubDefaultBranchCall{},
		now:     time.Now,
	}
}

func (c *githubDefaultBranchCache) get(ctx context.Context, key string, fetch func(context.Context) (string, error)) (string, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return e.branch, nil
	}
	call, inflight := c.calls[key]
	if !inflight {
		call = &githubDefaultBranchCall{done: make(chan struct{})}
		c.calls[key] = call
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			branch, err := fetch(fctx)
			c.mu.Lock()
			call.branch, call.err = branch, err
			if err == nil && branch != "" {
				c.entries[key] = githubDefaultBranchEntry{branch: branch, expires: c.now().Add(githubDefaultBranchTTL)}
			}
			delete(c.calls, key)
			c.mu.Unlock()
			close(call.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-call.done:
		return call.branch, call.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// WithGitHubDefaultBranchFetcher replaces the upstream default-branch lookup
// (tests). The cache in front of it stays in place.
func (h *ProxyHandler) WithGitHubDefaultBranchFetcher(f GitHubDefaultBranchFetcher) *ProxyHandler {
	h.githubDefaultFetch = f
	return h
}

func (h *ProxyHandler) githubDefaultBranchFor(ctx context.Context, req GitHubDefaultBranchRequest) (string, error) {
	fetch := h.githubDefaultFetch
	if fetch == nil {
		fetch = fetchGitHubDefaultBranch
	}
	h.githubDefaultOnce.Do(func() { h.githubDefaultCache = newGitHubDefaultBranchCache() })
	sum := sha256.Sum256([]byte(req.CredentialID))
	key := hex.EncodeToString(sum[:]) + "|" + strings.ToLower(req.Repo)
	return h.githubDefaultCache.get(ctx, key, func(c context.Context) (string, error) { return fetch(c, req) })
}

// githubGuard is the per-request enforcement state.
type githubGuard struct {
	repo   string // lower-case owner/repo
	policy services.GitHubRepoBranchPolicy
	// REST
	method string
	rest   []string
	restOK bool
	body   []byte
	// receive-pack
	receivePack bool
	rp          *receivePackRequest
}

// githubRepoAndRest splits a proxied upstream path into the repository and
// the REST segments after it, or flags smart-HTTP receive-pack.
func githubRepoAndRest(method, upstreamPath string) (repo string, rest []string, receivePack, restOK bool) {
	if strings.HasPrefix(upstreamPath, "/repos/") {
		trimmed := strings.TrimSuffix(strings.TrimPrefix(upstreamPath, "/repos/"), "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			return "", nil, false, false
		}
		restOK = true
		for _, p := range parts {
			if p == "" || p == "." || p == ".." {
				restOK = false
			}
		}
		return strings.ToLower(parts[0] + "/" + parts[1]), parts[2:], false, restOK
	}
	if strings.EqualFold(method, http.MethodPost) && strings.HasSuffix(upstreamPath, ".git/git-receive-pack") {
		repo := strings.TrimPrefix(strings.TrimSuffix(upstreamPath, ".git/git-receive-pack"), "/")
		if strings.Count(repo, "/") == 1 {
			return strings.ToLower(repo), nil, true, true
		}
	}
	return "", nil, false, false
}

// newGitHubGuard returns nil when the request needs no branch-rule
// enforcement. A non-nil error means the policy itself is unusable (fail
// closed).
func newGitHubGuard(permissionConfig, method, upstreamPath string) (*githubGuard, error) {
	if strings.TrimSpace(permissionConfig) == "" {
		return nil, nil
	}
	repo, rest, receivePack, restOK := githubRepoAndRest(method, upstreamPath)
	if repo == "" {
		return nil, nil
	}
	if !receivePack && (method == http.MethodGet || method == http.MethodHead) {
		return nil, nil
	}
	policies, err := services.GitHubBranchPoliciesFromConfig(permissionConfig)
	if err != nil {
		return nil, err
	}
	pol, ok := policies[repo]
	if !ok {
		return nil, nil
	}
	return &githubGuard{repo: repo, policy: pol, method: strings.ToUpper(method), rest: rest, restOK: restOK, receivePack: receivePack}, nil
}

type githubRESTOp int

const (
	opNone githubRESTOp = iota
	opRefCreate
	opRefWrite // PATCH / DELETE on a ref path
	opRefOther // any other write shape under git/refs: refused
	opContents
	opMerges
	opReleaseCreate
	opReleaseUpdate
	opPullCreate
	opPullUpdate
	opPullMerge
	opReview
	opReviewEvent
	opDispatch
)

func (op githubRESTOp) needsBody() bool {
	switch op {
	case opRefCreate, opContents, opMerges, opReleaseCreate, opReleaseUpdate,
		opPullCreate, opPullUpdate, opReview, opReviewEvent, opDispatch:
		return true
	}
	return false
}

func classifyGitHubREST(method string, rest []string) githubRESTOp {
	if len(rest) == 0 {
		return opNone
	}
	is := func(i int, s string) bool { return i < len(rest) && strings.EqualFold(rest[i], s) }
	switch strings.ToLower(rest[0]) {
	case "git":
		if is(1, "refs") && method != http.MethodGet && method != http.MethodHead {
			switch {
			case method == http.MethodPost && len(rest) == 2:
				return opRefCreate
			case (method == http.MethodPatch || method == http.MethodDelete) && len(rest) >= 3:
				return opRefWrite
			default:
				return opRefOther
			}
		}
	case "contents":
		if (method == http.MethodPut || method == http.MethodDelete) && len(rest) >= 2 {
			return opContents
		}
	case "merges":
		if method == http.MethodPost && len(rest) == 1 {
			return opMerges
		}
	case "releases":
		if method == http.MethodPost && len(rest) == 1 {
			return opReleaseCreate
		}
		if method == http.MethodPatch && len(rest) == 2 {
			return opReleaseUpdate
		}
	case "pulls":
		switch {
		case method == http.MethodPost && len(rest) == 1:
			return opPullCreate
		case method == http.MethodPatch && len(rest) == 2:
			return opPullUpdate
		case method == http.MethodPut && len(rest) == 3 && (is(2, "merge") || is(2, "update-branch")):
			return opPullMerge
		case method == http.MethodPost && len(rest) == 3 && is(2, "reviews"):
			return opReview
		case method == http.MethodPost && len(rest) == 5 && is(2, "reviews") && is(4, "events"):
			return opReviewEvent
		}
	case "actions":
		if method == http.MethodPost && len(rest) == 4 && is(1, "workflows") && is(3, "dispatches") {
			return opDispatch
		}
	}
	return opNone
}

// needsJSONBody reports whether the guard must read a JSON request body.
func (g *githubGuard) needsJSONBody() bool {
	return !g.receivePack && g.restOK && classifyGitHubREST(g.method, g.rest).needsBody()
}

type defaultBranchFn func() (string, error)

type guardEval struct {
	policy services.GitHubRepoBranchPolicy
	def    defaultBranchFn
	cached string
	err    error
	have   bool
}

func (e *guardEval) defaultBranch() (string, error) {
	if !e.have {
		e.cached, e.err = e.def()
		if e.err == nil && e.cached == "" {
			e.err = errors.New("empty default branch")
		}
		e.have = true
	}
	if e.err != nil {
		return "", fmt.Errorf("the repository's default branch could not be determined, so the request was refused")
	}
	return e.cached, nil
}

// roleOf returns the rule governing one branch name.
func (e *guardEval) ruleOf(branch string) (services.GitHubBranchRule, string, error) {
	def := ""
	if e.policy.BranchPolicy.HasDefaultRule() {
		d, err := e.defaultBranch()
		if err != nil {
			return services.GitHubBranchRule{}, "", err
		}
		def = d
	}
	rule, matched := e.policy.BranchPolicy.SelectRule(branch, def)
	if !matched {
		return rule, services.GitHubRoleRead, nil
	}
	return rule, rule.Role, nil
}

// branchCandidates returns the branch names a client-supplied branch field may
// resolve to; every one of them must pass.
func branchCandidates(name string) ([]string, string) {
	if name == "" {
		return nil, "empty branch name"
	}
	if strings.ContainsAny(name, "\x00\n") {
		return nil, "invalid branch name"
	}
	if strings.HasPrefix(name, "refs/heads/") {
		stripped := strings.TrimPrefix(name, "refs/heads/")
		if stripped == "" {
			return nil, "empty branch name"
		}
		return []string{name, stripped}, ""
	}
	if strings.HasPrefix(name, "refs/") {
		return nil, fmt.Sprintf("ref %q is not a branch", name)
	}
	return []string{name}, ""
}

// checkBranch requires every candidate of name to satisfy ok; what names the
// requirement in the refusal.
func (e *guardEval) checkBranch(name, what string, ok func(role string, rule services.GitHubBranchRule) bool) string {
	cands, bad := branchCandidates(name)
	if bad != "" {
		return bad
	}
	for _, c := range cands {
		rule, role, err := e.ruleOf(c)
		if err != nil {
			return err.Error()
		}
		if !ok(role, rule) {
			return fmt.Sprintf("branch %q has role %q under the repository's branch rules; %s", c, role, what)
		}
	}
	return ""
}

func (e *guardEval) requireAgent(name string) string {
	return e.checkBranch(name, "writing to it requires the agent role", func(role string, _ services.GitHubBranchRule) bool {
		return services.GitHubRoleCanPush(role)
	})
}

func (e *guardEval) requireBase(name string) string {
	return e.checkBranch(name, "pull requests may only target protected or agent branches", func(role string, _ services.GitHubBranchRule) bool {
		return services.GitHubRoleCanBeBase(role)
	})
}

func (e *guardEval) requireDefaultAgent() string {
	d, err := e.defaultBranch()
	if err != nil {
		return err.Error()
	}
	return e.requireAgent(d)
}

func (e *guardEval) requireTag(tag string) string {
	tag = strings.TrimPrefix(tag, "refs/tags/")
	if tag == "" || strings.ContainsAny(tag, "\x00\n") {
		return "invalid tag name"
	}
	if !e.policy.BranchPolicy.TagCreateAllowed(tag) {
		return fmt.Sprintf("tag %q may not be created under the repository's branch rules", tag)
	}
	return ""
}

func parseJSONObject(body []byte) (map[string]interface{}, bool) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// strField reads an optional string field; null counts as absent.
func strField(m map[string]interface{}, key string) (val string, present, ok bool) {
	v, exists := m[key]
	if !exists || v == nil {
		return "", false, true
	}
	s, isStr := v.(string)
	if !isStr {
		return "", true, false
	}
	return s, true, true
}

// checkREST evaluates a REST request; "" means allowed.
func (e *guardEval) checkREST(method string, rest []string, body []byte) string {
	op := classifyGitHubREST(method, rest)
	if op == opNone {
		return ""
	}
	if op == opRefOther {
		return "unsupported write request on git refs"
	}
	if op == opRefWrite {
		segs := rest[2:]
		if strings.EqualFold(segs[0], "refs") {
			segs = segs[1:]
		}
		if len(segs) < 2 {
			return "ref path cannot be determined"
		}
		switch strings.ToLower(segs[0]) {
		case "heads":
			return e.requireAgent("refs/heads/" + strings.Join(segs[1:], "/"))
		case "tags":
			return "existing tags can never be moved or deleted"
		}
		return "only branches may be changed through git refs"
	}
	if op == opPullMerge {
		if !e.policy.PullRequestsMerge {
			return "merging pull requests is not permitted for this grant"
		}
		return ""
	}
	var m map[string]interface{}
	var ok bool
	if len(bytes.TrimSpace(body)) == 0 && op == opReview {
		m, ok = map[string]interface{}{}, true
	} else {
		m, ok = parseJSONObject(body)
	}
	if !ok {
		return "request body must be a JSON object"
	}
	field := func(key string) (string, bool, string) {
		s, present, fine := strField(m, key)
		if !fine {
			return "", false, fmt.Sprintf("field %q must be a string", key)
		}
		return s, present, ""
	}
	switch op {
	case opRefCreate:
		ref, present, msg := field("ref")
		if msg != "" {
			return msg
		}
		switch {
		case !present:
			return "field \"ref\" is required"
		case strings.HasPrefix(ref, "refs/heads/"):
			return e.requireAgent(ref)
		case strings.HasPrefix(ref, "refs/tags/"):
			return e.requireTag(ref)
		}
		return fmt.Sprintf("ref %q is neither a branch nor a tag", ref)
	case opContents:
		branch, present, msg := field("branch")
		if msg != "" {
			return msg
		}
		if !present {
			return e.requireDefaultAgent()
		}
		return e.requireAgent(branch)
	case opMerges:
		base, present, msg := field("base")
		if msg != "" {
			return msg
		}
		if !present {
			return "field \"base\" is required"
		}
		return e.requireAgent(base)
	case opReleaseCreate, opReleaseUpdate:
		tag, present, msg := field("tag_name")
		if msg != "" {
			return msg
		}
		if !present {
			if op == opReleaseUpdate {
				return ""
			}
			return "field \"tag_name\" is required"
		}
		return e.requireTag(tag)
	case opPullCreate, opPullUpdate:
		base, present, msg := field("base")
		if msg != "" {
			return msg
		}
		if !present {
			if op == opPullUpdate {
				return ""
			}
			return "field \"base\" is required"
		}
		return e.requireBase(base)
	case opReview, opReviewEvent:
		event, present, msg := field("event")
		if msg != "" {
			return msg
		}
		if op == opReviewEvent && !present {
			return "field \"event\" is required"
		}
		if present && strings.EqualFold(strings.TrimSpace(event), "APPROVE") && !e.policy.PullRequestsApprove {
			return "submitting approving reviews is not permitted for this grant"
		}
		return ""
	case opDispatch:
		ref, present, msg := field("ref")
		if msg != "" {
			return msg
		}
		if !present {
			return "field \"ref\" is required"
		}
		if strings.HasPrefix(ref, "refs/tags/") {
			return "workflows may only be dispatched on branches"
		}
		return e.checkBranch(ref, "it is not a branch the rules allow to run workflows", func(_ string, rule services.GitHubBranchRule) bool {
			return rule.RunWorkflows
		})
	}
	return ""
}

// checkReceivePack returns one reason per command ("" = allowed).
func (e *guardEval) checkReceivePack(cmds []receivePackCommand) []string {
	reasons := make([]string, len(cmds))
	for i, c := range cmds {
		reasons[i] = e.checkPushCommand(c)
	}
	return reasons
}

func (e *guardEval) checkPushCommand(c receivePackCommand) string {
	ref := c.Ref
	if strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.ContainsAny(ref, "\x00\n") {
		return fmt.Sprintf("invalid ref %q", ref)
	}
	switch {
	case strings.HasPrefix(ref, "refs/heads/") && len(ref) > len("refs/heads/"):
		return e.requireAgent(ref)
	case strings.HasPrefix(ref, "refs/tags/") && len(ref) > len("refs/tags/"):
		if !c.isCreate() {
			return fmt.Sprintf("existing tag %q can never be moved or deleted", ref)
		}
		return e.requireTag(ref)
	}
	return fmt.Sprintf("ref %q is outside the branches and tags this grant may change", ref)
}

func anyRefused(reasons []string) bool {
	for _, r := range reasons {
		if r != "" {
			return true
		}
	}
	return false
}

// evaluateGitHubGuardREST is the pure REST decision (tests call it directly).
func evaluateGitHubGuardREST(pol services.GitHubRepoBranchPolicy, def defaultBranchFn, method string, rest []string, body []byte) string {
	e := &guardEval{policy: pol, def: def}
	return e.checkREST(strings.ToUpper(method), rest, body)
}

func githubDefaultCredentialID(app *githubAppCredential, realKey string) string {
	if app != nil {
		return fmt.Sprintf("app:%d:%d", app.AppID, app.InstallationID)
	}
	return "key:" + realKey
}
