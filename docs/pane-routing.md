# Pane routing contract

This graph describes the keyboard routes implemented by Ducklord. `P` means the
configured pane prefix (default `Ctrl+b`); `P o` means two key presses. Prefix
commands are local and are never sent to a remote PTY.

```text
Navigation (Project / Session list / Terminal preview)
|
+-- Project focus -- o ----------------------------> centered Notes modal (project scope)
|                                                     |
|                                                     +-- Esc / Ctrl+C --> exact prior pane/focus
|   Session list focus -- o -----------------------> centered Notes modal (selected session scope)
|   Focused terminal -- P o / P O -----------------> centered Notes modal (session scope)
|                                                     +-- plain o --> PTY input
|                                                     +-- no focused session --> feedback; layout unchanged
|   Focused terminal -- P ? -----------------------> local Help overlay; PTY unchanged
|
+-- P arrows / P n / P p --------------------------> selected pane/tab
|                                                     +-- Enter --> PTY control
|                                                     +-- Esc / Tab --> navigation
|
+-- P t -------------------------------------------> new tab flow
+-- P - / P \\ -------------------------------------> split flow
+-- P , --------------------------------------------> rename tab
+-- P c --------------------------------------------> config target flow
+-- Focused terminal: P d --------------------------> detach route by project
|                                                     +-- Default Project: centered detach-confirm modal
|                                                     +-- other Project: detach directly; select surviving pane
|                                                         and wait for its control lease before first input
|                                                     +-- no surviving pane: return focus to Project navigation
+-- Project focus: j/k, arrows, PgUp/PgDn, Enter --> project or pane action
+-- Session list: j/k, /, l, Enter ----------------> search/detail/PTY
+-- Detail list: j/k, /, l, Enter -----------------> search/close/PTY
+-- Terminal control: P + command; Ctrl+] ---------> navigation/copy
+-- v ------------------------------------------------> copy mode
+-- P ? ------------------------------------------------> local Help; ? opens/closes; Esc/Ctrl-C leave it pinned
+-- forms/modals: Enter apply/next; Esc/Ctrl+C ----> prior navigation
+-- P o from host-scoped view ----------------------> rejected with feedback

### Quick shell prefix contract

From a focused live Session, a single quick-shell suffix is held for 500 ms
while the router waits for a possible repeat. At prefix entry, the router captures the
focused Session pane's exact Project, tab, pane node, and Session identity. The
single-key path then runs the existing creation route; a matching repeat within
that window takes the direct route below. Prefix bytes are local and never reach
the Session PTY.

```text
focused live Session -- P then '-'  --500ms--> existing creation wizard
                     -- P then '\\' --500ms--> existing creation wizard
                     -- P then 't'  --500ms--> existing creation wizard

focused live Session -- P '-' '-'  ------------> Shell, same host/CWD, automatic handle, horizontal
                     -- P '\\' '\\' ------------> Shell, same host/CWD, automatic handle, vertical
                     -- P 't' 't'  ------------> Shell, same host/CWD, automatic handle, new tab

invalid or non-focused origin -------------------> no shell; shortcut bytes stay out of PTY
mismatched suffix within the window --------------> execute first ordinary route, then process second input
```
```

## Executable contract matrix

Each row is an auditable contract: origin, input owner, open/close behavior,
async ownership fence, forbidden side effects, and the exact test IDs and mode.
Every route row requires both state/dispatcher evidence and a default E2E
enabled by `scripts/ducklord-tui-e2e.sh` without a custom pattern.

