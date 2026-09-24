# UI routing verification plan

## Purpose and current status

This plan defines the evidence required to fix and verify UI routing failures
such as the Notes focus and reentry bug. The standard container E2E command now
always includes `NotesTUIContainer`; it enables the Notes fixture explicitly
and uses a post-close PTY sentinel to prove that input returned to the exact
originating terminal. The default manifest also includes a Help container E2E
row, alongside its state-router contract and
`TestWorkspaceHelpInterceptionAvailability` coverage. Help opens and closes
only with `?`; Esc and Ctrl-C leave it pinned.

The Help scenario also searches for a previously omitted operation, pins the
results and clears the search before closing. Catalog and hint regression tests
complement this PTY proof; use the [operation audit](help-operation-audit.md)
to keep child dialogs and configured shortcuts covered.

A targeted `DUCKLORD_TUI_E2E_PATTERN` is useful while developing one test, but
is not acceptance evidence for the complete route manifest. The default command
below is the required gate. A current-screen text match alone is never enough:
the test must also show the causal input transition, focus ownership, input
delivery, restoration, and absence of unintended modal or prefix leakage.

## Required workflow for a routing fix

### 1. Reproduce before changing code

Build and record an immutable checksum for the old build. Reproduce the
original report through a real PTY regression scenario using a credential-free,
synthetic fixture before editing the implementation. The reproduction must
identify the focused origin (project or session terminal), opening key, modal
state, close or back action, and the observable failure. Store only redacted
screen and event logs plus the exit status in ignored local evidence storage;
never save raw live-agent transcripts.

The reproduction is a prerequisite for the fix. If it cannot reproduce, record
the exact build, environment, command, and reason rather than treating a text
assertion or a passing smoke check as a substitute.

### 2. Specify the route contract

Every route under test gets a row in the route contract with these fields:

| Field | Required question |
| --- | --- |
| Stable route ID | Which route is being tested, independent of display text? |
| Origin | Which focus, project, session, and tab owns the opening event? |
| Opening key | Which exact key sequence opens the route? |
| Input owner | Which component receives each subsequent key or mouse event? |
| Modal stack | Which overlays are present and in what order? |
| Cancel/back | Which keys close or navigate back, and what state do they restore? |
| Restore/fallback | What happens if the prior session, tab, or pane disappeared? |
| Forbidden side effects | Which create, rename, focus, or session actions must not occur? |
| Test IDs | Which unit, dispatcher integration, and PTY tests prove the row? |

For Notes, the contract must cover Global, Project, and Session scope, origin
breadcrumbs, ambiguity selection, descendant search, and restoration to the
exact originating session. The container E2E establishes the `Project pane:`
focus indicator with the configured project-focus shortcut, then sends direct
lowercase `o` and asserts the Project Notes breadcrumb. It then selects a
session in the Session list and sends direct lowercase `o` for Session Notes
before exercising the focused terminal `Ctrl+B` followed by uppercase `O`
route. Plain `o` remains PTY input while the terminal is focused. While Notes is open,
modal ownership must be held across asynchronous PTY output/control readiness;
the test edits a
session-scoped entry in the modal and proves that the edit persists. Closing
Notes is followed by a post-close sentinel to prove exact focus restoration.
The route specification in
[`docs/pane-routing.md`](pane-routing.md) is the product-facing source to keep
aligned with this table.

