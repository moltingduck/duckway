# Current batch

## Objective
Audit every Ducklord TUI operation against its actual dispatcher; include missing
features in `?` help and correct stale/misleading action messages and footers.
Batch: help-audit-0924. Base: f41677eee0aabb5f631255f10ba0c1c124e0711a.

## Acceptance criteria
- [x] Inventory all TUI routes, modal/form operations and action hints against source.
- [x] Searchable help covers supported features, context/keys and important constraints.
- [x] Action messages/footers agree with real handlers, configured shortcuts and scope.
- [x] Regression checks cover completeness and corrected hints; actual help PTY routing passes.
- [x] Independent integrated review, applicable full tests/lint and default routing E2E pass (baseline findings documented).
- [x] Demo rebuilt/restarted and readiness verified; changes committed/pushed.
- [x] Owned resources disposed; evidence and measured/unmeasured usage recorded.

## Resource ledger
Owner help-audit-0924. Planned sibling worktrees: ../duckway-help-audit-0924-ui
(branch codex/help-audit-0924-ui) and ../duckway-help-audit-0924-hints
(branch codex/help-audit-0924-hints), removed after reviewed integration.
Transient logs/output under ignored tmp/help-audit-0924, removed after summaries.
Container E2E prefix help-audit-0924-e2e, script EXIT teardown required.
No ad-hoc daemon or stress shell. Agents must report owned processes and cleanup.
Repair worktree ../duckway-help-audit-0924-focus, branch
codex/help-audit-0924-focus, owner help-audit-0924, base 42b3f43;
remove after reviewed integration. Worktree checks use ignored tmp/help-audit-0924.
Retained demo ducklord-verified (owner ducklord-verified-20260917) will be rebuilt.
Baseline preserved: five .claude/worktrees/agent-*; three dw-*-1788855706-1278453
containers; existing demo, seven old E2E networks and 25 older setup logs.

## Status
Complete through integrated code ff2cbcd and the final documentation checkpoint.
Inventory and five-role review complete. Full tests/race/coverage and final
affected package checks passed; lint retains 13 documented baseline findings.
Full default container E2E PASS: exec 53657, 268.387s, 31 passed / one intentional
legacy skip. Demo restart PASS: exec 53616; deployed hashes match E2E binaries.
Final resource scan clean for this batch; baseline and named demo preserved.
Publish this checkpoint and confirm remote HEAD before reporting completion.
See HANDOFF.md and docs/help-operation-audit.md for evidence and coverage.
