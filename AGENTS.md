# Duckway agent instructions

## Start and scope

- Read `TASK.md` and `HANDOFF.md` first. Read only the specification sections and
  source files needed for the current request; do not reload full session history.
- Treat the user's latest request as authoritative. A completed task file is a
  checkpoint, not a reason to stop: update it for the next requested batch.
- Define one bounded batch with observable acceptance criteria. Complete the
  authorized work without asking for approval at each implementation step.
- Preserve unrelated working-tree edits, including deletions. Stage only files
  belonging to the task if committing is requested or required.
- Do not reopen completed features or broaden audits without a concrete defect,
  changed requirement, or dependency needed to finish the request.

## Context and delegation

- Search narrowly, read relevant ranges, and batch independent reads. Expand the
  search when evidence requires it; do not repeatedly dump entire files or logs.
- Use subagents only when the user or an applicable workflow calls for them and
  there is a bounded, independent task. Supply explicit files, scope, and output.
- For simple, bounded work that can be independently checked—such as narrow code
  searches, documentation review, test-log triage, or an initial focused review—
  prefer an available lower-tier model. Keep architecture decisions, changes that
  cross component boundaries, integration, and final verification with the main
  agent. Choose a stronger model when the task's risk or ambiguity warrants it.
- Use `fork_turns: "none"` with a self-contained brief. Reuse a reviewer for fixes
  in the same scope; give a new task a fresh brief.
- For substantial Duckway code batches, use the five review roles described in
  `docs/agent-workflow.md` at the integration checkpoint. This repository changes
  the timing of the duckway-parallel-review skill's early reviewer launch: review
  the integrated batch, then request only affected follow-up reviews. Preserve
  its applicable coverage and verification requirements.
- Documentation-only and small, low-risk changes do not trigger five reviewers.

### Subagent worktrees

- Use a separate Git worktree for a subagent when it will make code changes, run
  long tests, or need an isolated branch. Read the current `TASK.md` and
  `HANDOFF.md` before creating it.
- Create worktrees beside the checkout, never inside it. Use a unique branch and
  directory based on the bounded batch, for example:
  `git worktree add ../duckway-<batch>-<role> -b codex/<batch>-<role>`.
- Give each subagent one bounded scope and tell it the base commit, files it may
  change, acceptance criteria, and checks it owns. Use `fork_turns: "none"` so
  the worktree does not inherit an unrelated conversation.
- Do not let two worktrees edit the same files or share a branch. The main agent
  owns integration, conflict resolution, and the final verification.
- Keep worktree-local build output, logs, sockets, databases, and fixture data
  isolated. Never assume that separate Git directories isolate runtime state.
- Before merging, inspect each worktree's status and diff, run focused checks in
  the worktree, then integrate one bounded batch at a time. Remove the worktree
  and prune its branch only after the result is integrated or explicitly
  abandoned.
- Worktrees do not reduce model usage by themselves. Parallel subagents can
  increase token use, so use them only when the work is genuinely independent.

### Resource ownership and disposal

- Before creating a worktree, test runtime, container, daemon, socket, temporary
  root, build output, or log, record its owner token and intended disposal in the
  current task or handoff. A persistent demo is an exception only when it is
  explicitly named as retained.
- Confirm the checkout is usable before creating worktrees: `git rev-parse
  --is-bare-repository` must report `false`, and `git status` must succeed.
  Repair or recover a broken checkout before doing more work; do not leave a
  bare repository or redirected worktree as the next agent's starting point.
- Put transient data under a uniquely named, ignored task root. Scripts must
  remove it with an `EXIT INT TERM` trap; Go tests must use `t.Cleanup` for
  every process, socket, container, fixture, and temporary directory they own.
  Do not place ad-hoc artifacts in the repository root, home directory, or a
  shared `/tmp` location without an owner-specific name.
- A subagent report must identify its branch, worktree, runtime IDs/PIDs, and
  cleanup result. The integrating agent owns disposal: inspect and preserve a
  worktree's result before removal, then remove the worktree, delete an
  integrated or abandoned branch when appropriate, and run `git worktree prune`.
  Never force-remove unreviewed or unrecovered work.
- At batch completion, inspect the resource ledger and prove that owned
  processes, containers, sockets, temporary roots, generated binaries, coverage
  files, and patch backups are gone. Clean only resources owned by the batch;
  record pre-existing resources as a baseline instead of deleting them.

## Verification and handoff

- After every completed batch, rebuild and restart the demo with the latest changes
  so the user can inspect them. This is a standing user instruction; no repeated
  confirmation is needed. Verify readiness and provide the TUI launch command.
  If restart fails, report the concrete blocker instead of claiming it is ready.

- Follow the risk-based test sequence in `docs/agent-workflow.md`. Run required
  checks; repeat a passed check only after relevant changes or new evidence.
- Report concise failure excerpts and test summaries. Keep raw logs in ignored
  local storage; never copy secrets, credentials, or raw live agent transcripts.
- Track long-running process IDs and use bounded waits, not rapid empty polling.
- Update `HANDOFF.md` at meaningful checkpoints with decisions, exact validation,
  remaining work, and owned processes. Link evidence instead of copying it.
- Treat cleanup as a completion requirement: include the final resource scan and
  the retained demo, if any, in `HANDOFF.md`. Do not report completion while a
  batch-owned worktree or runtime artifact remains.
- Record available usage metrics per completed batch as described in the workflow;
  mark unavailable values as unmeasured, never zero or an estimate presented as fact.
- Create a goal only on explicit request. Keep an existing goal's agreed scope;
  never mark unfinished work complete to satisfy a token target.