The detach route has a project-specific contract: `Ctrl-B d` from a focused
Default Project terminal opens the centered detach-confirm modal, and the modal
owns navigation until it is cancelled or confirmed. From a focused terminal in
any other Project, `Ctrl-B d` detaches directly. The router selects a surviving
pane, waits until that pane owns a control lease, and only then releases the
first input to it. If no pane survives, focus returns to Project navigation.
The source identity is checked at completion so a stale detach cannot transfer
focus or input to another pane. State coverage includes
`TestFocusedTerminalPrefixDOpensDefaultDetachModal`,
`TestFocusedDefaultDetachModalOwnsNavigationAndRestoresFocus`,
`TestFocusedTerminalPrefixDDetachesNonDefaultProjectPane`,
`TestFocusedTerminalPrefixDDetachesNonDefaultProjectFromAtomicPrefix`, and
`TestFocusedTerminalPrefixDSelectsSurvivingPaneForHandoff`; the container
coverage uses `TestDucklordDefaultDetachContainerE2E` and the direct route in
`TestDucklordPrefixNavigationContainerE2E`.

The quick-shell route has a separate timing contract: after `Ctrl-B` and one
of `-`, `\\`, or `t`, the router waits 500 ms. If the same suffix repeats in
that window from a focused live Session, it launches a Shell directly on the
origin host and CWD with an automatic handle and uses horizontal, vertical, or
new-tab placement respectively in the exact Project/tab/pane captured at prefix
entry. Before placement completes, it verifies that node still owns the captured
Session identity; failure must not fall back to another terminal area. The single suffix keeps the existing creation
wizard. Invalid or non-focused origins create nothing and shortcut bytes must
not reach the PTY; a mismatched suffix runs the first ordinary route and then
processes the second input.

### 3. Make the test manifest a gate

Maintain a required, spec-driven manifest mapping every contract row to its
tests and execution mode. The gate fails when a required scenario is missing,
skipped, disabled, or excluded by an environment default. An opt-in test is
not evidence for a required route unless the gate explicitly enables it and
reports that fact.

The manifest must distinguish proposed automation from automation that exists.
The standard E2E script is the current Notes gate; add new required routes to
its default pattern in the same change as their contract row and E2E test.

The quick-shell manifest row is required and names both the fast state/race
coverage and the real PTY route:

The current manifest inventory is the following. IDs and modes must remain
aligned with the executable matrix in [`docs/pane-routing.md`](pane-routing.md):

