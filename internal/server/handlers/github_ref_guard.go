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
	"net/url"
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
	if e, ok := c.entries[key]; ok {
		if c.now().Before(e.expires) {
			c.mu.Unlock()
			return e.branch, nil
		}
		delete(c.entries, key)
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
				// Sweep expired entries on insertion so the map stays bounded by
				// the number of live credential+repo pairs.
				now := c.now()
				for k, e := range c.entries {
					if !now.Before(e.expires) {
						delete(c.entries, k)
					}
				}
				c.entries[key] = githubDefaultBranchEntry{branch: branch, expires: now.Add(githubDefaultBranchTTL)}
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
	// deny is a request-independent refusal reason (set when the request can
	// never be checked against the branch rules).
	deny string
	// REST
	method string
	rest   []string
	restOK bool
	body   []byte
	// receive-pack
	receivePack bool
	rp          *receivePackRequest
}

const (
	githubSuffixReceivePack = ".git/git-receive-pack"
	githubSuffixUploadPack  = ".git/git-upload-pack"
	githubSuffixInfoRefs    = ".git/info/refs"
)

// githubRepoAndRest splits a proxied upstream path into the repository and
// the REST segments after it. Git smart-HTTP suffixes are checked before the
// /repos/ prefix, with the same repository split the ACL uses
// (githubRepoFromScopedEndpoint), so `/repos/x.git/git-receive-pack` is the
// receive-pack of repository "repos/x". smartRead marks read-only smart-HTTP
// paths (upload-pack, info/refs), which move no ref.
func githubRepoAndRest(method, upstreamPath string) (repo string, rest []string, receivePack, restOK, smartRead bool) {
	post := strings.EqualFold(method, http.MethodPost)
	// A smart-HTTP path names exactly owner/repo before the suffix; anything
	// with more segments (a contents path that merely ends in ".git/info/refs")
	// is an ordinary REST path and falls through to the /repos/ split below.
	smartRepo := func(suffix string) (string, bool) {
		r := strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(upstreamPath, suffix), "/"))
		parts := strings.Split(r, "/")
		return r, len(parts) == 2 && parts[0] != "" && parts[1] != ""
	}
	switch {
	case strings.HasSuffix(upstreamPath, githubSuffixReceivePack):
		if r, ok := smartRepo(githubSuffixReceivePack); ok {
			if post {
				return r, nil, true, true, false
			}
			// Any other method on a receive-pack path cannot be classified.
			return r, nil, false, false, false
		}
	case strings.HasSuffix(upstreamPath, githubSuffixUploadPack):
		if r, ok := smartRepo(githubSuffixUploadPack); ok {
			return r, nil, false, post, true
		}
	case strings.HasSuffix(upstreamPath, githubSuffixInfoRefs):
		if r, ok := smartRepo(githubSuffixInfoRefs); ok {
			return r, nil, false, strings.EqualFold(method, http.MethodGet), true
		}
	}
	if strings.HasPrefix(upstreamPath, "/repos/") {
		trimmed := strings.TrimSuffix(strings.TrimPrefix(upstreamPath, "/repos/"), "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			return "", nil, false, false, false
		}
		restOK = true
		for _, p := range parts {
			if p == "" || p == "." || p == ".." {
				restOK = false
			}
		}
		return strings.ToLower(parts[0] + "/" + parts[1]), parts[2:], false, restOK, false
	}
	return "", nil, false, false, false
}

func hasMethodOverrideQuery(rawQuery string) bool {
	if rawQuery == "" {
		return false
	}
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return strings.Contains(strings.ToLower(rawQuery), "_method")
	}
	for k := range vals {
		if strings.EqualFold(k, "_method") {
			return true
		}
	}
	return false
}

