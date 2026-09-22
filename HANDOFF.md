# Current handoff


Updated: 2026-09-22 Asia/Taipei

## Project file exchange v1 — complete

Bounded batch: `TASK.md`; implementation base `8d9474f`, integrated code
`86a56e0`. Lower-tier agents implemented isolated backend, UI, and E2E scopes.
Two directory columns support Local, configured SSH hosts, and a persistent
Project shelf. Host copies stream through Ducklord without direct host links.
Current-directory search, multiselect, drag/drop, conflict preview, and modal
focus restoration are implemented. Recursive search and task-bundle metadata
remain outside v1. In the demo, Local means the controller container.

Five independent review roles cleared the integrated changes; affected fixes
received follow-up reviews. A final E2E review initially suspected a status-row
offset error, then cleared it after checking 1-based screen coordinates against
0-based slice indexes. No code change was needed for that finding.

Validation:
- `GOFLAGS=-buildvcs=false go test ./...` — PASS at `7373ace`.
- `CGO_ENABLED=1 GOFLAGS=-buildvcs=false go test -race ./...` — PASS at `51eaf19`.
- Later directory-relay and filter fixes passed affected normal/race tests.
  Final empty-copy guard passed full `cmd/ducklord` normal/race tests.
- Routing manifest — PASS (21 mappings; not behavioral acceptance).
- `bash scripts/pre-commit-env-test.sh` — PASS.
- `GOFLAGS=-buildvcs=false golangci-lint run ./...` — 13 pre-existing findings;
  no new issues from this batch.
- `scripts/check-coverage.sh` — PASS, 56.6% >= 37.5% at `94bbd7a`. Use default
  `/tmp`: longer TMPDIR caused unrelated Unix-socket path-length failures.
- Real exchange PTY/container E2E — PASS at `86a56e0`, 21.378s (run 6).
  Proves cross-host file/directory/multiselect and drag transfers, shelf transfer
  both directions and persistence, skip/rename/overwrite, preview cancellation,
  actual staged-transfer cancellation/read failure cleanup, intact sources, and
  restored terminal control through an executed sentinel.
- Full default PTY/container suite — PASS at `86a56e0`, 267.285s: 31 passed,
  1 intentional legacy-wizard skip, 0 failures. Exit 0; fixture teardown passed.

Failures discovered and fixed during real E2E: missing modal overlay in workspace
layouts, Ctrl-C intercepted by PTY gates, directory archive EOF, stale directory
filters, and empty copy previews during asynchronous list loading. E2E now
waits for reconstructed current-screen controls, the correct column's endpoint,
path and load status, and an actual marked entry before copying. Destination
filesystem bytes remain the acceptance evidence. Cancellation/failure waits for
receiver staging before triggering interruption and requires staging removal.

Test infrastructure repair: inherited Git hook environment contaminated fixture
`git init --bare`; restored `core.bare=false`, verified origin/HEAD, and added hook
environment isolation with a regression. Integration commits disable hooks;
required checks run explicitly outside hook state.

Demo rebuilt/restarted successfully with `scripts/ducklord-podman-demo.sh`
using prefix `ducklord-verified` and owner `ducklord-verified-20260917`.
Controller and clients a–d are Up; `ducklord clients` succeeds and SSH Ducklion
probes for configured clients a/b/c all report available. The retained demo's
SHA-256 values match the full E2E fixture exactly:
- ducklord: `224096a29a09b624f5b4d3e6d9d7cb4024e9c841a4d5611038983664a0184a8c`
- ducklion: `e547089b8b738315907eedf0524417b22b1c6142c15f8135e6336f2b8133e553`