| Route | State/dispatcher IDs | Default container E2E | Mode / fixture decision |
| --- | --- | --- | --- |
| `route.host-skills` | `TestHostSkillsDualPaneRouteOwnership`, `TestHostSkillsManagementIsScopedToAgent`, `TestHostSkillsRemoteListEventKeepsSkillsInputOwner`, `TestHostSkillsCtrlCCleansAndClosesWholeRoute`, `TestHostSkillsRouteDocumentationContract` | `TestDucklordHostSkillsContainerE2E` | required default manifest; Host list → Host settings → dual-pane Skills; repository/agent-tree ownership, per-agent `none`/`push`/`pull`, selected-target SSH deploy, remote delete, pull preview, and rename migration; Esc/Ctrl-C restore and preview cleanup are required |
| `route.quick-list` | `TestPaneRouteContract`, `TestWorkspaceArrowsCrossStackedLists` | `TestDucklordPrefixNavigationContainerE2E` | required default manifest; direct list input and post-close PTY sentinel |
| `route.detail-list` | `TestPaneRouteContract`, `TestDetailedModeRestoresKeyboardFocus` | `TestDucklordDetailedListContainerE2E` | required default manifest |
| `route.project` | `TestPaneRouteContract` | `TestDucklordWorkspaceProjectEnterFocusContainerE2E` | required default manifest |
| `route.tab` | `TestPaneRouteContract`, `TestPanePrefixLetterTabNavigation` | `TestDucklordPrefixNavigationContainerE2E` | required default manifest |
| `route.session-pane` | `TestPaneRouteContract`, `TestPanePrefixPlacement` | `TestDucklordPrefixNavigationContainerE2E` | required default manifest; direct pane input and post-close PTY sentinel |
| `route.terminal` | `TestPaneRouteContract` | `TestDucklordPaneControlContractContainerE2E` | required default manifest |
| `route.notes.global` | `TestPaneRouteContract`, `TestScopedNotesInputRoutesSearchAndPages` | `TestDucklordNotesTUIContainerE2E` | required default manifest |
| `route.notes.project` | `TestDirectNotesRoutesFollowNavigationFocus`, `TestProjectNotesRightArrowOpensItsOnlySession`, `TestProjectNotesRightArrowListsSessionsAddedAfterOpening` | `TestDucklordNotesTUIContainerE2E` | required default manifest |
| `route.notes.session` | `TestPaneRouteContract`, `TestCloseNotesModalFallsBackWhenOriginSessionDisappears`, `TestWorkspaceControlCannotReacquireFocusWhileNotesOpen` | `TestDucklordNotesTUIContainerE2E` | required default manifest; post-close PTY sentinel required |
| `route.detach` | `TestFocusedTerminalPrefixDOpensDefaultDetachModal`, `TestFocusedDefaultDetachModalOwnsNavigationAndRestoresFocus`, `TestFocusedTerminalPrefixDDetachesNonDefaultProjectPane`, `TestFocusedTerminalPrefixDDetachesNonDefaultProjectFromAtomicPrefix`, `TestFocusedTerminalPrefixDSelectsSurvivingPaneForHandoff` | `TestDucklordDefaultDetachContainerE2E`, `TestDucklordPrefixNavigationContainerE2E` | required default manifest; Default Project confirmation/cancel and direct non-Default handoff wait for the surviving control lease before the first input; no-survivor fallback returns to Project focus |
| `route.context-modal` | `TestPaneRouteContract`, `TestWorkspaceContextConfigTargets` | `TestDucklordPrefixNavigationContainerE2E` | required default manifest; async repaint ownership and close restore |
| `route.help` | `TestPaneRouteContract`, `TestWorkspaceHelpInterceptionAvailability` | `TestDucklordPrefixNavigationContainerE2E` | required default manifest; async ownership and post-close PTY sentinel |
| `route.overflow-scroll` | `TestWorkspaceScrollbarGeometryTracksOverflowAndBounds`, `TestWorkspaceScrollbarTrackPagesThumbDragAndRelease`, `TestHelpScrollMouseDispatcherWheelTrackDragAndRelease` | `TestDucklordScrollContainerE2E` | required default manifest; current terminal cells prove PageUp/PageDown, wheel, track paging, and thumb drag; Help retains search and input ownership through async PTY output, `?` restores the exact origin PTY, and list gestures do not activate rows or leak input |
| `route.create` | `TestPaneRouteContract` | `TestDucklordCreateTUIContainerE2E` | required default manifest |
| `route.quick-shell` | `TestQuickShellPrefixAndPlacement`, `TestQuickShellAsyncTeardownDoesNotRestoreProjectFocus`, `TestQuickShellOriginPinsFocusedSecondTab`, `TestQuickShellPrefixClearsStaleOriginAndBeginRequiresFocus` | `TestDucklordFocusedPrefixQuickShellHorizontalContainerE2E`, `TestDucklordFocusedPrefixQuickShellVerticalContainerE2E`, `TestDucklordFocusedPrefixQuickShellTabContainerE2E` | required default manifest; each sends a post-create PTY sentinel |

Every route row above requires a default-container E2E entry. State and
dispatcher tests supplement that E2E evidence; targeted or opt-in coverage
cannot satisfy the default completion gate. If a fixture cannot support a
required route, the manifest must fail closed and name the missing isolation.

### 4. Test behavior at three levels

Each route fix needs all applicable levels:

1. Unit tests for route parsing, state transitions, modal stack behavior, and
   fallback selection.
2. Real dispatcher integration tests proving the event reaches the intended
   owner and does not trigger forbidden side effects.
3. Container PTY behavior tests proving the user-visible sequence end to end.

