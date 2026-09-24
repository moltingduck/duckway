# Agent workflow and token usage

Use [AGENTS.md](../AGENTS.md) for standing instructions,
[TASK.md](../TASK.md) for the current batch, and [HANDOFF.md](../HANDOFF.md) to
resume work. Keep these short; existing specifications and verification documents
remain the detailed evidence. Read this procedure when planning review or tests.

For UI routing fixes, follow the proposed evidence and gating workflow in
[`docs/ui-routing-verification.md`](ui-routing-verification.md). It defines the
route contract, required test manifest, PTY checks, artifact identity, and
reporting requirements. The workflow is a plan until its automation is
implemented; do not treat opt-in or text-only Notes checks as proof of focus
restoration.

## 1. Bound each batch

Write the objective, affected area, acceptance criteria, and required checks before
substantial work. A batch should produce one reviewable outcome. Split independent
outcomes into sequential batches without dropping any part of the user's request.
Routine implementation choices do not need new permission.

When resuming, inspect the actual diff and read the handoff plus relevant source.
Do not replay the entire conversation, repeat completed audits, or infer new tasks
from old "next steps" that later checkpoints superseded.

## 1a. Develop independent work in worktrees

Use worktrees to isolate independent implementation batches, not as a default
wrapper around every subagent. A worktree isolates Git files; it does not isolate
ports, processes, containers, sockets, databases, caches, or `/tmp` files.

Create one directory and branch per bounded subtask, beside the main checkout:

```bash
git worktree add ../duckway-<batch>-<role> -b codex/<batch>-<role> <base-commit>
```

The subagent brief must include the base commit, exact scope, acceptance criteria,
allowed checks, runtime resources it must use, and a request to report its branch
and process IDs. The main agent reviews status and diff before integrating. Do not
have two worktrees modify the same file or branch. Integrate sequentially when
changes overlap, then remove completed worktrees:

```bash
git worktree remove ../duckway-<batch>-<role>
git worktree prune
```

## 1b. Own and dispose of development resources

Every batch starts with a resource ledger. Record a short owner token and every
resource the batch creates: worktree and branch, temporary root, build output,
log directory, PID, port, socket, container, and fixture store. Record a
pre-existing resource as a baseline; never delete it merely because it appears
in a scan. A running demo may remain only when the handoff names it as retained.

Before creating a worktree, verify that the primary checkout is a normal Git
working tree:

```bash
git rev-parse --is-bare-repository  # must print false
git status --short
git worktree list --porcelain
```

Use an owner-specific ignored temporary root and give every daemon/container a
matching name or label. Shell scripts must clean their owned root and child
processes on `EXIT`, `INT`, and `TERM`. Go tests must use `t.Cleanup` immediately
after acquiring each resource, so cleanup also runs after setup failures. Do not
write generated binaries, coverage files, patch backups, sockets, or test logs
to the source tree, home directory, or an unscoped shared `/tmp` path.

At integration, inspect a subagent worktree before disposal. Commit, integrate,
or explicitly abandon the reviewed result first; then remove the worktree, prune
Git's registry, and remove its branch when it is no longer needed. Never use a
forced worktree removal to hide an unreviewed diff.

Run this non-destructive exit check before claiming a batch complete. Use the
recorded owner token and exact IDs rather than broad `pkill`, global container
removal, or deletion of unknown temporary directories:

```bash
git status --short
git worktree list --porcelain
ps -eo pid,ppid,args
podman ps --all --format '{{.ID}} {{.Names}} {{.Labels}}'
```

The handoff must state: the baseline resources intentionally retained, resources
created by the batch, the cleanup command or teardown owner, and evidence that
each owned resource is gone. A leftover owned process, container, socket,
temporary root, generated artifact, or worktree is a failed batch even if its
product tests passed.

### Parallel test and runtime rules

- Run tests in parallel only when they have independent packages, ports, fixture
  roots, callback state, and daemon/container names.
- Tests that use a real daemon, host hooks, live callbacks, shared Podman names,
  fixed ports, or common fixture directories run serially unless the test itself
  provides unique namespaces. Host-hook configuration E2E and live-agent tests
  must not run concurrently on the same host.