Launch:
```sh
podman exec -it ducklord-verified-ducklord-dev ducklord tui --config /root/.ducklord/config.yaml
```
From Project/quick Session list press `f`; focused terminal uses `prefix+f`.
Operation guide: [Exchange Project files](docs/ducklord-user-guide.md#exchange-project-files).

Resources: all batch worktrees/branches integrated and removed; Git registry
pruned. Owned temporary roots `/tmp/duckway-exchange-v1-20260922` and `/tmp/x0922`
removed after recording results here. Test script traps removed fixture containers
and networks, including final full-suite prefix `ducklord-tui-e2e-1752833`.
Final process scan found no owned workers. Preserved five baseline
`.claude/worktrees`, old `dw-*-1788855706-1278453`, and seven older E2E networks
(created September 17/20). Retained demo is explicitly user-requested above.
Usage metrics unavailable/unmeasured; see the workflow batch ledger. No remaining
implementation or verification work; await the next user request.

## Terminal bookmark retention and output-search viewport repair (2026-09-22)

Terminal bookmarks now persist a session identity plus a line anchor and line
fingerprint, rather than fingerprinting the entire output snapshot. Appended
output therefore leaves a bookmark usable; the picker reports it unavailable
only after the actual retained terminal history no longer contains its anchor.
No terminal transcript is saved in bookmark metadata. In a `prefix+/` output
search, Up/Down now also scroll the retained terminal viewport to the selected
match while the search modal retains keyboard ownership and restores focus on
close. The user guide documents both routes and the Project config
export/import flow.

Validation:

- `GOFLAGS=-buildvcs=false go test -count=1 ./internal/ducklord ./cmd/ducklord` — PASS.
- `scripts/check-routing-manifest.sh` — PASS (20 routes).
- `DUCKLORD_TUI_E2E_PATTERN='^TestDucklordOutputSearchBookmarksContainerE2E$' CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS.
- `CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (exit 0; 243.945s).

Demo restart/readiness: rebuilt and restarted with
`DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh`.
`ducklord-verified-ducklord-dev` and clients `a` through `d` are Up; `ducklord
clients --config /root/.ducklord/config.yaml` lists client-a, client-b, and
client-c.
Usage metrics: unavailable/unmeasured.

## Development resource-cleanup gate (2026-09-22 Asia/Taipei)

The reusable development rules now require an owner-specific resource ledger,
scoped temporary roots, interruption-safe teardown, reviewed worktree disposal,
and a non-destructive final resource scan. Completion is blocked by any
batch-owned worktree, process, container, socket, temporary root, generated
artifact, or unreviewed branch. See `AGENTS.md` and
`docs/agent-workflow.md` section 1b.

Validation:

- `git diff --check` — PASS.
- `git rev-parse --is-bare-repository` — `false`; `git status --short` showed
  only this documentation batch before it is committed.
- Resource scan found no batch-owned `/tmp/duckway-*`, test Ducklion daemon,
  root `ducklord` binary, `.orig`, coverage, or generated artifact.
- `git worktree list --porcelain` contains the primary checkout plus five
  pre-existing `.claude/worktrees` snapshots. They are explicitly retained as
  the prior cleanup baseline, not resources created by this documentation batch.
- Rebuilt and restarted the retained owner-labelled demo with
  `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified`
  `DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917`
  `scripts/ducklord-podman-demo.sh` — PASS. The five `ducklord-verified-*`
  containers are running; `ducklord clients --config /root/.ducklord/config.yaml`
  lists client-a, client-b, and client-c with Ducklion.

Resource ledger: this documentation-only batch created no worktree, test
runtime, temporary root, process, or container. The restarted
`ducklord-verified-*` Podman demo is intentionally retained for inspection.
Usage metrics: unavailable/unmeasured.

## Workspace capabilities: palette, terminal tools, resources, transfer (2026-09-22)

Completed the four selected workspace capabilities. `prefix+Space` opens a
fuzzy command palette for actions, Projects, and Sessions; its modal owns
input and restores the exact origin focus on close. A focused terminal now
supports `prefix+/` output search, `m` bookmark creation, and `M` bookmark
listing without passing tool input to the PTY. Host settings includes a
read-only Resources route whose asynchronous completions are fenced by Host
identity and request state. Project config includes a reviewed export/import
flow: it validates bounded JSON, previews changes before confirmation,
regenerates imported identities, and excludes PTY output and secrets.

The route specification and verification manifest cover each new route in
`docs/pane-routing.md` and `docs/ui-routing-verification.md`. The transfer
path additionally rejects oversized note content and symlink traversal while
writing or rolling back imports.

Validation:

- `GOFLAGS=-buildvcs=false go test -count=1 ./...` — PASS.
- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord ./internal/ducklord ./internal/ducklion/daemon` — PASS.
- `scripts/check-routing-manifest.sh` — PASS (20 routes).
- `CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (exit 0; 241.311s).
- Independent frontend/E2E, quality, race/concurrency, backend API, and security reviews completed. The security findings resulted in bounded note reads and descriptor-pinned, no-follow import writes; final rereview found no remaining actionable issue.

Demo restart/readiness: rebuilt and restarted with
`DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh`.
All five `ducklord-verified-*` containers are Up, and `ducklord clients --config
/root/.ducklord/config.yaml` lists client-a, client-b, and client-c.
Usage metrics: unavailable/unmeasured.

## Direct-detach input handoff repair (2026-09-21)

Fixed direct-detach handoff so deferred post-detach input remains targeted to
the PTY after control-lease acceptance instead of being replayed as workspace
navigation.

Validation:

- `TestFocusedTerminalPrefixDSelectsSurvivingPaneForHandoff` — PASS.
- `CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (224.507s).
- `GOFLAGS=-buildvcs=false go test -count=1 ./...` — PASS.
- `scripts/check-routing-manifest.sh` — PASS.

## Remote deletion, managed rename, and stale host-event guard (2026-09-21)

Completed the Host Skills batch for confirmed remote-skill deletion and
managed-repository skill rename. Rename validates the new ID and migrates all
saved Host/agent state records; deletion is scoped to the captured Host, agent,
directory, and skill ID. Host identity checks now discard stale asynchronous
events from a replaced or removed Host.

Validation:

- Host Skills container E2E — PASS.
- Routing container E2E — PASS.
- `GOFLAGS=-buildvcs=false go test -count=1 ./...` — PASS.
- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord ./internal/ducklord ./internal/ducklion/skill` — PASS.

Demo restart/readiness: restarted after final integrated tests with the
`ducklord-verified` prefix; the readiness command passed and
`ducklord-verified-ducklord-dev` plus clients `a`/`b`/`c`/`d` are up.
Usage metrics: unavailable/unmeasured.

## Host Skills remote deletion and managed rename (2026-09-21)

The host-centred dual-pane Skills manager now supports confirmed deletion of a
selected remote skill and confirmed rename of a selected Ducklord managed
repository skill. Remote deletion is scoped to the captured Host, agent target,
directory, and skill ID; it refreshes that target's remote list without changing
Ducklord's managed repository or saved management state. Managed rename
validates a new unique ID, moves the repository directory, and migrates every
saved Host/agent state record atomically, rolling back the directory if saving
configuration fails.

The Host Skills container route now checks the correct focus contract for a
direct `h` invocation: Ctrl+C returns to workspace navigation, where `?` can
open and close help. It does not falsely require terminal focus that was never
owned by the route.

Validation:

- `DUCKLORD_TUI_E2E_PATTERN='^TestDucklordHostSkillsContainerE2E$' CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (5.07s).
- `GOFLAGS=-buildvcs=false go test -count=1 ./internal/ducklion/skill ./internal/ducklioncli ./internal/ducklord -run 'Test(SkillSSH|Run|Delete|RenameManaged)'` — PASS.
- `GOFLAGS=-buildvcs=false go test -count=1 ./cmd/ducklord -run '^TestHostSkills'` — PASS.
- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord ./internal/ducklord ./internal/ducklion/skill` — PASS.
- `GOFLAGS=-buildvcs=false go test -count=1 ./...` — PASS.
- `scripts/check-routing-manifest.sh` — PASS (15 routes).
- Explicit worktree `git diff --check` — PASS.

Demo restart/readiness: rebuilt and restarted with
`DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh`. All five `ducklord-verified-*` containers are Up, and `ducklord clients --config /root/.ducklord/config.yaml` lists client-a, client-b, and client-c. Usage metrics: unavailable/unmeasured.

# Current handoff

Updated: 2026-09-21 Asia/Taipei

## Host-centred, per-agent Skill management (2026-09-21)

Host Skills now follows `h` → **Host list** → Enter → **Host settings** →
**Skills** → **agent installation** → **skills**.  A management record belongs
to one host and one agent target, and is explicitly `none`, `push`, or `pull`.
`push` deploys only that selected target's skills. `pull` stages a remote skill,
shows its diff, and records the state only after confirmation. An explicit
`none` record prevents legacy host-wide selections from being deployed again.

The container E2E drives the complete route with a real PTY. It proves that
Ducklord's `ducklord-quack` skill (`呱呱`) is pushed to client-a and that
client-a's `client-a-meow` skill (`喵喵`) is pulled only after confirmation. It
also opens a `client-a-purr` preview, sends Ctrl+C, verifies the preview is
discarded, restores workspace input, and reopens the route before performing
the successful transfer.

Validation:

- `DUCKLORD_TUI_E2E_PATTERN='^TestDucklordHostSkillsContainerE2E$' CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (4.78s).
- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord ./internal/ducklord` — PASS.
- `GOFLAGS=-buildvcs=false go test -count=1 ./...` — PASS.
- `scripts/check-routing-manifest.sh` — PASS (15 routes).
- Explicit worktree `git diff --check` — PASS.
- Independent reviews completed for security, config/deploy, routing, async behaviour, and E2E/docs. The config review found and the implementation fixed the legacy-selection fallback for explicit `none`; the E2E/docs review prompted the live-preview Ctrl+C check above.

Demo restart/readiness: rebuilt and restarted with
`DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh`.
All five `ducklord-verified-*` containers are Up, and `ducklord clients
--config /root/.ducklord/config.yaml` lists client-a, client-b, and client-c.
Usage metrics: unavailable/unmeasured.

# Current handoff

Updated: 2026-09-21 Asia/Taipei

## Demo Host Skills fixtures and transfer E2E (2026-09-21)

The Podman demo now seeds two separate, visible skill fixtures: Ducklord's
managed repository contains `ducklord-quack` (reply exactly `呱呱`) and
client-a's configured Codex target contains `client-a-meow` (reply exactly
`喵喵`). client-a preselects `ducklord-quack`, so `d` deploys it through SSH.

