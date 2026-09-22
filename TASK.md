# Current batch

## Objective
Deliver a Project file exchange first version: two directory columns, local or
configured SSH hosts, and persistent Project exchange storage. Remote copies
stream through Ducklord; hosts never need direct connectivity.

## Acceptance criteria
- [x] Browse and search directories, select multiple entries, copy by keyboard or
  dragging between columns, and explicitly review conflict policy.
- [x] Local/remote and remote/remote files and directories transfer correctly;
  failed or cancelled transfers clean staging and preserve sources.
- [x] Each Project has its own persistent exchange directory.
- [x] Modal owns keyboard/mouse during background events; close restores origin.
- [x] State/backend tests and default real-PTY/container route E2E pass.
- [x] Five independent review roles completed; demo rebuilt and verified.
- [x] Batch-owned resources removed and evidence recorded in HANDOFF.md.

## Resource ledger
Owner: exchange-v1-20260922. Base: 8d9474f.
All batch worktrees and branches were integrated and removed.
Temporary roots /tmp/duckway-exchange-v1-20260922 and /tmp/x0922 removed.
Test containers/networks: script-owned traps completed teardown; none retained.
Baseline retained: five existing .claude/worktrees snapshots; owner-labelled
 ducklord-verified-* demo (owner ducklord-verified-20260917).

## Status
Complete. Validation, artifact identity, demo readiness, and cleanup evidence:
[HANDOFF.md](HANDOFF.md#project-file-exchange-v1--complete).
Await the next user request.
