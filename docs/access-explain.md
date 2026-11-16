# Refusal reasons and access simulator (rlog1116)

Design: console canvas, page "Refusal reasons & simulator". The page has three boards: RefusalLog, AccessSimulator and AccessSimMobile.

User decisions (2026-11-16):
- The simulator is a new tab, "Test access", on the phantom token page.
- Raw request mode is required.
- A body that contains bytes is shown and edited as hex.

## 1. One decision path

The proxy and the simulator must reach the same verdict for the same request. So that they cannot drift apart:

- Move the gateway decision in `internal/server/handlers/proxy.go` into one function that has no side effects. It covers:
  - token-wide approval
  - the three ACL layers (service, api_key, placeholder)
  - approve rules
  - the GitHub branch guard: `newGitHubGuard`, the receive-pack parse, the JSON body, `precheck`, and the final check that needs `@default`
- The function returns a `GatewayDecision`:
  - `Verdict`: `allowed` | `refused` | `held`
  - `Kind`: `service_acl` | `api_key_acl` | `token_acl` | `capability` | `branch_rules` | `invalid_policy` | `uncheckable` | `approval`
  - `Layer`
  - `Rule`: the rule name, or a branch pattern plus role
  - `Reason`: the text sent to the agent today
  - `Refs`: for receive-pack, one entry per command: ref, verdict, matching pattern, role and reason
  - `Sees`: the exact client-visible response body or the report-status ng lines, as text
- The proxy calls this function and then does what it does today. It logs, writes the response, drains the body, holds for approval or forwards.
- Response bytes and status codes stay exactly as they are now. Existing tests are the regression net, and new tests must assert that the bytes are unchanged.
- No side effects in the decision:
  - It never creates approvals or writes to the request log.
  - It never forwards anything or consumes a key lease or rotation.
  - The one exception is the default-branch lookup. It is read-only, already cached for 5 minutes, and fails closed.
  - Key choice: the simulator evaluates the API-key ACL of every key the placeholder can resolve to, without leasing. If the outcome differs between keys, it reports the result for each key.

## 2. Request log reason

- Add one additive column: `request_log.deny_detail TEXT NOT NULL DEFAULT ''`. It holds JSON: `{kind, layer, rule, reason, refs[], sees}`.
  - Cap the stored JSON at 8 KiB. Keep the first 20 refs and add `"more": N` for the rest.
  - Truncate strings on UTF-8 boundaries.
  - Replace control characters, because ref names come from the agent.
- Add `repeat_count INTEGER NOT NULL DEFAULT 1`. A denial coalesced within the same minute increments this count on the existing row (one UPDATE). Today such repeats are dropped silently.
  - Keep the per-client cap of 30 rows per minute and the suppressed-marker behaviour.
- Add both columns through `safeAlters`. Use portable SQL that works on SQLite and Postgres. Older rows have an empty detail.
- Fill `deny_detail` on every `acl_denied` row, including the GitHub refusal paths that exist today:
  - invalid policy
  - Content-Encoding
  - unparseable receive-pack
  - body too large
  - path that cannot be checked
- Show the reason in the logs UI (`web/templates/logs.html` and its JS):
  - A "Refused by" filter on kind, as a server-side filter param.
  - In the detail panel:
    - a summary sentence
    - the "Decided by" rule
    - per-ref rows for pushes
    - "What the agent saw", in mono, `pre-line`
    - "Repeats ×N"
    - a link "Test this in the simulator" to `/admin/phantom-tokens/{id}?tab=test&method=…&path=…`
  - The log never stores a body, so the link carries method and path only.
- Render all of it CSP-safe: no innerHTML string building. Follow the existing DW helpers and lint.

## 3. Simulator API

- `POST /admin/api/phantom-tokens/{id}/simulate` uses admin auth and CSRF exactly like the other admin APIs on that page.
- Request:
  - Action mode: `{mode:"action", repo, action, ref, workflow?}`. Actions:
    - `clone`
    - `push`
    - `force_or_delete`
    - `create_branch`
    - `open_pr_into` (with head `agent/sim`)
    - `merge_pr`
    - `approve_pr`
    - `create_tag`
    - `dispatch`
    - `release`
  - The server turns an action into the same synthetic HTTP request the agent would send. A push becomes a receive-pack body built with real pkt-line framing. Dispatch becomes `POST .../dispatches {ref}`. The request then goes through §1.
  - Raw mode: `{mode:"raw", method, path, query?, headers?: {name: value}, body_text? | body_hex?}`.
    - `body_hex` accepts hex digits with optional whitespace.
    - Odd length or a non-hex character returns a 400 that gives the offset.
    - The decoded body is capped at the proxy's inspect cap (16 MiB).
- Response: `{verdict, kind, title, steps:[{status:"pass"|"stop"|"skip", label, detail}], refs[], sees, depends_on_default_branch: bool, per_key?: [...]}`. Steps are generated from the decision path in order:
  - token-wide approval
  - each ACL layer
  - capability
  - branch rule
  - approval hold
- For GitHub, also return `GET /admin/api/phantom-tokens/{id}/access-summary`. It gives, per repo, the rule rows (pattern, role, push, create/force/delete, PR into, run workflows) and the repo flags (tags, merge, approve, clone).
- Simulations are never written to the request log.

## 4. Simulator UI ("Test access" tab)

- Add a new tab to the phantom token page. Its state is deep-linkable with `?tab=test`, and prefill parameters come from the log link.
- Action mode:
  - A repo select listing the token's GitHub repos.
  - A ref input with chips for the token's own patterns and examples.
  - Action chips.
  - The verdict card: Allowed / Refused / Held, the title, the steps, and "What the agent would see".
  - Below it, the "What this token can do" table.
- Raw mode: method, path, headers, and a body with a Text/Hex toggle.
  - A body that is not valid UTF-8, or that contains control bytes other than `\t\n\r`, opens in Hex. In Text view such bytes are shown as `\xNN`, and the Text view is read-only for that body: editing happens in Hex.
  - The Hex view shows offset / hex / ASCII rows, 16 bytes each, and an editable hex textarea.
  - Two example presets: "JSON: move main with force" and "git push (binary pkt-line)".
- Raw mode works for every service. A non-GitHub token shows only raw mode.
- Must be usable at 412 px (Pixel 10), in dark and light themes, CSP-safe, with real buttons and labels, and with live verdict announcements (`aria-live`).

## 5. Tests

- Go:
  - Decision equivalence: proxy response bytes are unchanged, using the existing guard and ACL tests plus new golden cases.
  - Simulator verdicts match the proxy for a table of requests, including receive-pack with mixed refs, dispatch, tag, merge toggle, approve rules, and a non-GitHub raw request.
  - `deny_detail` content and caps, `repeat_count` coalescing, and the per-client cap still holds.
  - Hex parsing errors.
  - No side effects: no approvals, no log rows, no key lease.
  - Migration on SQLite. Postgres too if the existing harness allows.
- JS: hex view/parse round-trip, UTF-8 detection, and the read-only Text view for binary.
- E2E: `scripts/e2e-test.sh` additions for the simulate API and a refused push showing its reason in the logs API.
