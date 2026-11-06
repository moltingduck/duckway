# GitHub branch rules

Status: agreed design, batch `brr1106` (2026-11-06). Design canvas: the
"Repo ref permissions" page of the console redesign canvas, boards
`RefEffects` (inventory) and `RefBranchRules` (target UI).

Branch rules replace the free-text `ref_allowlist` with branch-protection-style
roles. They narrow what an already-opened GitHub endpoint may do to a ref; the
capability checkboxes still decide which endpoints are open at all.

## 1. Model

Per repository, optional object `branch_policy`:

```json
"branch_policy": {
  "rules": [
    {"branches": "@default", "role": "protected", "run_workflows": false},
    {"branches": "agent/**", "role": "agent",     "run_workflows": false},
    {"branches": "**",       "role": "read",      "run_workflows": false}
  ],
  "tags": {"create": ["v*"]}
}
```

- `role`: `read` | `protected` | `agent`.
- `branches`: `@default` (the repository's current default branch), or a branch
  glob. Input may be written `refs/heads/x` and is stored without the prefix.
  `*` matches within one `/` segment, `**` matches any characters including
  `/`. Allowed characters: those valid in git branch names (so `?`, `[`, `\\`,
  spaces and control characters are rejected);
  reject empty patterns, `..`, leading `/`, and duplicates.
- `run_workflows`: may start workflows (`workflow_dispatch`) on matching
  branches. Requires the `workflow_dispatch` capability; reject `true` otherwise.
- `tags.create`: tag globs (same syntax) the agent may create. Empty or absent
  means no tag may be created. Existing tags can never be moved or deleted.
- At most 50 rules, 20 tag patterns, pattern length ≤ 200.
- New capability booleans in `capabilities` (v2) and new builder capability
  rows (see §5): `pull_requests_merge` and `pull_requests_approve`. Both require
  pull request write. Default false.

### Matching

A branch name `B` (without `refs/heads/`) selects exactly one rule:

1. `@default` rule, if `B` equals the resolved default branch.
2. Otherwise, among glob rules that match `B`, the most specific wins: longer
   literal prefix (characters before the first `*`) first, then fewer `*`
   characters, then longer pattern; a remaining tie picks the most restrictive
   role (`read` < `protected` < `agent`) and `run_workflows=false` over true.
3. No rule matches → `read`.

| Role | push / commit / update | create / delete branch | open PR into (base) |
| --- | --- | --- | --- |
| read | no | no | no |
| protected | no | no | yes |
| agent | yes (force allowed) | yes | yes |

### Default branch resolution

`@default` is resolved when a request needs it, via `GET /repos/{owner}/{repo}`
using the same credential, cached per credential+repo for 5 minutes. On lookup
failure, any decision that depends on `@default` fails closed (refused with a
clear message). Never log tokens.

## 2. Compatibility

- `branch_policy` absent → legacy behaviour exactly as today: no ref checks on
  push; `workflow_dispatch` uses `ref_allowlist` (`path.Match`, unchanged).
  `pull_requests_merge`/`approve` are not consulted for legacy grants, so legacy
  grants keep their current endpoint set.
- `branch_policy` present → `ref_allowlist` must be absent (validation error
  otherwise); dispatch refs come from rules with `run_workflows`.
- Old clients parse v2 non-strictly; new fields must not change `git list`
  output (`cmd/client`).
- Builder (phantom token page) configs follow the same rule per repository.

## 3. Enforcement (server proxy, GitHub service, repo has a branch_policy)

Applied after the existing endpoint allow decision and before forwarding. Any
ref-writing request whose ref cannot be determined is refused (fail closed).
Refusals return 403 JSON `{"error": "duckway: <reason>"}` for REST and a git
report-status rejection for receive-pack. Each refusal is recorded in the
request log like other ACL denials.

| Request | Ref source | Rule |
| --- | --- | --- |
| `POST {repo}.git/git-receive-pack` | pkt-line commands before the first flush (skip `shallow` lines; first command carries capabilities after NUL) | branch create/update/delete: `agent`; tag: create only (old = zero id) and name matches `tags.create`; any other ref namespace: refused. If any command is refused, nothing is forwarded. |
| `POST /repos/{r}/git/refs` | body `ref` | `refs/heads/X` → `agent`; `refs/tags/X` → `tags.create`; else refused |
| `PATCH`/`DELETE /repos/{r}/git/refs/{ref}` | path (`heads/X`, `tags/X`) | branch → `agent`; tag → refused |
| other `/repos/{r}/git/*` writes (blobs, trees, commits, tag objects) | none | unchanged (no ref is moved) |
| `PUT`/`DELETE /repos/{r}/contents/{path}` | body `branch`, absent = `@default` | `agent` |
| `POST /repos/{r}/merges` | body `base` | `agent` |
| `POST /repos/{r}/releases`, `PATCH /repos/{r}/releases/{id}` | body `tag_name` (PATCH: only when present) | must match `tags.create` |
| `POST /repos/{r}/pulls` | body `base` | `protected` or `agent` |
| `PATCH /repos/{r}/pulls/{n}` | body `base` (only when present) | `protected` or `agent` |
| `PUT /repos/{r}/pulls/{n}/merge`, `PUT /repos/{r}/pulls/{n}/update-branch` | none | requires `pull_requests_merge` |
| `POST /repos/{r}/pulls/{n}/reviews` with body `event` = `APPROVE`; `POST /repos/{r}/pulls/{n}/reviews/{id}/events` with `event` = `APPROVE` | body | requires `pull_requests_approve`; COMMENT / REQUEST_CHANGES / pending reviews unaffected |
| `POST /repos/{r}/actions/workflows/{w}/dispatches` | body `ref` (`X`, `refs/heads/X`; tags refused) | matched rule has `run_workflows` |
| rerun / cancel | none | unchanged capability check |

Event matching is case-insensitive. JSON bodies that fail to parse on these
endpoints are refused.

Hardening rules (all refusals use the 403 / `ng` forms above):

- Names are bounded before matching: a branch, tag, ref or dispatch name longer
  than 255 bytes is refused, and the glob matcher itself returns no match above
  1024 bytes.
- Branch names taken from a request body are refused when they start with
  `heads/`, `tags/` or `remotes/`, contain `..`, `@{` or `//`, or start or end
  with `/`; GitHub could resolve such a name to a different ref than the one
  the rules were evaluated for.
- `@default` is compared case-insensitively: a name that equals the default
  branch ignoring case is treated as the default branch (the fail-closed
  direction).
- Default deny: for a repository with a branch policy, every non-GET request is
  refused (`duckway: endpoint not allowed under branch rules`) unless it is in
  the table above or in this list (anything else, including `runners/**`,
  `pending_deployments`, run `approve`, review dismissals, secrets, branch
  rename and merge-upstream, is refused):
  - `issues/**`; `pulls/**` except merge, update-branch and
    `reviews/{id}/dismissals`; `POST git/blobs|trees|commits|tags`.
  - Actions: `POST actions/runs/{id}/(rerun|rerun-failed-jobs|cancel|force-cancel)`,
    `DELETE actions/runs/{id}` and `actions/runs/{id}/logs`,
    `POST actions/jobs/{id}/rerun`, `DELETE actions/artifacts/{id}`.
  - Releases: `POST releases/{id}/assets`, `PATCH|DELETE releases/assets/{id}`,
    `POST releases/generate-notes`, `DELETE releases/{id}`.
- When any repository of the placeholder's config has a branch policy, non-GET
  `/graphql` and every `/repositories/**` path are refused (neither can be
  attributed to a repository policy).
- Method override: `X-HTTP-Method-Override`, `X-HTTP-Method` and
  `X-Method-Override` are removed from every forwarded GitHub request, and a
  request with a `_method` query parameter is refused for a repository with a
  policy.
- Smart-HTTP paths (`.git/git-receive-pack`, `.git/git-upload-pack`,
  `.git/info/refs`) are recognised before the `/repos/` prefix, but only when
  the part before the suffix is exactly `owner/repo`; `/repos/x.git/git-receive-pack`
  is the push endpoint of repository `repos/x`, while a REST path that merely
  ends in such a suffix (`/repos/o/r/contents/a.git/info/refs`) is an ordinary
  REST path of `o/r` and is guarded as one. A write under `/repos/` that does
  not resolve to `owner/repo` is refused whenever any policy exists.
- Whether a config carries a policy is decided by decoding it with the same
  case-insensitive field matching every reader uses (`Branch_Policy` is a
  policy, not a skipped unknown key); the decoded result is memoised per config
  string.
- One ref-name check (`GitHubRefNameProblem`) serves pattern validation and
  every request-time name check: no control characters, space, `~ ^ : ? [ \`,
  `..`, `//`, `@{`, leading `/`; concrete names also no `*`, no trailing `/`,
  at most 255 bytes. Pushes to a ref with such a name are refused.
- A refusal that does not depend on the default branch is decided before an
  approval rule can hold the request, so a definitely refused request never
  creates an approval. Decisions that need `@default` keep the order
  ACL, approval hold, upstream credential, default lookup, guard.
- App-backed grants: merge and update-branch mint
  `{pull_requests: write, contents: write}` when the policy opens merging
  (`pull_requests_merge`); every other request mints what it always did.
- Policies are stored normalised (no `refs/heads/` or `refs/tags/` prefixes).
- The editor serialises trimmed patterns; the server rejects whitespace.

### receive-pack details

- Read and parse only the command section (bounded, e.g. 1 MiB / 1000
  commands); then stream the buffered prefix plus the remaining body upstream
  unchanged. Do not buffer the pack.
- Refuse a request with a non-identity `Content-Encoding` when a policy applies.
- Rejection response: HTTP 200, `Content-Type:
  application/x-git-receive-pack-result`, body with `unpack ok` and one
  `ng <ref> duckway: <reason>` per command (allowed ones in a refused push get
  `ng <ref> duckway: another ref in this push was refused`), wrapped in
  side-band-64k band 1 when the client requested `side-band-64k`, and only if
  the client requested `report-status`; otherwise 403 with a plain message.
  A real `git push` must print `! [remote rejected] <ref> (duckway: ...)`.

## 4. Admin UI (client detail → assign mintable GitHub key; phantom token builder)

One shared, CSP-safe component (no innerHTML string building; follow existing
`DW.html` patterns) used by both pages, matching board `RefBranchRules`:

- Per repository, a "Branch rules" panel shown when push / contents write,
  pull request write, or workflow dispatch is enabled.
- Templates: "Agent works on its own branches" (default for new grants:
  `@default` protected, `agent/**` agent, `**` read, no tags), "CI operator"
  (`@default` and `release/**` protected with run workflows, `**` read),
  "Read only".
- Rows: fixed first row "default branch" (shows the real name when known),
  free rows (add by pattern, remove), fixed last row "All other branches"
  (`**`). Role as a three-way segmented control; "Run workflows here" checkbox
  disabled unless dispatch is enabled; warning row when `agent` + run.
- Tags: "No new tags" / "May create tags matching [pattern list]".
- Under pull requests: "Merge pull requests" and "Submit approving reviews"
  toggles, default off.
- "Try a branch" tester: shows matched rule, role and allowed actions, using the
  same matching rules (keep a shared test vector table, §6).
- Inline validation; Save disabled with an inline message when invalid
  (e.g. dispatch enabled but no rule has run workflows, empty workflow list).
- Workflow and environment allowlists remain, with visible labels.
- The legacy "Allowed refs" field is gone for new grants. A legacy grant shows
  a notice and a "Convert to branch rules" action that fills the editor; the
  admin reviews and saves. Conversion: `**` = `agent` if push else `read`;
  `@default` = same role as `**`; each `ref_allowlist` entry becomes a rule with
  role `agent` if push else `protected` and run_workflows true; tags: none;
  merge: true only for builder grants that had pull request write; approve:
  false. The notice lists these differences before saving.
- Works at 412 px width (Pixel 10), dark and light themes.

## 5. Builder (phantom token page)

The builder rule (the `PermissionRule` carrying `github_capabilities`) gains an
optional field keyed by the same lower-case `owner/repo` keys:

```json
"github_branch_policies": {
  "acme/api": {
    "branch_policy": { "rules": [...], "tags": {"create": []} },
    "pull_requests_merge": false,
    "pull_requests_approve": false
  }
}
```

The browser keeps compiling endpoints from the capability table as today. The
server reads this field for enforcement (§3): when a repository has an entry,
merge/update-branch and APPROVE reviews are refused unless the matching flag is
true, and all §3 ref checks apply. `run_workflows: true` requires that repo's
Actions capability at write level. Repos without an entry keep legacy behaviour.

## 6. Tests

- Matching: one shared vector file,
  `internal/server/services/testdata/github_branch_match_vectors.json`,
  consumed by Go and JS tests (`web/testdata/gbr_test.js`, run by
  `web/gbr_js_test.go`; set `DUCKWAY_REQUIRE_NODE=1` to fail when node is missing).
- Validation: limits, invalid globs, `ref_allowlist` + `branch_policy` conflict,
  `run_workflows` without dispatch, merge/approve without PR write.
- Enforcement: every table row above, allow and refuse, plus fail-closed
  `@default` lookup failure and malformed bodies.
- receive-pack: unit tests for pkt-line parsing (shallow, capabilities, many
  commands, oversize) and an integration test with a real `git push` through
  the proxy against a local fake upstream showing the remote-rejected message
  and that allowed pushes still stream the pack.
- Legacy configs: unchanged behaviour (existing tests stay green).
- UI: template tests for both pages, conversion, and the tester.

## 7. Limitations

- A bare `ref` in a workflow dispatch (`main`, no `refs/heads/`) is treated as a
  branch name. A tag with the same name as a branch cannot be told apart by the
  proxy, so dispatch is checked against the branch rules only.
- Pushes with more than 1000 commands are refused; a command section the proxy
  cannot parse gets a plain 403 instead of a report-status rejection.
- Branch rules do not stop `on: push` or `on: pull_request` workflows that a
  permitted push or pull request triggers. Treat any branch an agent may write
  as able to run the workflows triggered by pushes to it.
- JSON requests the guard must read are capped at 16 MiB (413 above that,
  recorded as an ACL denial).
- A refused push drains at most 256 MiB of the client's pack, for at most 30
  seconds, so the client sees the rejection instead of a connection reset.
- Approval-held requests whose decision needs `@default` are held before the
  guard can refuse them.
- The request log records only that a request was refused, not the ref or the
  reason.