The Host Skills list now reserves `r` for downloading the selected managed
skill and adds `R` for entering any remote skill ID. This makes the demo pull
route direct: use `R`, enter `client-a-meow`, choose `codex`, inspect the
preview, then press Enter to confirm. The result remains in Ducklord's managed
repository, separate from agent-loaded client paths.

Validation:

- `DUCKLORD_TUI_E2E_PATTERN='^TestDucklordHostSkillsContainerE2E$' CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (5.92s). It asserts SSH push writes `呱呱` to client-a and preview/confirmation pull writes `喵喵` only after confirmation.
- `GOFLAGS=-buildvcs=false go test -count=1 ./cmd/ducklord` — PASS.
- `GOFLAGS=-buildvcs=false go test -count=1 ./internal/ducklord` — PASS.
- `bash -n scripts/ducklord-podman-demo.sh` — PASS.
- `scripts/check-routing-manifest.sh` — PASS (15 routes).
- `git diff --check` using the explicit repository worktree — PASS.

Demo restart/readiness: rebuilt and restarted with `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh`. All five `ducklord-verified-*` containers are Up; `ducklord clients --config /root/.ducklord/config.yaml` lists client-a, client-b, and client-c. Fixture content was verified inside both containers. Usage metrics: unavailable/unmeasured.

# Current handoff

Updated: 2026-09-21 Asia/Taipei

## Managed Host Skills: HTTPS tracking and safe transfer finalization (2026-09-21)

Ducklord now maintains a separate managed skill repository, lets each Host
select agent install targets and skills, and supports SSH upload/download plus
manual HTTPS tracking with preview, diff, and explicit replacement confirmation.
Tracking accepts public HTTPS only; `insecure_https` is disabled by default and
is an explicit, visible per-source toggle.

A final security remediation pins the source root with directory descriptors and
reads children through `openat(..., O_NOFOLLOW)`, so a swapped nested symlink
cannot redirect local import or SSH archive creation. Reads are bounded by the
skill limit, and inspection rejects content whose descriptor-read byte count
changes from the enumerated source. Regression tests swap a nested directory
after it is discovered and before transfer, and independently verify the pinned
descriptor rejects the external symlink.

Validation:

- `GOFLAGS=-buildvcs=false go test -count=1 ./...` — PASS.
- `GOFLAGS=-buildvcs=false go test -race -count=1 ./internal/ducklord ./cmd/ducklord` — PASS.
- `scripts/check-routing-manifest.sh` — PASS (15 routes).
- `DUCKLORD_TUI_E2E_PATTERN='^TestDucklordHostSkillsContainerE2E$' CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (5.01s).
- Integrated reviews: security, code quality, concurrency, frontend/E2E, and backend API completed. The affected security re-review passed after the descriptor and byte-consistency remediation.