| ID | Origin / opening input | Input owner after open | Close: exact restore / fallback | Async ownership fence | Forbidden side effects | Evidence (mode) |
| --- | --- | --- | --- | --- | --- | --- |
| `route.quick-list` | startup or Project focus; `b` | quick-list selection | `b`/arrows restore Project/list cursor; removed item falls back to valid visible row | inventory refresh cannot steal Project focus or PTY control | no preview/create, no PTY bytes | `TestPaneRouteContract`, `TestWorkspaceArrowsCrossStackedLists` (state); `TestDucklordPrefixNavigationContainerE2E` (default E2E) |
| `route.detail-list` | navigation focus; `l` | detail list/search | `l`/Esc restore prior navigation location; missing selection clears to valid row | inventory/filter updates retain detail owner | no session start or project mutation | `TestPaneRouteContract`, `TestDetailedModeRestoresKeyboardFocus` (state); `TestDucklordDetailedListContainerE2E` (default E2E) |
| `route.project` | Project navigation focus; Up/`b` | Project pane | Esc/Ctrl+C/Tab restore prior navigation owner; removed project falls back to valid project | project inventory/control completion cannot reclaim a different owner | no session selection or PTY input | `TestPaneRouteContract`; `TestDucklordWorkspaceProjectEnterFocusContainerE2E` (default E2E) |
| `route.tab` | focused workspace; `P n`/`P p` | tab navigation | Esc/Tab restore prior pane; removed tab falls back to surviving tab | tab reorder/completion is fenced to selected tab | no pane create/move | `TestPaneRouteContract`, `TestPanePrefixLetterTabNavigation` (state); `TestDucklordPrefixNavigationContainerE2E` (default E2E) |
| `route.session-pane` | focused workspace; `P` arrows/selection | session-pane selector | arrows/Esc restore prior owner; gone Session falls back to remaining pane | async inventory/control must match selected pane identity | no cross-pane focus or PTY leakage | `TestPaneRouteContract`, `TestPanePrefixPlacement` (state); `TestDucklordPrefixNavigationContainerE2E` (default E2E) |
| `route.terminal` | selected Session; Enter | terminal PTY/control | `Ctrl+]`/Esc restore exact selected pane, else surviving pane fallback | control readiness is leased to the selected Session | no modal key consumption or other-session input | `TestPaneRouteContract`; `TestDucklordPaneControlContractContainerE2E` (default E2E) |
| `route.notes.global` | Notes opener from navigation; scope Global | Notes modal | Esc/Ctrl+C restores exact origin; missing origin falls back to valid navigation | modal owns keys across PTY output/control readiness | no layout/session mutation; no PTY leakage | `TestScopedNotesInputRoutesSearchAndPages`, `TestPaneRouteContract`; `TestDucklordNotesTUIContainerE2E` (default E2E) |
| `route.notes.project` | Project focus `o` | Project-scoped Notes/picker | close restores exact Project focus; removed project falls back to navigation | same Notes ownership fence | no pane create/focus change | `TestDirectNotesRoutesFollowNavigationFocus`, `TestProjectNotesRightArrowOpensItsOnlySession`, `TestProjectNotesRightArrowListsSessionsAddedAfterOpening`, `TestPaneRouteContract`; `TestDucklordNotesTUIContainerE2E` (default E2E) |
| `route.notes.session` | Session list `o`, or terminal `P o`/`P O` | Session-scoped Notes/forms | close restores exact originating Session; disappeared Session falls back to Project/navigation | pending PTY input is queued/dropped by Session identity; control cannot reclaim focus | no create modal, no PTY bytes consumed as Notes keys | `TestCloseNotesModalFallsBackWhenOriginSessionDisappears`, `TestWorkspaceControlCannotReacquireFocusWhileNotesOpen`, `TestDucklordNotesTUIContainerE2E` (state + default E2E) |
| `route.detach` | focused terminal; `P d` | Default Project opens detach-confirm modal; other Projects detach directly | Default modal Esc restores exact terminal; confirmed Default detach or direct non-Default detach selects a surviving pane, waits for its control lease, then releases the first input; no surviving pane falls back to Project focus | detach completion validates source identity and surviving-pane lease before releasing input | no remote inventory mutation; confirmation remains required for Default Project | `TestFocusedTerminalPrefixDOpensDefaultDetachModal`, `TestFocusedDefaultDetachModalOwnsNavigationAndRestoresFocus`, `TestFocusedTerminalPrefixDDetachesNonDefaultProjectPane`, `TestFocusedTerminalPrefixDSelectsSurvivingPaneForHandoff`, `TestFocusedTerminalPrefixDDetachesNonDefaultProjectFromAtomicPrefix` (state); `TestDucklordDefaultDetachContainerE2E`, `TestDucklordPrefixNavigationContainerE2E` (default E2E) |
| `route.context-modal` | workspace focus; `P c`/right click | context/config modal | Esc/Enter restore exact origin; removed target falls back to valid target | target identity is revalidated after async save | no wrong-project/tab mutation | `TestPaneRouteContract`, `TestWorkspaceContextConfigTargets` (state); `TestDucklordPrefixNavigationContainerE2E` (default E2E) |
| `route.help` | terminal `P ?`, navigation `?` | Help overlay | `?` opens/closes; Esc/Ctrl-C leave help pinned with prior owner | help remains owner while async repaint arrives; after close, focus and pending input restore when output is ready | no PTY bytes, create, or focus transfer | `TestPaneRouteContract`, `TestWorkspaceHelpInterceptionAvailability` (state); `TestDucklordPrefixNavigationContainerE2E` (default E2E) |
| `route.create` | Project/session focus; `e`/`c`/`P t` | create wizard/form | Esc cancels to exact origin; Enter advances/applies; removed target falls back | async discovery cannot overwrite wizard origin | no partial layout/session mutation on failure | `TestPaneRouteContract`; `TestDucklordCreateTUIContainerE2E` (default E2E) |
| `route.quick-shell` | focused live Session; `P --`, `P \\\\`, `P tt` | created Session PTY | completion selects created Session; stale source fails closed with no alternate placement | captured Project/tab/node/Session fence; teardown cannot restore Project focus while pending | no shell for invalid origin; prefix bytes never reach PTY; no wizard on repeat | `TestQuickShellPrefixAndPlacement`, `TestQuickShellAsyncTeardownDoesNotRestoreProjectFocus`, `TestQuickShellOriginPinsFocusedSecondTab`, `TestQuickShellPrefixClearsStaleOriginAndBeginRequiresFocus` (state/race); `TestDucklordFocusedPrefixQuickShellHorizontalContainerE2E`, `TestDucklordFocusedPrefixQuickShellVerticalContainerE2E`, `TestDucklordFocusedPrefixQuickShellTabContainerE2E` (default E2E, post-create sentinel) |

