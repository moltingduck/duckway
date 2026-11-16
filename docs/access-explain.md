# Refusal reasons and access simulator (rlog1116)

Design: console canvas, page "Refusal reasons & simulator". The page has three boards: RefusalLog, AccessSimulator and AccessSimMobile.

User decisions (2026-11-16):
- The simulator is a new tab, "Test access", on the phantom token page.
- Raw request mode is required.
- A body that contains bytes is shown and edited as hex.

All endpoints below are admin-session APIs under `/api/...` (admin auth plus the same-origin CSRF check, like the other admin APIs). This document describes the implemented contract.

## 1. One decision path

The proxy and the simulator reach the same verdict for the same request because both run the same code, `gatewayState` in `internal/server/handlers/gateway_decision.go`:

- `decide()` (phase one, no upstream credential needed): the three ACL layers (service, api_key, placeholder), approve rules, then the GitHub guard: `newGitHubGuard`, the receive-pack parse, the JSON body, and `precheck` (the decision that does not need `@default`).
- `finish(def)` (phase two): the guard's final check, which resolves `@default` through `def`.
- Between the phases the proxy may hold the request for approval; a definite refusal in phase one is decided before an approval rule can hold the request.
- `GatewayDecision` carries `Verdict` (`allowed` | `refused` | `held`), `Kind`, `Layer`, `Rule`, `Reason` (the bare text sent to the agent; `duckway: ` is added once when the response is built), `Refs` (one entry per command of a push) and `Sees` (the exact client-visible body, or for a report-status push the `unpack ok` and `ng` lines, as text).
- The proxy acts on the decision (logs, answers, drains, holds, forwards). Response bytes and status codes are unchanged; `proxy_gateway_golden_test.go` pins them and also passes on the commit before the refactor.
- The decision has no side effects: it never writes the request log, creates approvals, leases or rotates a key, or forwards anything. The only outbound call is the default-branch lookup, which is read-only, cached for 5 minutes and fails closed.
- The proxy counts rate limits (`PermissionChecker.Check`); the simulator uses `Explain`, which never does. A rate-limited rule is therefore reported as allowed.

### Kinds, layers and the deciding rule

`Kind` (`queries.DenyKinds`): `service_acl`, `api_key_acl`, `token_acl`, `capability`, `branch_rules`, `invalid_policy`, `uncheckable`, `approval`.
The simulator adds `credential` (no usable credential: inactive token or key, key resolution failed). It is never stored in the request log because the proxy does not log those as `acl_denied`.

`Layer` is `service`, `api_key` or `placeholder` for ACL refusals and guard refusals (`placeholder`), empty otherwise.

`Rule` says what decided the refusal, and is empty when nothing specific did:
- ACL layers: the rule name, or `rule N` for an unnamed rule.
- Branch rules: `pattern → role` (U+2192), for example `** → read` or `@default → protected`. When no rule matched, `(no rule) → read`.
- Tag refusals: `tag create patterns: v*` (or `(none)`); a ref entry for a tag has role `tag` and the same text as its pattern.
- Capability refusals: the capability name, `pull_requests_merge` or `pull_requests_approve`.
- A refusal no rule decided names a cause, never a rule: `existing tags never move`, `invalid ref name`, `refs outside branches and tags`, `ref type not allowed`. Its refs carry no pattern or role.
- `uncheckable` and `invalid_policy` refusals have no rule.