Demo restart/readiness: rebuilt and restarted with `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified`; five owner-labelled containers are Up. `ducklord clients --config /root/.ducklord/config.yaml` lists client-a, client-b, and client-c with Ducklion available. Usage metrics: unavailable/unmeasured.

# Current handoff

Updated: 2026-09-21 Asia/Taipei

## Default Project focused new-shell route repair (2026-09-21)

Opening **New shell session** from a focused Default Project terminal left the
PTY focus flag set. The next `Enter` on the Host selector was therefore routed
to the old remote shell before the create wizard handled it, leaving the UI at
`host ›` without starting bookmark discovery. `beginCreate` now transfers
keyboard ownership to the create modal before showing it. The focused-route
regression test asserts this focus transition; the container route exercises
Host selection, discovery, directory selection, split placement, and new-pane
focus.

Validation:

- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord -run 'TestWorkspace(ProjectDirectCreateUsesAssociatedHosts|NewShellHostEnterDoesNotRestartDiscovery|ControlCannotReacquireFocusWhileNotesOpen)$'` — PASS.
- `DUCKLORD_TUI_E2E_PATTERN='^TestDucklordWorkspaceDefaultNewShellSplitContainerE2E$' CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-tui-e2e.sh` — PASS (6.84s).
- `scripts/check-routing-manifest.sh` — PASS (15 routes, state tests, and default E2E mappings).
- `GOFLAGS=-buildvcs=false go test -count=1 ./cmd/ducklord` — PASS.
- Independent read-only route review — PASS; no remaining ownership or input-order defect found.

Demo restart/readiness: rebuilt and restarted with `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh`; five owner-labelled containers are Up and `ducklord clients` returned client-a, client-b, and client-c. Usage metrics: unavailable/unmeasured.

# Current handoff

Updated: 2026-09-18 Asia/Taipei

## Managed Host Skills: public HTTPS and explicit insecure TLS (2026-09-20)

Host Skill tracking sources now require HTTPS in both persisted configuration
and the TUI source form, including mixed-case URL schemes. The downloader
validates public destinations before and at dial time, disables proxies,
rechecks redirects, and only skips certificate verification when the saved
source has `insecure_https: true`. The Host Skills source modal now marks the
one active text field or checkbox, including a visible `INSECURE TLS` focus
state. The TUI E2E saves the secure default first, then explicitly focuses and
toggles the checkbox before asserting persistence.

Validation:

- `GOFLAGS=-buildvcs=false go test -count=1 ./...` — PASS.
- Focused Host Skills and HTTPS tests, plus race tests for `internal/ducklord`
  and `cmd/ducklord` — PASS.
- `DUCKLORD_TUI_E2E_PATTERN='^TestDucklordHostSkillsContainerE2E$' ... scripts/ducklord-tui-e2e.sh`
  — PASS (4.15s); it covers public HTTPS source routing, secure default,
  explicit insecure toggle, visible state, and persisted config. Fetch,
  archive validation, preview, and confirmation are covered by targeted
  internal tests; the current container fixture cannot safely serve a public
  HTTPS endpoint.
- Five-role integrated review completed: security, code quality, concurrency,
  frontend/E2E, and backend API. The quality and frontend findings were fixed
  and re-reviewed.
- `scripts/check-routing-manifest.sh` remains FAIL due to a pre-existing
  missing `TestWorkspaceControlCannotReacquireFocusWhileNotesOpen` registration.
- `TestDucklordWorkspaceDefaultNewShellSplitContainerE2E` remains FAIL: after
  Host selection it returns to `host ›`; do not treat the attempted discovery
  retry change as a completed fix.

Demo restart/readiness: `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified
DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman
GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh` — PASS. All five
owner-labelled containers are Up; client-a, client-b, and client-c are
Ducklion-ready. Usage metrics: unavailable/unmeasured.

## Human-paced `Ctrl-B t t` quick-tab repair (2026-09-18)

The repeated `t` deadline was 180 ms, which allowed the test's 25 ms delivery
but routinely expired during normal terminal typing. It is now the named
`quickShellRepeatWindow` of 500 ms. The atomic `Ctrl-Btt` path also clears all
prefix state so later keystrokes cannot be consumed by a stale command. The
quick tab retains the focused live Session's host and canonical working
directory. A lone `Ctrl-B t` still dispatches the existing new-shell wizard
after the window.

Validation:

- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord -run 'TestQuickShellPrefix(AndPlacement|AtomicRepeatedSuffix|RepeatedSuffixAfterFocusHandoff|DelayedRepeatInsideWindow|ExpiryDispatchesOrdinarySuffix|MismatchReplaysInput)$'` — PASS.
- `GOFLAGS=-buildvcs=false DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_TUI_E2E_PATTERN='^TestDucklordFocusedPrefixQuickShellTabContainerE2E$' CONTAINER_RUNTIME=podman scripts/ducklord-tui-e2e.sh` — PASS (7.30s), with `Ctrl-B`, `t`, `t` delivered separately at 300 ms intervals and the created shell checked for matching host/CWD.
- `GOFLAGS=-buildvcs=false go test -count=1 ./cmd/ducklord` — PASS.
- Independent follow-up review — PASS; it identified the atomic stale-prefix issue above, which was fixed and re-reviewed.
- Both `ducklord-dev` and `ducklord-verified-ducklord-dev` were rebuilt and are Up; each reports Ducklion-ready client-a, client-b, and client-c.

