# Completed batch: pane-scroll-0924 / build-dist-0924

## Objective and scope
Repair the reported Darwin client-dist compilation failure and finish authorized
Help, Project list and Session list overflow scrolling. Base: 48f0fac5086d3be2473b7fdffe60f7b6b13c90ab.
Integrated implementation through 72b6099. Main planned/integrated/verified;
lower-tier subagents implemented in isolated worktrees.

## Acceptance and evidence
- [x] Platform-specific peer credentials/process identity; all twelve Docker
  client-dist Linux/Darwin amd64/arm64 targets compile under Go 1.25 (13042).
- [x] Help arrows/page/search/wheel/track/drag and exact origin restoration;
  Project/Session list gestures move selection/preview without PTY input.
- [x] Regression tests, updated routing specifications and 22-route manifest.
- [x] Five integration review roles plus affected follow-ups complete.
- [x] Full normal/race/coverage checks passed; final affected normal/race passed;
  default container E2E 92277: 32 PASS, 1 intentional legacy SKIP, 297.289s.
- [x] Demo rebuilt/restarted and readiness passed (49566); binary hashes match E2E.
- [x] Owned temporary roots/worktrees/branches/processes/containers removed;
  named persistent demo retained. Final evidence recorded in HANDOFF.md.

Lint is NOT clean: final full lint 47308 retains thirteen pre-existing findings;
this batch's added QF1003 was removed at 72b6099. Native Darwin runtime remains
unavailable. Earlier intermittent prefix navigation / initial output activation
failures were not causally diagnosed; the final passing run does not establish
that every timing defect is eliminated. e192366 fixes a separately demonstrated
quick-shell placement/old-control teardown identity loss.

## Resource ledger and disposal
Owners: pane-scroll-0924 and build-dist-0924. Removed sibling worktrees and
branches: pane-scroll-0924-{ui,e2e,focus,lint}, build-dist-0924-fix. Worktree
results reviewed and integrated before removal; git worktree prune completed.
Ignored tmp/pane-scroll-0924 and tmp/build-dist-0924, logs, generated binaries,
coverage, sockets, and exclusive short-path test exception /tmp/9 are absent.
E2E containers/network and build-dist-0924-go125 container removed. Final scan
found no owned executable process. Temporary resources used EXIT/t.Cleanup.

Retained: five ducklord-verified containers and their network, owner
ducklord-verified-20260917. Preserved baseline: five .claude worktrees, three
dw-*-1788855706-1278453 containers, seven older E2E networks and older setup logs.
Usage calls/tokens/cache and total subagent count: unmeasured.

Launch:
`podman exec -it ducklord-verified-ducklord-dev ducklord tui --config /root/.ducklord/config.yaml`