`TestDucklordNotesTUIContainerE2E` is part of the default
`scripts/ducklord-tui-e2e.sh` gate, which enables its Notes fixture explicitly.
It exercises direct Project `o` and Session-list `o` routes plus focused-terminal `P O`,
Global/Project/Session scope navigation, j/k selection, descendant search with
origin labels, and the built-in `a`/`e` title and content forms. Only `E`
opens the full notebook through `$EDITOR`.
The container route checks are split across the named existing tests:
`TestDucklordWorkspaceProjectEnterFocusContainerE2E` (Project focus),
`TestDucklordPrefixNavigationContainerE2E` (tab next/previous),
`TestDucklordPaneControlContractContainerE2E` (session terminal focus and
`Ctrl+]`), `TestDucklordNotesTUIContainerE2E` (Notes), and
`TestDucklordCreateTUIContainerE2E` (create flow). Run these with
`DUCKLORD_TUI_CONTAINER_E2E=1` where their individual gates require it.
Every route in this matrix has a default-container E2E row in the executable
manifest, including detail, context-modal, and Help. State and dispatcher tests
remain the required fast checks for every route change, while the
default-container E2E is required completion evidence. Help opens and closes
only with `?`; Esc and Ctrl-C leave the overlay pinned while preserving its
ownership.

## Developer requirement

Every new UI/UX route must add a stable `route.*` entry to this matrix, a
table-driven state-router contract test using the real handler, and a default
manifest container TUI E2E. If an isolated fixture cannot support the route,
the route must fail closed in the manifest and matrix with the concrete reason;
an opt-in or targeted test cannot satisfy the default completion gate.

### Ducklord Host Skills