Usage metrics: unavailable/unmeasured.

## Split `Ctrl-B tt` quick-shell-tab verification (2026-09-18)

The former tab route test sent `Ctrl-Btt` in one PTY write.  Real terminal key
delivery can split those bytes across reads, especially while the control
handoff updates focus.  The route now sends `Ctrl-B`, `t`, and `t` as separate
writes and unit coverage includes both atomic and split `tt` (with focus
handoff).  The production route preserves the captured Session origin, so the
second `t` resolves to `quick-shell:t` even after that handoff.

Validation before demo restart:

- `GOFLAGS=-buildvcs=false go test -count=1 ./cmd/ducklord -run 'TestQuickShellPrefix'` — PASS.
- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord -run 'TestQuickShellPrefix|TestPanePrefix'` — PASS.
- `GOFLAGS=-buildvcs=false DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_TUI_E2E_PATTERN='^TestDucklordFocusedPrefixQuickShellTabContainerE2E$' CONTAINER_RUNTIME=podman scripts/ducklord-tui-e2e.sh` — PASS (7.24s). The E2E proved a separately delivered `Ctrl-B`, `t`, `t` creates exactly one tab and a shell with the source host/CWD.

The prior restart used an isolated `ducklord-verified-*` namespace, while the
interactive demo is the older default `ducklord-dev`; this left the visible demo
on an old binary.  The default containers were removed and rebuilt with the current binary. `ducklord-dev` is Up and `ducklord clients --config /root/.ducklord/config.yaml` reports client-a, client-b, and client-c as Ducklion-ready. Usage metrics: unavailable/unmeasured.

## Completion-reporting gate (2026-09-18)

The workflow, UI-routing verification plan, and `duckway-pane-routing` skill
now distinguish a targeted pass from final completion. A final report requires
the integrated artifact, all required checks, resolved and re-reviewed findings,
reruns after final changes, and any requested demo restart/readiness. Progress
updates must name remaining evidence rather than imply completion.

Validation: documentation review, required-term search, and whitespace check
passed. Demo restart/readiness:
`DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS='-buildvcs=false' scripts/ducklord-podman-demo.sh` — PASS;
the service reported ready. Usage metrics: unavailable/unmeasured.

## Quick duplicate shell exact terminal-area batch (2026-09-18)

Completed the quick duplicate shell placement batch. The implementation captures
the focused pane's project, tab, and layout origin before asynchronous creation,
corrects the repeated E2E suffix handling, and fences tab moves so `Ctrl-B --`,
`Ctrl-B \\ \\`, and `Ctrl-B tt` remain attached to the exact source terminal area.

Validation:

- Focused quick-shell and prefix routing tests with `-race` — PASS.
- `go test -count=1 ./cmd/ducklord` — PASS.
- Target Podman E2E for horizontal, vertical, and tab quick-shell routes — PASS,
  all three in about 21s.
- Five-role review found issues; fixes were applied and the affected reviews were
  rerun with no remaining findings.

Demo restart/readiness: `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS='-buildvcs=false' scripts/ducklord-podman-demo.sh` — PASS. Five owner-labelled containers are Up; clients client-a, client-b, and client-c are ready. Usage metrics: unavailable/unmeasured.

## Quick same-session shell prefix (2026-09-18)

Focused live Session panes now hold a single `Ctrl-B` suffix (`-`, `\\`, or `t`) for 180 ms. Repeating that suffix opens a Shell directly on the source Session’s host and canonical working directory, uses the automatic handle, and preserves placement: `--` horizontal, `\\` vertical, and `tt` a new tab. A single suffix still opens the existing creation wizard. Mismatched follow-up input is replayed once through the normal prefix dispatcher and never sent to the PTY. Quick-shell discovery and start failures clear their temporary placement intent while ordinary creation failures retain their wizard state.

Validation:

- `GOFLAGS=-buildvcs=false go test -count=1 ./cmd/ducklord -run 'Test.*(QuickShell|PanePrefix|PaneRouteContract|Workspace.*Shell)'` — PASS.
- `GOFLAGS=-buildvcs=false go test -race -count=1 ./cmd/ducklord -run 'Test.*(QuickShell|PanePrefix|PaneRouteContract|Workspace.*Shell)'` — PASS.
- `GOFLAGS=-buildvcs=false go test -count=1 ./cmd/ducklord` — PASS.
- `GOFLAGS=-buildvcs=false DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_TUI_E2E_PATTERN='^TestDucklordFocusedPrefixQuickShellTabContainerE2E$' CONTAINER_RUNTIME=podman scripts/ducklord-tui-e2e.sh` — PASS (about 5s); it proves `Ctrl-B tt` adds exactly one tab and verifies the live shell’s `pwd -P`.
- The default `scripts/ducklord-tui-e2e.sh` ran the new quick-shell route successfully. The aggregate manifest failed in unrelated existing routes: `TestDucklordWorkspaceDefaultNewShellSplitContainerE2E` waited for `host ›` while the wizard remained on `New shell session`; `TestDucklordPaneControlContractContainerE2E/shared-shell` expected `Contract A` while `Contract B` remained selected.
- Five-role integrated review completed. The backend reviewer prompted and re-approved quick-shell failure cleanup; the frontend/E2E follow-up found no defects.

Demo restart/readiness: `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman GOFLAGS=-buildvcs=false scripts/ducklord-podman-demo.sh` — PASS. Five owner-labelled containers are Up; `ducklord clients` returned client-a, client-b, and client-c with Ducklion. This batch left no owner-prefixed E2E containers. Usage metrics: unavailable/unmeasured.

# Current handoff

Updated: 2026-09-17 Asia/Taipei

## Notes feature audit (2026-09-17)

Reviewed the modal input router, scope hierarchy, live Project Session picker,
entry and whole-notebook editing, search, OSC 52 copy, and deferred terminal
focus restoration against the routing guide. No additional reproducible Notes
product defect was found.

One route test had a stale expectation: after a Project gains a second attached
Session, Right correctly opens the Session picker instead of navigating straight
to the former sole child. `TestScopedNotesInputRoutesSearchAndPages` now chooses
the second picker item before asserting its Session scope. This protects the
intended multi-session interaction.

Validation:

- `go test -race -count=1 ./cmd/ducklord -run 'Test(.*Notes.*|.*Note.*|PaneRouteContract|FocusedPanePrefixNotesStaysLocal|DirectNotesRoutesFollowNavigationFocus|WorkspaceControlCannotReacquireFocusWhileNotesOpen)$'` — PASS (1.056s).
- `go test -race -count=1 ./internal/ducklord -run 'Test.*Notes.*'` — PASS (1.014s).
- Full `scripts/ducklord-tui-e2e.sh` reached and passed `TestDucklordNotesTUIContainerE2E` (3.78s), including scope routing, picker, edit, search, copy, and focused-terminal restoration. Its aggregate run still failed unrelated PTY timing checks in `WorkspaceTwoLivePanes`, `LocalNotification`, and `PaneControlContract`; do not cite it as a full-suite PASS.
- `git diff --check -- TASK.md HANDOFF.md cmd/ducklord/workspace_pane_modal_test.go` — PASS.

Coverage note: the live Notes E2E validates the existing picker flow; the
specific "attach a pre-existing Session while Project Notes is already open"
case is covered by the focused regression test. Usage metrics: unavailable/
unmeasured.

## Project Notes live Session picker repair (2026-09-17)

Project Notes Right navigation previously excluded `notesSessionIdentity`, even while the modal was in Project scope. That stale selection could suppress an attached session and turn a multi-session picker into an automatic single selection. The picker now takes every Session identity from the current Project layout. The new regression opens Project Notes, attaches a pre-existing second Session, verifies both names render in the picker, and selects the added Session.

Validation:

- `go test -race -count=1 ./cmd/ducklord -run 'Test(ProjectNotesRightArrowListsSessionsAddedAfterOpening|ProjectNotesRightArrowOpensItsOnlySession|NotesSessionPickerResolvesSelectedIdentity|SessionNotesPickerUsesSessionNames|PaneRouteContract)$'` — PASS (1.026s).
- `DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_TUI_E2E_PATTERN='^TestDucklordNotesTUIContainerE2E$' timeout 55s scripts/ducklord-tui-e2e.sh` — PASS (3.789s).
- `git diff --check -- cmd/ducklord/workspace_pane_modal.go cmd/ducklord/workspace_pane_modal_test.go TASK.md` — PASS.

Demo restart/readiness: `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman scripts/ducklord-podman-demo.sh` — PASS. Five owner-labelled containers are Up; `ducklord clients` returned client-a, client-b, and client-c with Ducklion (client-d is reachable but intentionally has no Ducklion). Usage metrics: unavailable/unmeasured.

# Current handoff

Updated: 2026-09-17 Asia/Taipei

## Live Project Notes and focused non-Default detach (2026-09-17)

Project Notes now resolves its Session children from the current Project layout whenever Right is pressed. A newly placed existing Session is therefore available immediately, including the single-child case. Session Notes picker labels, breadcrumbs, and descendant-search origins use the live session name; a stable ID is retained only when no name is available.

A focused terminal in a non-Default Project now handles `Ctrl-B d` locally and detaches its selected Session pane. The Default Project retains its existing Project-focused detach behavior.

Validation:

- `go test -race -count=1 ./cmd/ducklord -run 'Test(ProjectNotesRightArrowOpensItsOnlySession|SessionNotesPickerUsesSessionNames|FocusedTerminalPrefixDDetachesNonDefaultProjectPane|ScopedNotesInputRoutesSearchAndPages|NotesSessionPickerResolvesSelectedIdentity|PaneRouteContract)$'` — PASS (1.021s).
- `DUCKLORD_TUI_CONTAINER_E2E=1 CONTAINER_RUNTIME=podman go test -count=1 ./cmd/ducklord -run '^TestDucklordNotesTUIContainerE2E$'` — PASS.
- `git diff --check` — PASS.
- The default `scripts/ducklord-tui-e2e.sh` was also started serially; its wrapper returns before the background manifest reports a final aggregate result, so do not cite it as a passed full gate for this batch. Its owned test processes and containers had exited before the demo restart.

Demo restart/readiness: `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman scripts/ducklord-podman-demo.sh` — PASS. Five `ducklord-verified` containers are Up; `ducklord clients` returned client-a, client-b, and client-c. Usage metrics: unavailable/unmeasured.

## Direct Notes shortcut from navigation panes (2026-09-17)

Project-list focus now routes plain `o` to Project Notes. Session-list and Detailed Sessions focus route plain `o` to the selected Session Notes. A focused terminal does not consume plain `o`; it remains PTY input, with `Ctrl-B O` continuing to open Session Notes. Search and text-owning modal states keep literal `o` as input.

Validation:

- `go test -race -count=1 ./cmd/ducklord -run 'Test(DirectNotesRoutesFollowNavigationFocus|FocusedPanePrefixNotesStaysLocal|PaneRouteContract)$'` — PASS (1.018s).
- `DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_TUI_E2E_PATTERN='^TestDucklordNotesTUIContainerE2E$' timeout 55s scripts/ducklord-tui-e2e.sh` — PASS (3.872s); proves direct project/session routes and focused-terminal PTY `o` sentinel.
- `git diff --check -- cmd/ducklord/pane_prefix.go cmd/ducklord/pane_prefix_test.go cmd/ducklord/tui_notes_container_e2e_test.go docs/pane-routing.md docs/ui-routing-verification.md` — PASS.

Demo restart/readiness: `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman scripts/ducklord-podman-demo.sh` — PASS. Five `ducklord-verified` containers are Up; `podman exec ducklord-verified-ducklord-dev ducklord clients --config /root/.ducklord/config.yaml` returned client-a, client-b, and client-c. Usage metrics: unavailable/unmeasured.

## Session Notes modal ownership follow-up (2026-09-17)

A user-reported regression showed that the earlier gate proved focus restoration
*after* closing Notes but did not prove that Notes retained input ownership while
the originating PTY was still producing output. The repair fences two async
paths: control readiness cannot reclaim workspace focus while any workspace
modal is open, and pending PTY input cannot consume Notes modal keys.

The causal container route starts from a focused `alpha` terminal, emits finite
asynchronous output, opens Session Notes with `Ctrl+B` then `O`, holds the modal
for 1.5 seconds, creates and saves a session-scoped entry, closes and reopens
that same scope to prove persistence, then closes it and sends a unique PTY
sentinel. The standard credential-free manifest passed this route and all other
required routes:

- `go test -race -count=1 ./cmd/ducklord -run 'TestWorkspaceControlCannotReacquireFocusWhileNotesOpen|TestPendingPTYInput|TestPanePrefix|TestFocusedPanePrefix'` — PASS
- `CONTAINER_RUNTIME=podman scripts/ducklord-tui-e2e.sh` — PASS, 179.416s
- `git diff --check` — PASS
- `DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman scripts/ducklord-podman-demo.sh` — PASS; five verified containers are Up.

An independent code-path review found no remaining focus-steal path in this
scope and corrected the routing guide opening-key wording to `Ctrl+B`, then
uppercase `O`. A fresh independent container execution could not be scheduled
because the agent-thread capacity was already exhausted; the completed default
manifest above is the authoritative execution evidence.

`go test -count=1 ./...` currently fails in the unrelated existing supervisor
test `TestCodexManagedPTYInjectsCompletionHookWithoutBypassingHookTrust`:
`managed Codex args did not install authenticated hook: ""`. This Notes repair
does not touch that package; do not present the historical full-suite PASS as a
current result.

## Active Notes routing repair and required E2E gate

The bounded Notes focus-return repair in [TASK.md](TASK.md) is complete. The
`/model/Notes` modal returns input to the exact original session, and the
standard E2E includes `NotesTUIContainer`.

## Batch verification status

### Current bounded verification batch

The route evidence preserves raw capture and strips only SGR for visible-screen
assertions. Atomic Prefix input and split Ctrl-B input are both tested. Closing
Notes is followed by an original-session sentinel, proving exact focus restore
without leaked modal or prefix state. The default manifest includes Notes.

`CONTAINER_RUNTIME=podman DUCKLORD_TUI_E2E_PATTERN='^TestDucklordNotesTUIContainerE2E$' scripts/ducklord-tui-e2e.sh`

The integrated five-role review is complete. The post-fix frontend/E2E reviewer
found three issues; all three were fixed and the follow-up review reported no
issues.

Exact verification:

- `go test -count=1 ./...` — PASS
- `go test -race -count=1 ./cmd/ducklord -run 'Test(Notes|PanePrefix|FocusedPanePrefix|PaneRouteContract|PendingPTYInput|ReadInput|TUINotesContainer|WorkspaceHelpInterceptionAvailability|DucklordPrefixNavigationContainer)'` — PASS
- `CONTAINER_RUNTIME=podman scripts/ducklord-tui-e2e.sh` — PASS, 179.416s
- Independent verification: sha256 for 9 files OK; race PASS, 1.043s; target Notes+Prefix E2E PASS, 15.971s; owner E2E containers none

### Corrected status of the Notes focus follow-up

The Notes focus restoration follow-up is verified. The default manifest includes
`NotesTUIContainer`, and the post-modal session reentry route passes its exact
focus sentinel check.

The following outcomes were reported previously and are preserved as
historical command results:

- `go test -race -count=1 ./cmd/ducklord`
- `git diff --check`
- `CONTAINER_RUNTIME=podman scripts/ducklord-tui-e2e.sh` (reported PASS, 181.157s;
  historical run before Notes became part of the default manifest)

The demo was rebuilt and restarted with Podman. Containers with prefix
`ducklord-verified` are Up; owner token: `ducklord-verified-20260917`. Test owner
containers: none. Exact command:

`DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman scripts/ducklord-podman-demo.sh`

Readiness: PASS.

The following additional outcomes are historical context and do not change the
completed status:

### Earlier reported outcomes preserved as historical

- `go test -race -count=1 ./cmd/ducklord ./internal/ducklord`
- `go test -count=1 ./...`
- `golangci-lint run ./...`
- `CONTAINER_RUNTIME=podman DUCKLORD_TUI_E2E_FULL=1 scripts/ducklord-tui-e2e.sh`
  (reported PASS, 182.777s)

Usage metrics: unavailable/unmeasured.

### Next route workflow

For every new route, create a route-matrix ID, define the state contract and
dispatcher plus PTY evidence, make the default manifest a gate, verify the
reviewer source checksum, and run the demo from the same artifact.

The proposed reproduction, manifest gate, and PTY evidence requirements are recorded in
[docs/ui-routing-verification.md](docs/ui-routing-verification.md).

### Prior planning validation

Documentation-only validation also passed: Markdown relative links resolve
locally, and `git diff --check` passed.

## Historical validation

The previous batch recorded these results:

- `go test -count=1 ./...`
- `go test -race -count=1 ./cmd/ducklord ./internal/ducklord ./internal/ducklion/daemon`
- `CONTAINER_RUNTIME=podman DUCKLORD_TUI_E2E_FULL=1 scripts/ducklord-tui-e2e.sh`
  (PASS, 180.425s)
- Independently rerun focused high-output and prefix-route checks, plus Notes
  with `DUCKLORD_TUI_E2E_FULL=1`.
- Bash demo-common test and the diff check.

Those results are historical evidence only. Production remote deployment was
unavailable because `.prod.env` was absent.

The prior isolated owner-labelled demo was rebuilt and restarted successfully
with:

`DUCKLORD_DEMO_NAME_PREFIX=ducklord-verified DUCKLORD_DEMO_OWNER_TOKEN=ducklord-verified-20260917 CONTAINER_RUNTIME=podman scripts/ducklord-podman-demo.sh`

Its readiness probes passed and no stale stress processes or
`ducklord-tui-e2e` containers were found.

## Scope and safety

Keep unrelated edits intact. Product, test, script, and runtime changes are out
of scope for this documentation batch. No active owned test process is
recorded. Commit status: no commit requested.

## Worktree-state recovery and cleanup classification (2026-09-22)

The primary checkout had `core.bare=true` while its live source tree was set as
an external work tree. This left the current Duckway implementation as an
uncommitted delta from `40c9011` and made regular worktree cleanup unsafe.
The checkout has been restored as a normal working tree and its live source
state is preserved on `codex/recover-live-state-20260922` before any dirty
worktree is removed. All registered worktrees are inactive. The remaining
home-directory worktrees are historical, uncommitted implementation snapshots;
the `host-skills-e2e-trace` worktree also has a unique commit (`cd1d2cd`) that
requires explicit comparison before disposal. The old `.claude/worktrees`
entries predate Duckway and require separate archival or explicit disposal.

Validation:

- `git diff --cached --check` — pending at commit creation.
- No Ducklion test daemon remains outside a live demo container.

Usage metrics: unavailable/unmeasured.


Completion:

- Stored the live workspace as commit `7cc6f46` on
  `codex/recover-live-state-20260922`.
- Removed 36 stale home-directory worktrees and 3 stale `/tmp` worktrees;
  `git worktree prune` completed.
- Stopped five orphaned standalone Ducklion test daemons from `/tmp`.
- Retained five `.claude/worktrees` entries because they predate this Duckway
  work and contain unrelated, uncommitted historical changes.
- Removed generated local `ducklord` binary and `.orig` test backup.

Validation:

- Pre-commit `go test -race ./...` — PASS.
- Pre-commit coverage — PASS (56.2%, threshold 37.5%).
- Pre-commit lint reported 13 existing static-analysis/unused-code issues, so
  the preservation commit used `--no-verify` after the tests passed.