A refusal decided before the default branch is known (the guard's `precheck`) names the most specific non-`@default` rule that matched, because the refusal holds whether or not the ref is the default branch. Pattern and role of an allowed ref in such a push stay empty when they would depend on the default branch.
The proxy never looks the default branch up just to log a refusal: it reads the 5-minute cache only (`enrich` with a cache peek). With a warm cache the entry is exact (`@default → protected`); with a cold one it is the non-`@default` match. The simulator uses its real, cached lookup.

### Key choice (no leasing)

The simulator evaluates every key the token can resolve to, read-only: the assigned key, or the active members of its key group (`GroupQueries.GetMembers`, at most 20). It never calls the key resolver, so no round-robin index moves and no usage counter changes. If the outcomes differ between keys the response has `per_key`, and the top-level fields describe the most restrictive outcome (refused over held over allowed).

The proxy ignores the key-level ACL and upstream proxy URL of key pool members (`GetMembers` does not select them; a known defect that is not changed here). The simulator mirrors that: for a pool, the "Credential rules" step says the pool key's own rules are not evaluated.
When key resolution fails the simulator reports what the proxy does: an empty or unreadable key group, or a missing key, is a 500 `key resolution failed`; both are kind `credential`, with `sees` equal to the proxy's body.

## 2. Request log reason

Migration (`safeAlters`, portable SQL): `request_log.deny_detail TEXT NOT NULL DEFAULT ''` and `request_log.repeat_count INTEGER NOT NULL DEFAULT 1`. Older rows have an empty detail and a count of 1.

`deny_detail` is written for every `acl_denied` row, including the GitHub refusals decided before the rules (invalid policy, `Content-Encoding`, unparseable receive-pack, body too large, path that cannot be checked). Shape:

```json
{"kind":"branch_rules","layer":"placeholder","rule":"** → read","reason":"…",
 "refs":[{"ref":"refs/heads/x","verdict":"refused","pattern":"**","role":"read","reason":"…"}],
 "sees":"unpack ok\nng refs/heads/x duckway: …\n","more":3}
```

- `kind` is always the first key (the `deny_kind` filter matches that prefix); `refs` is always an array; `more` appears only when refs were dropped.
- At most 20 refs are kept (the rest counted in `more`), the stored JSON is at most 8 KiB, strings are cut on UTF-8 boundaries.
- Agent-supplied strings have control characters and Unicode format characters (category Cf: bidi overrides and isolates, marks, zero-width) replaced by U+FFFD. Line feeds survive only in `sees`.
- `sees` is cut at 2 KiB and then ends in `… (truncated)`.

`repeat_count`: identical denials in the same UTC minute increment the existing row (one `UPDATE`) instead of inserting. Identity is client, phantom token, method, path **and `deny_detail`**: a different reason for the same path (another push to the same repository, say) is its own row, so a row never explains something else. The per-client cap of 30 rows per minute and the suppressed-marker row are unchanged; repeats do not consume the cap.

Logs API (`GET /api/logs`): every item gains `deny_detail` (object or `null`) and `repeat_count` (integer ≥ 1). New filter `deny_kind=<one of queries.DenyKinds>`; an unknown value is a 400. Like the other filters it implies the default 7-day window when no time bound is given. The log never stores a request body.

The logs UI shows the reason (`web/templates/logs.html` and its JS): a "Refused by" filter, a summary sentence, "Decided by" (the rule), per-ref rows for pushes, "What the agent saw" (mono, `pre-line`), "Repeats ×N", and a link "Test this in the simulator" to `/admin/phantom-tokens/{id}?tab=test&method=…&path=…`. All CSP-safe, no innerHTML string building.

## 3. Simulator API

### `POST /api/phantom-tokens/{id}/simulate`

Request:
- Action mode (GitHub tokens only, otherwise 400): `{mode:"action", repo:"owner/name", action, ref, workflow?}`. Actions: `clone`, `push`, `force_or_delete`, `create_branch`, `open_pr_into` (head `agent/sim`), `merge_pr`, `approve_pr`, `create_tag`, `dispatch` (workflow defaults to `ci.yml`), `release`. `ref` is not needed for `clone`, `merge_pr` and `approve_pr`.
  - A push is a receive-pack request with real pkt-line framing (capabilities after NUL on the first command, flush packet, empty pack). `force_or_delete` is a delete command: the proxy cannot tell a forced update from a normal one, and both need the same role.
  - `create_branch` and `create_tag` are `POST /repos/{r}/git/refs`; `dispatch` is `POST …/dispatches {ref}`; the others are the matching REST requests.
- Raw mode (every service): `{mode:"raw", method, path, query?, headers?:{name:value}, body_text? | body_hex?}`.
  - `path` is what the agent sends to the upstream API. A `/proxy/<service>` prefix is accepted and stripped; a `?query` may be inside `path` (not both).
  - `body_hex` takes hex digits with optional whitespace. `body_text` and `body_hex` together are a 400.
  - Only the `Content-Encoding` header changes a decision.
  - Paths are validated like the proxy does: a `#` is rejected (a client never sends a fragment; a literal `#` can only be sent in the query string, as `%23`), `%2f` is rejected, and a path the gateway's mux would clean (`.` or `..` segments, `//`) is a 400 that says the proxy would redirect it before checking it.

Response:

```json
{"verdict":"allowed|refused|held","kind":"","title":"…","layer":"","rule":"","reason":"",
 "steps":[{"status":"pass|stop|skip","label":"…","detail":"…"}],
 "refs":[{"ref":"","verdict":"allowed|refused","pattern":"","role":"","reason":""}],
 "sees":"","depends_on_default_branch":false,
 "request":{"method":"","path":"","query":"","body_text":"","body_hex":"","body_size":0},
 "per_key":[{"key_id":"","key_name":"","verdict":"","kind":"","title":"","layer":"","rule":"","reason":"","sees":""}]}
```

- `kind` is empty when allowed. `request` echoes the evaluated request (for an action, the one it was turned into); `body_hex` is always set, `body_text` only for valid UTF-8. `per_key` appears only when keys disagree.
- Steps are in this fixed order, unreached stages marked `skip`: Token status, Token-wide approval, Service rules, Credential rules, Token rules, Capability toggles, Branch rules, Approval hold.
- `held` (kind `approval`) means: a token-wide approval is missing, or an approve rule matched and nothing refused. It is reported as the production proxy would (an approval store is always present). The simulator cannot know whether an approval for this exact request (scoped to the full body) already exists, so a gating approve rule is always `held`. A request an approve rule gates is held before the branch rules that need `@default` run; the Branch rules step then says so (only when the request has a guard that depends on the default branch).
- `depends_on_default_branch` is true when the repository's rules mention `@default`.
- The synthetic request is never forwarded and nothing is written: no request-log row, approval, usage counter or rate-limit count.
- Outbound: only the default-branch lookup, per candidate credential, with a 15 s overall deadline. A failed lookup is remembered for 60 seconds so a down upstream is not hit again on every simulation. `"refresh": true` in the request body bypasses that remembered failure for this request only (still single-flight and bounded by the deadline).

Errors (`{"error":"…"}`): 400 invalid request (unknown mode/action, bad repo, bad path or headers, invalid hex); invalid hex adds `"offset"`, the 0-based character (rune) index of the first bad character, and the message names the character; 404 unknown token; 413 request or decoded body larger than 16 MiB (the proxy's inspect cap).

### `GET /api/phantom-tokens/{id}/access-summary`

GitHub tokens only (otherwise 400); 422 when the stored permission config is invalid.

```json
{"service":"github","repos":[{"repo":"owner/repo","branch_rules":true,"legacy":false,
 "rules":[{"pattern":"@default","role":"protected","push":false,"create_force_delete":false,"pr_into":true,"run_workflows":false}],
 "tag_patterns":["v*"],"tags":true,"merge":false,"approve":false,"clone":true,
 "push_capability":true,"pull_requests_write":true,"workflow_dispatch":true,
 "ref_allowlist":[],"default_branch":"main"}]}
```

- The rule booleans are already narrowed by what the repo's capabilities open at all. A grant without branch rules has `legacy:true`, no rules and its `ref_allowlist`.
- `default_branch` is set only for repositories whose rules mention `@default`, from the same read-only, cached lookup as the simulator with the token's first active key (4 s per lookup, 8 s for the whole summary; lookups stop once the deadline passes). It is `null` when unknown. Failed lookups are remembered for 60 seconds; `?refresh=1` bypasses that for the request (same deadlines, single-flight).

## 4. Simulator UI ("Test access" tab)

- A new tab on the phantom token page, deep-linkable with `?tab=test`; prefill parameters come from the log link.
- Action mode: repo select (from the access summary), ref input with chips for the token's own patterns, action chips, the verdict card (Allowed / Refused / Held, title, steps, "What the agent would see"), and below it the "What this token can do" table.
- Raw mode: method, path, headers, and a body with a Text/Hex toggle. A body that is not valid UTF-8, or contains control bytes other than `\t\n\r`, opens in Hex; in Text view such bytes show as `\xNN` and Text is read-only for that body. The Hex view shows offset / hex / ASCII rows of 16 bytes and an editable hex textarea. Presets: "JSON: move main with force" and "git push (binary pkt-line)".
- Raw mode works for every service; a non-GitHub token shows only raw mode.
- Usable at 412 px, dark and light, CSP-safe, real buttons and labels, `aria-live` verdict announcements.

## 5. Tests

- Go: golden response bytes; simulator vs proxy table (actions, raw requests, `/proxy/<svc>` prefix, query, mixed-ref and tag pushes, held cases for approve rules and token-wide approval, unknown `@default` failing closed), deciding-rule coverage through both the log path and the simulator, `deny_detail` content, caps and sanitising, `repeat_count` coalescing and the per-client cap, hex parsing, raw path validation, key pools and resolution failures, no side effects, negative caching, migration on SQLite.
- JS: hex view/parse round-trip, UTF-8 detection, read-only Text view for binary.
- E2E: `scripts/e2e-test.sh` section 11f (simulate API; a refused push and its reason in the logs API).