```text
workspace focus -- h --> Host list -- Enter --> Host settings -- Skills --> Host Skills · host
                                                                         |
                           +---------------------------------------------+----------------------------------------------+
                           | Ducklord managed repository                 | client host skill tree                        |
                           | local skill list                             | agent target (Enter/Space = expand/collapse) |
                           | `p` push / `n` none / `m` rename / import    | remote + configured state union               |
                           |                                              | `r` pull / `x` delete remote / `d` deploy     |
                           +----------------------------------------------+----------------------------------------------+
                                      Tab / Left / Right switches the focused pane

Esc: Host Skills -> Host settings -> Host list -> restore workspace focus
Ctrl-C: any Host Skills child -> cancel work, clean preview artifacts, restore workspace focus
```

The Host Skills route begins with `h` from the workspace. `h` opens the Host
list; Enter chooses the Host whose settings will be changed. Selecting `Skills`
opens that Host's dual-pane manager directly. Its left pane is Ducklord's
separate managed repository. Its right pane is an expandable tree of the
selected Host's agent installation targets, each with an ID and absolute
directory. The first available target is expanded and active; `Tab`, Left, and
Right change pane ownership, while Enter or Space expands or collapses an agent
header. Remote discovery starts only for expanded targets and its completion
never steals focus or keyboard input.

Each displayed skill state belongs to exactly one Host and agent target:
`none` means Ducklord makes no change and the client manages it; `push` means
`d` uploads Ducklord's managed-repository copy to this target; `pull` means the
skill was downloaded from this target into Ducklord's separate managed
repository after preview and confirmation. The right pane shows the union of
the target's discovered remote skills and its saved state entries, so a deleted
remote item remains visibly configured until its state is changed. No state is
inherited by a different target. Existing legacy host-wide selections are
`push` only until that target receives an explicit state.

With the repository pane focused, `p` marks the selected local skill `push`
for the active right-pane agent, `n` records `none`, and `m` opens a confirmed
rename form. Rename validates a safe unique skill ID, moves the local managed
repository entry, and migrates all saved Host/agent state records from the old
ID to the new ID; it does not alter any remote skill. With a remote skill leaf
focused, `r` previews a pull and `R` accepts a remote skill ID; both require
confirmation before the managed repository changes. `x` opens a confirmation
that names the Host, agent target, path, and remote skill; confirmation deletes
only that remote skill over SSH, refreshes the target tree, and does not change
the local repository or saved state. `d` deploys the active target's `push`
skills.

Source, local import, target editing, and HTTPS tracking are independent
subroutes from the selected target. Tracking is bound only when a source is
explicitly saved. A tracking source accepts public HTTPS only; the source form
makes the optional `INSECURE TLS` certificate-verification bypass explicit and
the repository pane marks sources using it. Tracking and update show an HTTPS
preview/diff, and confirmation is required before applying an update. `Esc`
moves exactly one parent route at a time; `Ctrl-C` closes the complete Host
Skills route, cancels an operation, and cleans preview artifacts before the
central router restores the original workspace focus.

| `route.host-skills` | workspace; `h` -> Host list -> Enter -> Host settings -> `Skills` -> dual-pane Host Skills | repository pane, agent tree, source/target/import, remote-delete, pull-preview, rename-confirm/result | Esc returns one parent at a time; Ctrl-C restores exact workspace origin and preview cancellation cleans temporary state | per-target `none`/`push`/`pull`, public HTTPS source, explicit `INSECURE TLS` opt-in, target identity, selected-target SSH deploy, remote-delete identity, pull preview, and local rename migration are revalidated before apply | no PTY input, host focus change, remote write, remote delete, or local rename before required confirmation | `TestHostSkillsDualPaneRouteOwnership`, `TestHostSkillsManagementIsScopedToAgent`, `TestHostSkillsRemoteListEventKeepsSkillsInputOwner`, `TestHostSkillsCtrlCCleansAndClosesWholeRoute`, `TestHostSkillsRouteDocumentationContract`; `TestDucklordHostSkillsContainerE2E` (required default container E2E) |

## Workspace capability routes

```text
workspace or focused terminal -- P Space --> Command palette
                                           | action result: execute the named local action
                                           | Project result: select Project in navigation
                                           | Session result: select Session preview in navigation
                                           ` Esc / Ctrl-C: exact captured origin

