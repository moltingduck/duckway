# Current batch: live-credential-path-20261017

## Objective and acceptance
Share ignored live credential files across worktrees through the
`DUCKWAY_LIVE_CREDENTIALS_DIR` environment variable or shared Git config
`duckway.liveCredentialsDir`, while preserving per-script `live-credentials`
fallback and explicit `CODEX_AUTH`/`CLAUDE_AUTH` overrides. Relative configured
paths resolve from the primary worktree where Git exposes that root. Add
credential-free regression coverage and keep secrets untouched.

## Candidate status
Implementation on `codex/live-credential-path-impl`, base
`04ca844e04eb023a03d69c71aad5b41ce19a19e0`. Shared resolver is used by all six
live credential consumer scripts and `find-oauth-tokens.sh`; user-facing hints
now describe the configured path. Credential-free shell regression covers env
over config, config over fallback, relative path normalization, linked
worktree root anchoring, separate-git-dir primary checkout behavior, and
explicit CODEX_AUTH/CLAUDE_AUTH child preflight precedence using mock paths
with each agent mode selected explicitly. Separate-git-dir linked worktrees
now fail clearly when Git cannot identify the primary checkout root, avoiding
an ambiguous relative path. Resolver failure in the Claude OAuth script exits
with a clear error. Final independent tests and security review passed. The
demo was rebuilt and passed readiness under owner `ducklord-verified-20260917`;
the existing five named containers were retained. Credential inspection was
metadata-only: the shared directory's Codex file is currently missing or
nonregular, and no credential contents were read or changed. No provider tests
were run. Commit-hook gates: container-runtime/demo-common PASS;
`go test -race ./...` PASS; coverage 57.4% (threshold 37.5%) PASS. `golangci-lint`
reported 11 unrelated pre-existing Go issues, including warnings for other
worktrees and a removed path; this shell-only change contains no Go changes.
This candidate is ready for the root agent to commit and push.

# Current batch: tui-palette-actions-20261016

## Objective and acceptance
Implement the bounded command palette action batch: add useful operational
commands through existing flows, support both terminal Backspace encodings with
Unicode-rune deletion, keep fuzzy search/navigation correct with no matches, add
regressions and update help. Preserve project/session generation fencing.

## Candidate and resource ledger
Owner `tui-palette-actions-20261016`; branch
`codex/tui-palette-actions-impl`; worktree
`/home/cjiso1117/duckway-tui-palette-actions-impl`; base
`1f7384a88d2d63965b9b698c997552617b28f5ed` (`origin/main`). Implementation commit
`94c1922154b053f70564eb0b00087804ea6e8704`; amended locally for these
documentation updates; worktree clean. No push: Claude
live E2E credential preflight returned HTTP 401 and OAuth refresh-only returned
HTTP 502, so the `AGENTS.md` push gate remains closed. No feature-owned temporary
root or build outputs remain; the retained demo is tracked below.

## Status
Implementation and task/handoff updates complete. Independent full tests, race,
build, and isolated PTY E2E pass on the integrated candidate. Scoped lint has
9 existing findings with no findings on candidate changes. Codex live agent E2E
passes; Claude live E2E is blocked by credential preflight (401) and OAuth
refresh-only (502). Push remains prohibited by `AGENTS.md` until both live gates pass; Claude is blocked by HTTP 401/502.
Add host dispatch transfers focused PTY ownership to the modal; cancel and
completion restore the source PTY only when client, instance, session ID, and
runtime generation still match. Replaced runtimes leave focus with workspace
navigation; SSH config load errors preserve focus. Asynchronous workspace control acceptance is fenced
while the add-host modal owns keyboard input.
The retained demo was rebuilt and restarted from this commit and reports ready;
all five `ducklord-verified-*` containers are running. Launch with `podman exec
-it ducklord-verified-ducklord-dev ducklord tui --config
/root/.ducklord/config.yaml`.

---

# Current batch: codex-routing-0924

## Objective
Diagnose Codex 0.156.1 account/read workspace routing timeout through Duckway.
User reproduced on hermes@orb with proxy exec and --no-daemon. Do not claim a
fix without reproducing and verifying the failing bootstrap path.

## Acceptance
- Identify actual discovery HTTP route and isolate the failing stage.
- If a Duckway defect is established, add regression coverage, fix, independently
  review and verify bootstrap, then rebuild/restart the retained demo.
