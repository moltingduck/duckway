# Current batch

## Objective
Make Project file exchange visually clear: independent host panels, directional
copy feedback, per-file highlights, and project-scoped recent transfer history.

## Scope
Build on verified v1 at db2b0df. Preserve streaming via Ducklord and copy semantics.
Specification: docs/file-exchange-visual-design.md; route.file-exchange.

## Acceptance criteria
- [x] Independently bordered cyan/purple panels show host, path, active side,
  filter, selection counts; folder icons have an ASCII fallback.
- [x] Preview and busy states show real source/destination and direction; truthful
  per-item progress and copied/skipped/failed/cancelled/not-started outcomes.
- [x] Latest-batch highlights bind to endpoint and full path; renamed targets are
  explicit, success follows commit, sources remain intact.
- [x] Per-project last 20 batches survive modal reopen; history navigation restores
  the active browser side; narrow layouts and mouse geometry remain consistent.
- [x] Focus/async state tests, backend progress tests, and real two-host PTY E2E
  verify outcomes, history, modal ownership and terminal restoration.
- [x] Five integrated review roles clear findings; full tests/race/lint, coverage,
  routing manifest and default container E2E checked on integrated artifact.
- [x] Demo rebuilt/restarted, readiness and artifact identity checked; commit/push.
- [x] Owned resources cleaned; handoff contains evidence and usage availability.

## Resource ledger
Owner: exchange-visual-0923. Base: db2b0df9d43dec1fbf927d0c57421187872fd28d.
All UI/backend/E2E and correction worktrees/branches were inspected, integrated,
removed and pruned. E2E f3e81b2 is preserved in main as 0036366.
Focused r10 and default1 scripts exited 0 including teardown; no owned containers,
networks, workers, sockets, task roots, generated binaries or patch backups remain.
Demo and hook-check temporary directories were removed by script EXIT traps.

Preserved baseline: five .claude/worktrees/agent-*, three
dw-*-1788855706-1278453 containers, seven old E2E networks and 25 older setup logs.
Retained/restarted demo: ducklord-verified controller + clients a-d;
owner ducklord-verified-20260917. Do not delete baseline resources.

## Status
Complete implementation at 0036366, pushed to origin on
codex/recover-live-state-20260922. Final documentation records verification.
Full Go/race, affected focused checks, 56.9% coverage, 21-route manifest,
portability/lock/hook checks and five review roles completed.
Focused real E2E passed (26.096s); full default E2E passed (269.939s), both scripts
exited 0 including teardown. Lint has 13 unrelated baseline findings; no new
batch findings remain. Demo rebuilt/restarted (exec 68697, exit 0), readiness
and client probes passed; three binary hashes match the tested fixture exactly.
See HANDOFF.md for commands, coverage scope, artifact hashes and final resource scan.
Usage metrics unavailable/unmeasured; recorded in docs/agent-workflow.md.