focused terminal -- P / --> Output search modal -- Enter --> next retained-screen match
                                                   ` Esc / Ctrl-C: exact terminal control
focused terminal -- P m --> Bookmark label form --> saved bookmark (session identity + line metadata)
focused terminal -- P M --> Bookmark picker ----- > selected retained-screen match / unavailable message

workspace -- h --> Host list -- Enter --> Host settings -- Resources --> Host resources
                                                                  ` r refresh, Esc back one parent

Project navigation -- P c --> Project config --> Export --> destination form --> atomic .duckproj.json
                                             `-> Import --> source form --> validated preview --> confirm --> imported Project
```

`route.command-palette` owns all query input after `P Space`. It searches local
actions, Projects, and visible Sessions without sending any byte to a PTY.
Actions execute their existing local route; Project and Session results close to
navigation with that target selected. Esc or Ctrl-C restores the captured focus
exactly, falling back to valid navigation if the origin disappeared.

`route.output-search` and `route.output-bookmarks` start only from focused
terminal control (`P /`, `P m`, and `P M`). Query and form keys are local.
Searches use Ducklord's bounded retained terminal snapshot. Up/Down moves the
selected match into the terminal viewport. A bookmark stores a Session identity,
user label, and metadata-only line anchor/fingerprint; it never persists PTY
output. Appended output leaves a retained anchor usable. Only an anchor that has
rolled out of retained scrollback is shown as unavailable; Enter then selects the
deterministic current-history fallback without changing focus or creating a
Session.

`route.file-exchange` opens Project files from Project/quick-session-list focus with
`f`, from a focused terminal with the pane prefix followed by `f`, or from the
Command palette's **Project files** action. The modal owns both columns while
host and Project-shelf listings load.
Opening selects the left local-working-directory column; the right starts at
the active session's host/cwd when available, otherwise the Project shelf.
The browser's stable states are `browse`,
`endpoints`, `path`, `filter`, `preview`, `busy`, and `history`; `Tab` changes columns,
`h` chooses local/host/Project shelf endpoints, `g` edits a path, `/` filters
the current directory, Space marks entries, Enter opens or confirms, and `c`
opens the copy preview. `i` switches folder icons to ASCII. `l` opens the
captured Project's transfer history: Left/Right selects a batch, Up/Down selects
an item, and `x` clears that Project's history/highlights. History Esc returns
to the same browser side and cursor; history keys never reach the PTY.
Backspace opens the parent directory and clears its
filter. Endpoint selection and confirmed path changes clear the old directory
filter. Esc unwinds one form state or closes the browser; it is ignored during
copy. Ctrl-C requests cancellation and holds the busy state until cleanup
acknowledges completion, or closes an idle modal. Close restores the captured Project/session/terminal
origin even when shell output arrives while a listing or copy is pending.
Copies preserve the source and owner execute permission. Each transferred item
is limited to 1 GiB; remote archives are bounded to 10,000 records (including
the completion record) and 64 path components. Host
copies stream through the controller, never copy host-to-host directly, and temporary destination state is removed on
cancel or failure. A Project shelf persists per Project. Existing destination
directories are refused for overwrite rather than merged; skip and rename
remain available. Remote Ducklions must support the file-exchange commands
before Host exchange is available; older Ducklions show the command failure.

```text
Project / quick Session list -- f ------+
Focused terminal ----------- prefix+f -+--> Project files (modal owns focus)
Command palette ------------ select --+      |
                                            +-- Tab: left <-> right
                                            +-- h: Local / Host / Project shelf
                                            +-- g: path; /: filter; Enter: directory
                                            +-- Space: select entries; i: icons
                                            +-- l: History -- Esc --> same browser
                                            |      Left/Right: batch; Up/Down: item
                                            |      x: clear history/highlights
                                            |
                              c / drag -----+--> Copy preview
                                                  | source + destination + policy
                                                  +-- Esc --> browser
                                                  +-- Enter --> Copying
                                                                | Ctrl-C: cancel
                                                                | wait for cleanup
                                                                v
                                                             browser
Idle browser -- Esc / Ctrl-C --> exact opening pane / terminal
```