// newGitHubGuard returns nil when the request needs no branch-rule
// enforcement. A non-nil error means the policy itself is unusable (fail
// closed).
func newGitHubGuard(permissionConfig, method, upstreamPath, rawQuery string) (*githubGuard, error) {
	if strings.TrimSpace(permissionConfig) == "" {
		return nil, nil
	}
	readOnly := strings.EqualFold(method, http.MethodGet) || strings.EqualFold(method, http.MethodHead)
	lower := strings.ToLower(strings.TrimRight(upstreamPath, "/"))
	graphql := lower == "/graphql" && !readOnly
	repositories := lower == "/repositories" || strings.HasPrefix(lower, "/repositories/")
	reposPath := lower == "/repos" || strings.HasPrefix(lower, "/repos/")
	override := hasMethodOverrideQuery(rawQuery)
	repo, rest, receivePack, restOK, smartRead := githubRepoAndRest(method, upstreamPath)
	unresolved := repo == "" && reposPath && !readOnly
	if !graphql && !repositories && !unresolved {
		if repo == "" || (readOnly && !receivePack && !override) {
			return nil, nil
		}
	}
	policies, err := services.ParseGitHubBranchPolicies(permissionConfig)
	if err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		return nil, nil
	}
	switch {
	case graphql:
		return &githubGuard{deny: "GraphQL writes cannot be checked against branch rules"}, nil
	case repositories:
		return &githubGuard{deny: "repository-id paths cannot be checked against branch rules"}, nil
	case unresolved:
		// A write under /repos/ that does not resolve to owner/repo cannot be
		// attributed to a policy: fail closed.
		return &githubGuard{deny: "request path cannot be checked against branch rules"}, nil
	}
	pol, ok := policies[repo]
	if !ok {
		return nil, nil
	}
	if override {
		return &githubGuard{repo: repo, policy: pol, deny: "method override parameters are not allowed under branch rules"}, nil
	}
	if smartRead {
		if !restOK {
			return &githubGuard{repo: repo, policy: pol, deny: "request cannot be checked against branch rules"}, nil
		}
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

// safeUnclassifiedWrite is the explicit list of writes that move no ref and
// open nothing the rules cover, allowed under a branch policy although the
// table in the spec does not classify them. Every other write is refused.
func safeUnclassifiedWrite(method string, rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	seg := func(i int) string {
		if i < len(rest) {
			return strings.ToLower(rest[i])
		}
		return ""
	}
	switch seg(0) {
	case "issues":
		return true
	case "pulls":
		// merge and update-branch are classified (PUT) or refused (anything else);
		// dismissing a review is refused.
		if seg(2) == "merge" || seg(2) == "update-branch" {
			return false
		}
		return seg(2) != "reviews" || seg(4) != "dismissals"
	case "git":
		if method != http.MethodPost || len(rest) != 2 {
			return false
		}
		switch seg(1) {
		case "blobs", "trees", "commits", "tags":
			return true
		}
	case "releases":
		switch {
		case method == http.MethodPost && len(rest) == 3 && seg(2) == "assets" && seg(1) != "assets":
			return true // upload an asset
		case (method == http.MethodPatch || method == http.MethodDelete) && len(rest) == 3 && seg(1) == "assets":
			return true
		case method == http.MethodPost && len(rest) == 2 && seg(1) == "generate-notes":
			return true
		case method == http.MethodDelete && len(rest) == 2 && seg(1) != "assets" && seg(1) != "generate-notes":
			return true
		}
	case "actions":
		switch seg(1) {
		case "runs":
			switch {
			case method == http.MethodDelete && len(rest) == 3:
				return true
			case method == http.MethodPost && len(rest) == 4:
				switch seg(3) {
				case "rerun", "rerun-failed-jobs", "cancel", "force-cancel":
					return true
				}
			case method == http.MethodDelete && len(rest) == 4 && seg(3) == "logs":
				return true
			}
		case "jobs":
			return method == http.MethodPost && len(rest) == 4 && seg(3) == "rerun"
		case "artifacts":
			return method == http.MethodDelete && len(rest) == 3
		}
	}
	return false
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
	// unknownDefault makes checkBranch decide without the default branch: a
	// candidate is refused when it is refused whether or not it is the default
	// branch, allowed when it is allowed either way, and otherwise undecided.
	unknownDefault bool
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

// ruleOf returns the rule governing one branch name.
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

// maxGitHubRefNameBytes bounds any branch, tag, ref or dispatch name before
// it reaches the matcher (git itself limits ref names to 255-byte components;
// GitHub refuses anything near this size).
const maxGitHubRefNameBytes = services.MaxGitHubRefNameBytes

// invalidBranchName rejects names that GitHub could resolve to something other
// than the branch the rules were evaluated for.
func invalidBranchName(name string) string {
	if prob := services.GitHubRefNameProblem(name, false); prob != "" {
		return fmt.Sprintf("branch name %s", prob)
	}
	for _, p := range []string{"heads/", "tags/", "remotes/"} {
		if strings.HasPrefix(name, p) {
			return fmt.Sprintf("branch name %q is ambiguous (starts with %q)", name, p)
		}
	}
	return ""
}

// branchCandidates returns the branch names a client-supplied branch field may
// resolve to; every one of them must pass.
func branchCandidates(name string) ([]string, string) {
	if len(name) > maxGitHubRefNameBytes {
		return nil, fmt.Sprintf("branch name is longer than %d bytes", maxGitHubRefNameBytes)
	}
	if name == "" {
		return nil, "empty branch name"
	}
	if strings.ContainsAny(name, "\x00\n") {
		return nil, "invalid branch name"
	}
	if strings.HasPrefix(name, "refs/heads/") {
		stripped := strings.TrimPrefix(name, "refs/heads/")
		if bad := invalidBranchName(stripped); bad != "" {
			return nil, bad
		}
		return []string{name, stripped}, ""
	}
	if strings.HasPrefix(name, "refs/") {
		return nil, fmt.Sprintf("ref %q is not a branch", name)
	}
	if bad := invalidBranchName(name); bad != "" {
		return nil, bad
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
		if e.unknownDefault && e.policy.BranchPolicy.HasDefaultRule() {
			asOther, okOther := e.policy.BranchPolicy.SelectRule(c, "")
			roleOther := services.GitHubRoleRead
			if okOther {
				roleOther = asOther.Role
			}
			asDef, okDef := e.policy.BranchPolicy.SelectRule(c, c)
			roleDef := services.GitHubRoleRead
			if okDef {
				roleDef = asDef.Role
			}
			passOther, passDef := ok(roleOther, asOther), ok(roleDef, asDef)
			switch {
			case !passOther && !passDef:
				return fmt.Sprintf("branch %q has role %q under the repository's branch rules; %s", c, roleOther, what)
			case passOther && passDef:
				continue
			}
			e.have, e.err = true, errDefaultDeferred
			return "the repository's default branch could not be determined, so the request was refused"
		}
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
	if len(tag) > maxGitHubRefNameBytes {
		return fmt.Sprintf("tag name is longer than %d bytes", maxGitHubRefNameBytes)
	}
	tag = strings.TrimPrefix(tag, "refs/tags/")
	if services.GitHubRefNameProblem(tag, false) != "" {
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
		if method != http.MethodGet && method != http.MethodHead && !safeUnclassifiedWrite(method, rest) {
			return "endpoint not allowed under branch rules"
		}
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
	if len(ref) > maxGitHubRefNameBytes {
		return fmt.Sprintf("ref name is longer than %d bytes (%d)", maxGitHubRefNameBytes, len(ref))
	}
	if prob := services.GitHubRefNameProblem(ref, false); prob != "" {
		return fmt.Sprintf("invalid ref %q: it %s", ref, prob)
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

var errDefaultDeferred = errors.New("default branch lookup deferred")

// precheck runs the guard decision without the default-branch lookup. definite
// is true when the request is refused whatever the default branch is; reasons
// are per command for receive-pack. A decision that would need @default is not
// definite (the full check runs later, once the upstream credential exists).
func (g *githubGuard) precheck() (definite bool, msg string, reasons []string) {
	e := &guardEval{policy: g.policy, unknownDefault: true, def: func() (string, error) { return "", errDefaultDeferred }}
	if g.receivePack {
		reasons = make([]string, len(g.rp.Commands))
		for i, c := range g.rp.Commands {
			e.have, e.err = false, nil
			r := e.checkPushCommand(c)
			switch {
			case r == "":
			case e.err != nil && errors.Is(e.err, errDefaultDeferred):
				// Undecided: only the refusals of other commands matter.
			default:
				reasons[i] = r
				definite = true
			}
		}
		if definite {
			return true, firstReason(reasons), reasons
		}
		return false, "", nil
	}
	m := e.checkREST(g.method, g.rest, g.body)
	if m != "" && (!e.have || !errors.Is(e.err, errDefaultDeferred)) {
		return true, m, nil
	}
	return false, "", nil
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
