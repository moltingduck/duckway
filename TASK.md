# Current batch

## Objective
Make resource ownership and cleanup a required Duckway development completion
gate, so worktrees and test artifacts do not accumulate after a batch.

## Acceptance criteria
- [x] Agent rules define resource ownership, scoped temporary storage, teardown,
  worktree disposal, and a final cleanup gate.
- [x] The reusable workflow defines a resource ledger, non-destructive exit scan,
  and handoff evidence requirements.
- [x] Restart the demo and record readiness plus cleanup evidence in HANDOFF.md.

## Status
Complete — the cleanup gate, resource ledger, exit scan, and handoff template
are documented; the owner-labelled Podman demo restarted and passed readiness.