Assertions must cover the selected entry and its content effect through a
post-input transition or other causal change, input ownership, and absence of
PTY input leakage. Every open modal must retain input ownership while an
asynchronous repaint or fresh PTY output arrives. After closing Notes, send a sentinel to the
exact original session and prove that it receives it. Prove that no unintended
create modal opens. Assert the current screen after input, rather than relying
on stale transcript text.

Exercise each event using separately delivered and combined or split escape
sequences. Cover 80x24 and a wide terminal. Cover keyboard and mouse paths for
existing routes. Repeat open and close cycles, and repeat with the originating
session disappearing to verify fallback behavior.

### 5. Review and artifact identity

An independent reviewer reproduces the original failure and verifies the route
contract and test evidence. The reviewer records the build checksum used for
the regression test, E2E run, and demo. The E2E and demo must use the same
immutable artifact; a rebuilt or restarted demo is not evidence for a different
binary.

Restart the demo from that artifact and run the same smoke path after the E2E
run. Keep process IDs, container names, ports, fixture roots, and log paths
isolated and bounded. On success or failure, clean up every owned process and
fixture, and record the cleanup result.

### 6. Report evidence without overclaiming

The evidence report records the source checksum, exact commands, enabled
manifest rows, pass/fail/skip status, PTY dimensions, event encoding, captured
assertions, reviewer result, restart readiness, and cleanup status. It labels
unmeasured values as unmeasured. A default run that excludes Notes, a text-only
assertion, or a commented-out scenario cannot be reported as verification of
the Notes focus route.

### 7. Completion gate

For routing work, a focused route E2E passing is **partial validation**, not a
completion claim. Report the work as complete only when the final integrated
artifact has passed every required contract row, all review findings affecting
the route are fixed and re-reviewed, any checks affected by those repairs have
been rerun, and the requested demo restart plus the route smoke path are ready.

Before the final report, compare the artifact used by tests, review, and demo;
check every acceptance item in `TASK.md`; and write commands, results, skipped
checks, and readiness to `HANDOFF.md`. Until then, updates must say which
evidence passed and which completion condition remains open.

## Prioritized batches

1. **Failing Notes reproduction and honest gate.** Capture the original bug on
   the old build through a real PTY, then define the required Notes rows and
   make missing, skipped, or disabled coverage fail the gate.
2. **Minimal Notes focus fix and route regressions.** Implement the smallest
   fix, restore the post-modal reentry scenario, and add the unit, dispatcher,
   and PTY assertions for ownership, sentinel delivery, fallback, and forbidden
   side effects.
3. **Routing matrix expansion in CI.** Extend the manifest and required matrix
   to remaining keyboard and mouse routes, dimensions, escape encodings, repeat
   cycles, and disappearing-session cases.

The focused Notes command recommended for the next verification batch is:

```sh
# Required complete routing gate
CONTAINER_RUNTIME=podman scripts/ducklord-tui-e2e.sh

# Targeted development run only; it does not replace the complete gate.
CONTAINER_RUNTIME=podman \
DUCKLORD_TUI_E2E_PATTERN='^TestDucklordNotesTUIContainerE2E$' \
scripts/ducklord-tui-e2e.sh
```

Record the exact command, source revision, environment, and outcome in
`HANDOFF.md` after each repair batch.

### Host Skills route contract

The required `route.host-skills` branch is `h` from the workspace to Host list,
Enter to Host settings, then `Skills` directly to the dual-pane Host Skills
manager. The left Ducklord managed-repository pane and right expandable client
agent tree are separate input owners, switched with Tab or Left/Right. Enter or
Space expands an agent; only expanded agents are remotely discovered, and
completion may never move focus or replace the active input owner.

The agent tree presents the union of remotely discovered skills and saved state
records, marking each item `none`, `push`, or `pull` for that Host and agent
only. `none` leaves client ownership intact; `push` is uploaded only to the
active target with `d`; `pull` is written only after preview and confirmation.
`x` confirms deletion of a selected remote skill and leaves the managed local
repository and saved state unchanged. `m` confirms a safe managed rename in the Ducklord repository
rename and migrates every persisted Host/agent state record from the old ID to
the new ID without changing a remote skill.