- Assign each parallel test a unique temporary root, project/session IDs, port
  range, container name, and log path. Pass these explicitly through environment
  variables or test flags; do not rely on the current working directory alone.
- Do not run two tests against the same durable runtime or shared database. A
  worktree's teardown must terminate every process and remove every fixture it
  created, including on setup failure and interruption.
- Before starting a parallel test batch, record the resources and PIDs it owns.
  After it completes, verify those PIDs, sockets, containers, and fixture roots
  are gone. A leftover resource blocks the next batch and is a test failure.
- If a test fails while another is using shared state, stop the batch, preserve
  the failure logs, clean up owned resources, and rerun the affected checks
  serially before diagnosing the product code.
- Parallelism is a scheduling choice, not a correctness requirement. Prefer one
  reliable serial run over concurrent runs with ambiguous cross-test failures.

Task template (replace the root task contents for a new request):

```markdown
# Current batch
## Objective
<One observable outcome requested by the user>
## Scope
<Affected area, relevant spec, dependencies, existing edits to preserve>
## Acceptance criteria
- [ ] <Observable behavior or deliverable>
- [ ] <Required verification>
## Status
<In progress / blocked with reason / complete with evidence link>
## Next batch
<Remaining authorized outcome, or await next user request>
```

## 2. Review the integrated change

For substantial code changes, apply `duckway-parallel-review` with these repository
timing rules: finish the bounded implementation first, then review its combined
diff. Keep its applicable safety, testing, and delivery requirements. If the user
explicitly requests early reviews or another sequence, follow that request.

Cover five roles: security, code quality, race/concurrency, frontend/E2E, and backend
API. Respect available concurrency slots; queue roles when needed. A role without
relevant changes may report "not applicable" with a reason instead of inventing
work. Documentation-only and small, low-risk edits use direct local review.

Give each reviewer a self-contained brief with `fork_turns: "none"`:

```text
Role and question: <specific risk to examine>
Scope: <files and relevant specification sections>
Baseline: <commit plus included working-tree changes; exclude unrelated edits>
Acceptance criteria: <expected behavior>
Already verified: <commands, results, and evidence paths>
Return: actionable findings with file/line, impact, and suggested check;
        say explicitly if no findings. Do not edit or run broad tests unless assigned.
```

The main agent integrates findings and records each as fixed, deferred with a
reason, or not applicable. Send changed files and the fix summary back only to
affected reviewers. Start another broad review only if changes introduce broader
risk. Avoid having every reviewer run the same suite or reread all project history.

## 3. Verify according to the change

### Notes pane completed batch (2026-09-16)

Verified current-project `prefix+o`, per-project `notes.md`, j/k/Enter
body-only native system clipboard with OSC 52 terminal fallback, editor fallback and immediate redraw, safe 0600 no-follow
descriptor storage, and Esc navigation return. All non-`/tmp` Go packages,
Notes container E2E, race tests for the two affected packages, lint (0 issues),
and `git diff --check` passed. Five review roles and three affected final
reviews completed with no findings. The Podman demo rebuilt successfully and is
ready. Full `./...` and vet are blocked only by pre-existing
`tmp/fixture-failure-overlay.go` references to absent symbols. Usage unmeasured;
no commit.

| Change or stage | Verification |
| --- | --- |
| Documentation only | Diff review, `git diff --check`, local link checks |
| Local code iteration | Relevant existing tests; add meaningful regression coverage for behavior changes |
| Shared state, goroutines, lifecycle | Relevant race tests in addition to functional tests |
| Integrated substantial code batch | Required repository/skill full tests and lint once on the integrated result |
| TUI or container behavior | Relevant existing Podman E2E scenarios |
| Provider authentication or live interaction | Relevant live tests when required to establish correctness |