- Record deployment limitations and dispose all owned resources.

## Resource ledger
Owner codex-routing-0924. Planned sibling worktree ../duckway-codex-routing-0924-probe,
branch codex/codex-routing-0924-probe, for a lower-tier diagnostic implementation.
Its ignored tmp/codex-routing-0924 root owns mock server, child app-server,
synthetic auth/config and sanitized output; bounded execution with EXIT cleanup.
Remove worktree/branch after preserving any useful reviewed result.
Baseline retained: existing five .claude worktrees and old containers listed in
HANDOFF; named persistent demo ducklord-verified, owner ducklord-verified-20260917.
User authorizes local live credentials. Probe reads the local credential source
without printing tokens or account identities; isolated credential copies, local
server/client, app-server and build outputs belong to the worktree ignored root.
No raw live transcript capture. Teardown terminates and waits for child processes
and removes credential copies, databases, logs, binaries and temporary roots.

## Status: superseded and closed
The diagnosis checkpoint is superseded by completed repair batch
codex-account-routing-0924 below. The local live credential test isolated the
account-ID mismatch and the repair batch fixed and verified that path.
SSH orb DNS is not resolvable here, so remote TUI startup remains unverified.
Main orchestrates/verifies; only lower-tier subagents write diagnostic code.
Initial probe results were invalid: the app-server subprocess omitted its supplied
environment and the log filter missed openai-chatgpt. Both defects were corrected;
the final harness and isolation were independently reviewed.

Final Codex 0.156.1 live run, same credential snapshot, isolated HOME/CODEX_HOME:
- Direct account/read signed in: 596 ms.
- Stock Duckway-synced phantom auth: -32603, selected workspace missing, 941 ms.
- Test-only copy matching phantom account IDs to source account ID: signed in,
  640 ms, through the same gateway; still fake access/id tokens.
- Discovery /backend-api/wham/accounts/check returned HTTP 200.

This establishes an account-ID mismatch in local bootstrap, not a reproduction
of the remote timeout or evidence of a complete interactive Codex session.
The experiment must not become the production fix: existing privacy tests require
that real account identities are absent from client phantom credentials.
Next product batch needs a privacy-preserving identity mapping, regression tests,
independent review and live verification. Original credential files were not edited.
Probe runtime, credential copies, worktree and branch removed after source/diff
review and preservation of sanitized evidence in HANDOFF.md. Registry pruned.
Retained demo rebuild/restart passed readiness, PTY I/O, daemon recovery and native
shell lifecycle (exec session 38024). Final resource scan and diff check passed.
Local live-test request fulfilled; product repair remains open as stated above.

## Completed repair batch: codex-account-routing-0924
Root cause: Codex 0.156.1 sends Duckway's phantom account ID during workspace
discovery, while the upstream account-check response contains the OAuth
account ID. Duckway now maps only the scoped discovery request upstream and
maps known account fields in that route's bounded response back to the phantom
ID. This keeps real identity out of client auth. It also normalizes compressed
response handling, drops stale validators after rewriting the body, and closes
failed WebSocket upgrades. Regression tests cover scope, privacy, limits,
gzip, headers and socket cleanup.

Validation: `go test ./...`, handler race tests, `go vet` and affected-package
lint passed. Codex 0.156.1 with local live credentials completed `account/read`
and a prompt through the isolated Duckway proxy; LLM and WebSocket checks
passed. Full-repository lint still reports 13 pre-existing issues; changed
handler package lint reports zero. Security, quality, race and API reviews
passed after the API review's validator-header fix. Frontend/E2E review was not
applicable to this backend-only protocol change. Orb DNS is unresolved here,
so remote deployment is not verified.

The retained demo was rebuilt/restarted and readiness checks passed. Launch:
`podman exec -it ducklord-verified-ducklord-dev ducklord tui --config /root/.ducklord/config.yaml`.
Owner codex-account-routing-0924 temporary live root and its logs were removed;
ephemeral Podman test container is absent; implementation worktree and branch
were removed and the worktree registry pruned. Final `git diff --check` passed.
Preserved baseline five `.claude` worktrees and older `dw-*-1788855706-1278453`
containers, plus the retained demo. Usage metrics unavailable/unmeasured.