Source, absolute target, local import, preview, result, public HTTPS tracking,
and explicit `INSECURE TLS` are retained subroutes. Tracking update shows a
diff and requires confirmation. `Esc` returns Host Skills to Host settings,
Host settings to Host list, and Host list to the original workspace focus.
`Ctrl-C` from any child cancels work, cleans preview state and artifacts, and
restores that exact focus. The static contract test must fail if either routing
document omits a required branch.

## Capability route additions

The file-exchange E2E uses real container paths on two Ducklions and the
controller. It checks current-directory-only filtering, source-preserving copy
semantics, the persistent per-Project shelf in both transfer directions, and
destination cleanup on cancel or failure. Cancellation/failure checks wait for
actual receiver staging before triggering the interruption and then require
its removal, absent destination, and unchanged source. Directory overwrite is
refused rather than merged; skip and rename
are the available conflict policies. A remote Ducklion must support the
file-exchange commands before the route is usable.

The file-exchange [visual contract](file-exchange-visual-design.md) also requires
rendered source/destination direction, committed per-item results, actual renamed
targets, skipped-item feedback, history surviving modal reopen, and usable
wide/narrow mouse regions. History must retain modal input ownership during
background shell output, return to the same browser side, and ultimately restore
the opening terminal with an executed sentinel. Unit/state tests additionally
cover per-Project history bounds and isolation, endpoint/full-path highlight
isolation, stale progress rejection, and partial/cancel/not-started outcomes.
Validated at `0036366`: focused container E2E and the full default container
suite passed. See [the batch handoff](../HANDOFF.md#latest-checkpoint-exchange-visual-0923)
for test scope, artifact identity and cleanup evidence.

The default container manifest must include these rows before any of these
capabilities is reported complete:

| Route | Required state/dispatcher evidence | Required default container evidence | Required causal assertion |
| --- | --- | --- | --- |
| `route.command-palette` | query ownership, action/project/session selection, exact origin restoration | `TestDucklordCommandPaletteContainerE2E` | after closing, a terminal sentinel reaches the original PTY; palette query never does |
| `route.output-search` | retained-snapshot matching, query ownership, selected-match viewport positioning, terminal restoration | `TestDucklordOutputSearchBookmarksContainerE2E` | Up/Down changes the visible local match selection while output is refreshed; query bytes never reach PTY |
| `route.output-bookmarks` | Session-identity persistence, unavailable-anchor fallback, export redaction | `TestDucklordOutputSearchBookmarksContainerE2E` | Enter reveals the anchor or selects current retained history, then returns to the same PTY; only metadata survives save/export |
| `route.host-resources` | stale Host/request response rejection and `r` refresh ownership | `TestDucklordHostResourcesContainerE2E` | delayed response for Host A cannot change Host B's Resources screen or focus |
| `route.project-transfer` | schema rejection, preview-before-write, collision confirmation, identity skip report | `TestDucklordProjectTransferContainerE2E` | export/import round trip preserves allowed layout/notes/bookmark metadata and excludes PTY output/secrets |
| `route.file-exchange` | `TestProjectFilesInputSelectionAndConflictPreview`, `TestProjectFilesCloseCancelsAndRestoresOrigin`, `TestProjectFilesHistoryIsBoundedAndProjectScoped`, `TestProjectFilesLatestMarksOnlyCopiedAtExactEndpoint`, `TestProjectFilesScrolledMouseRegionsMatchVisibleRowsWideAndStacked` | `TestDucklordFileExchangeContainerE2E` | both host directions and Project shelf copies preserve source/nested bytes; wide/narrow drag targets the intended folder; skip/rename/overwrite and interrupted per-item outcomes match filesystem results; history remains usable during async PTY output and survives reopen; closing restores the original PTY with an executed sentinel |