Use existing repository commands and test names. See the
[developer guide](developer-guide.md#testing) and
[pane verification evidence](pane-verification.md) for relevant examples.
Do not repeat provider-consuming tests for unrelated documentation or UI changes
unless a concrete dependency requires them. Never claim unrun tests passed.

A changed dependency, failing test, or unresolved concern can justify broader or
repeated testing. A passed check alone does not justify rerunning it. Required
checks remain required; report environment blockers and residual uncertainty.

### Completion gate and progress language

Do not describe a batch as **complete**, **fixed**, or **ready for the user**
until its whole acceptance checklist is closed. A successful targeted test is
progress evidence only. It does not close the batch while any of these remain:

1. required review findings are unresolved or affected reviewers have not
   re-reviewed the repair;
2. a change made after validation has not had its affected checks rerun;
3. the required E2E route or smoke path has not passed on the integrated
   artifact; or
4. a requested demo restart and readiness check has not passed.
5. the final resource ledger has not been checked, or it still contains a
   batch-owned worktree, process, container, socket, temporary root, generated
   artifact, or unreviewed branch.

Use explicit status words in updates: **in progress** for implementation,
**partial validation passed** for a subset of checks, **blocked** with the
exact missing evidence, and **complete** only after this gate. Before a final
user report, compare the final diff to the test/review artifact, confirm every
acceptance item in `TASK.md`, restart the demo when requested, and record the
exact results in `HANDOFF.md`. If a reviewer finds a defect after an earlier
progress update, downgrade the status to in progress, repair it, and repeat
only the checks affected by that repair.

Host-hook configuration E2E and live-agent tests must not run concurrently on the
same host because they share callback state. See the
[pane checkpoint](pane-implementation-status.md) for the recorded failure.

## 4. Limit repeated context and tool output

- Search relevant directories first and expand only as needed. Read bounded file
  ranges. Batch independent reads and searches; keep dependent edits sequential.
- Keep large command output in ignored `tmp/` files when needed. Return commands,
  exit status, counts, and relevant failure excerpts. Do not persist credentials
  or raw live transcripts as routine debugging evidence.
- Track the tool session ID or owned PID of long-running work in the handoff.
  Use the tool's completion notification or bounded waits of at most 60 seconds;
  avoid repeated immediate polling. Give a concise progress update during long work.
- After a completed batch, write a handoff suitable for a fresh session. Do not
  repeatedly reload detailed history just to continue a small follow-up.

Handoff template:

```markdown
# Current handoff
Updated: <date and timezone>
## Current batch
<Task link, branch, baseline commit, outcome>
## Decisions
<Only decisions needed to continue; link detailed evidence>
## Preserve existing edits
<Unrelated dirty files and ownership constraints>
## Validation and processes
<Exact commands, results, unrun checks, owned session IDs/PIDs or none started>
## Resource ledger and cleanup
<Baseline retained resources; batch-owned worktrees, temp roots, PIDs, containers,
and logs; disposal evidence; retained demo if applicable>
## Next action
<Next authorized step, blocker, or await next user request>
```

## 5. Measure completed outcomes

At completion, record the batch identifier and available measurements below.
Use session usage records or an existing usage report; do not reread all historical
logs on every turn merely to populate this table. Record source and covered session
IDs when metrics are available. Missing metrics are **unmeasured**, not zero.

Count input, cached input, and output separately. Only derive noncached input as
`input - cached input` when the source defines input as including cached tokens.
Do not sum cumulative snapshots: use per-call increments or consistent start/end
deltas, without also counting the same child session in an aggregate. Raw token
counts are not monetary cost or subscription quota consumption.

| Batch | Completed outcome | Calls | Input / cached / output tokens | Subagents | Repeated checks | Source |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-09-24 pg-blob-0924 | PostgreSQL migration32 BYTEA fix; actual admin restart and demo ready | Unmeasured | Unmeasured | 2 | Fixture failure repairs, actual PostgreSQL race tests and public-extension variant | HANDOFF.md; token usage not collected |
| 2026-09-15 demo refresh | Standing restart instruction and current demo ready | Unmeasured | Unmeasured | 0 | No runtime test reruns; startup/readiness checks passed | TASK.md and HANDOFF.md; token usage not collected |
| 2026-09-15 tab navigation | Prefix+n/p and empty Project Enter | Unmeasured | Unmeasured | 0 | No repeated checks; container E2E blocked by existing fixture | TASK.md and HANDOFF.md; token usage not collected |
| 2026-09-15 workflow | Repository instructions and batch/handoff procedure | Unmeasured | Unmeasured | 0 | No runtime tests or reruns | Current task; token usage not collected |

| 2026-09-15 contextual config/help | Target-specific right-click forms, focus-aware help, tab navigation verified | Unmeasured | Unmeasured | 5 | Lint style fix; affected E2E reruns after test focus/layout fixes; required pre-commit race/coverage/lint | HANDOFF.md; token usage not collected |

| 2026-09-15 Codex flicker | Complete buffered synchronized TUI frames; demo restarted | Unmeasured | Unmeasured | 0 | No repeated checks; unit and two-live-pane E2E passed | HANDOFF.md; token usage not collected |

| 2026-09-15 six config targets/colors | Right-click and prefix+c menus, persisted themes, demo restarted; 01be868 pushed | Unmeasured | Unmeasured | 5 | Affected unit/E2E reruns after focus fixes; required pre-commit race/coverage/lint | HANDOFF.md; token usage not collected |

After at least three comparable batches, compare tokens and calls per completed
outcome, repeated checks, and defects found after delivery. Set a soft warning
threshold from that baseline if useful. These Markdown rules do not enforce a hard
token cap; do not promise a savings percentage before measurement or truncate
required work to hit a target. Goal creation and budgets require explicit user
instructions, and completion must reflect the actual agreed objective.

| 2026-09-15 incremental frames/native copy | Changed-row rendering, frozen native selection; 40c9011 pushed; demo ready | Unmeasured | Unmeasured | 5 | Lint escape correction; required pre-commit race/coverage/lint | HANDOFF.md; usage not collected |

| 2026-09-15 fixture cleanup/routes | 670 shells + 672 orphan runtimes removed; ASCII routes; demo restarted | Unmeasured | Unmeasured | 0 for redirected batch | Links/diff/readiness passed; stress rerun stopped at user request | HANDOFF.md; usage not collected |

| 2026-09-15 fixture lifecycle fix | Test-only owned-runtime teardown; zero leaked fixtures; demo ready | Unmeasured | Unmeasured | 0 | Repeated E2E, injected failure, scoped race/lint/process scan passed | HANDOFF.md; usage not collected |

| 2026-09-15 lower-tier delegation policy | Prefer lower-tier models for simple bounded subwork; demo ready | Unmeasured | Unmeasured | 0 | Diff check and readiness passed | HANDOFF.md; usage not collected |
| 2026-09-18 quick shell prefix | Focused `Ctrl-B --`, `Ctrl-B \\`, and `Ctrl-B tt` duplicate a live shell on its host/CWD; route and PTY E2E covered | Unmeasured | Unmeasured | 5 | Quick-shell lifecycle and E2E fixture fixes; focused/race/package checks rerun | TASK.md and HANDOFF.md; token usage not collected |

| Batch | Completed outcome | Calls | Input / cached / output tokens | Subagents | Repeated checks | Source |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-09-22 Project file exchange v1 | Two-column SSH exchange and persistent Project shelf; demo rebuilt with E2E-matching binaries | Unmeasured | Unmeasured | Unmeasured (three implementation scopes and five review roles, with follow-ups) | Focused reruns after actual modal, archive, filter, and asynchronous selection defects; final default PTY suite 31 passed / 1 intentional skip | HANDOFF.md; no usage records available |

| 2026-09-23 exchange visual feedback | Independent colored panels, directional results and project transfer history; integrated code 0036366 | Unmeasured | Unmeasured | Unmeasured (isolated implementation scopes and five review roles, with follow-ups) | Focused E2E reruns after render/selection fixture repairs; final focused and full default PTY suites passed | TASK.md and HANDOFF.md; batch token records unavailable |

| 2026-09-24 Help and operation audit | Searchable feature catalog, truthful context hints and repaired Help mouse ownership; demo matches verified binaries | Unmeasured | Unmeasured | Unmeasured (isolated implementation and five review roles with follow-ups) | Actual Help failures drove focused repairs; final 31 default PTY tests passed / one intentional skip; final affected normal/race passed | HANDOFF.md; batch usage records unavailable |

| 2026-09-24 scrolling and Darwin build | Three-pane scrolling and portable peer credentials; demo matches default E2E artifacts | Unmeasured | Unmeasured | Unmeasured (isolated implementation and five review roles with follow-ups) | Actual PTY failures drove routing/fixture fixes; final 32 passed / one intentional skip, 12 cross-build targets passed; prior intermittent failures retained as uncertainty | TASK.md and HANDOFF.md; usage records unavailable |
