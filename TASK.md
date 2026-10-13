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