The [visual contract](file-exchange-visual-design.md) defines independent host
panels, narrow-screen stacking, shared render/mouse geometry, committed-only
highlights keyed by endpoint/path, ordered per-item progress, and the last 20
batches per Project in memory. Delayed listing/progress events cannot change
input ownership or apply to a different generation. Partial failures retain
completed items and distinguish the attempted item from items not started.

The modal must render in the default workspace, an empty layout, and detailed
Session mode. A palette item containing its title is not evidence that the modal
opened: PTY checks must observe browser controls and verify copied bytes.

`route.host-resources` is Host list -> Host settings -> Resources. It displays
portable Ducklion process/resource data (host OS/architecture, CPU count,
Ducklion memory, uptime, managed Sessions, and active PTYs). `r` issues a
refresh. A response applies only while its captured Host identity, resources
screen, and request ID are current; refresh never moves selection or focus.

`route.project-transfer` is Project config -> Export or Import. Export writes a
versioned JSON document atomically with owner-only permissions. It contains the
selected Project layout, scoped Notes, Session descriptors, and output-bookmark
metadata, but excludes credentials, host secrets, SSH settings, and PTY output.
Import parses and validates the complete document before preview. It reports
Session identities absent locally as skipped and performs no remote connection
or Session creation. A name collision is never overwritten before explicit
confirmation; the confirmation names the target Project and every skipped
Session.

| ID | Origin / opening input | Input owner after open | Close: exact restore / fallback | Async ownership fence | Forbidden side effects | Evidence (mode) |
| --- | --- | --- | --- | --- | --- | --- |
| `route.command-palette` | workspace or focused terminal; `P Space` | palette query/results | Esc/Ctrl-C exact origin, missing origin -> navigation | palette results use captured workspace generation; action result validates target identity | no PTY bytes; no action before Enter | command-palette state/router tests; `TestDucklordCommandPaletteContainerE2E` (default E2E) |
| `route.output-search` | focused terminal; `P /` | search query/results | Up/Down selects the retained match and positions the terminal display; Esc/Ctrl-C restores exact terminal control | snapshot/session identity remains captured; repaint cannot take ownership | no PTY query bytes; no Session/layout mutation | output-search state/router tests; `TestDucklordOutputSearchBookmarksContainerE2E` (default E2E) |
| `route.output-bookmarks` | focused terminal; `P m` / `P M` | label form or bookmark picker | Enter reveals the retained anchor or shows the deterministic current-history fallback; Esc/Ctrl-C restores exact terminal control | bookmark write/read is fenced by Session identity | no raw terminal output in durable state/export; no PTY input leak | bookmark persistence tests; `TestDucklordOutputSearchBookmarksContainerE2E` (default E2E) |
| `route.host-resources` | `h`, Enter Host, Resources | resource screen | Esc Host settings -> Host list -> origin | Host ID + request ID + screen mode must match | no focus change, PTY input, or Host mutation on refresh | host-resource protocol/router tests; `TestDucklordHostResourcesContainerE2E` (default E2E) |
| `route.project-transfer` | Project navigation; `P c`, Export/Import | transfer form/preview/confirmation | Esc one parent; Ctrl-C exact origin | import document hash/name and selected Project are revalidated at confirmation | no write before confirmation; no credential/output export; no remote mutation | project-transfer unit/router tests; `TestDucklordProjectTransferContainerE2E` (default E2E) |
| `route.file-exchange` | Project/session-list `f`, focused terminal `P f`, or Command palette **Project files** | two-column file browser; `browse`/`endpoints`/`path`/`filter`/`preview`/`busy`/`history` | Esc backs out/closes while idle; history Esc restores browser side/cursor; busy Ctrl-C cancels and waits for cleanup; idle close restores exact origin | listing/copy generation and endpoint identity must match the open modal; pending shell output cannot reclaim focus | no PTY bytes, source deletion, direct host-to-host connection, or destination write before preview confirmation | `TestProjectFilesInputSelectionAndConflictPreview`, `TestProjectFilesCloseCancelsAndRestoresOrigin` (state); `TestDucklordFileExchangeContainerE2E` (default E2E) |
