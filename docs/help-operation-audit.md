# Help and operation hints

## Contract

`?` opens and closes Help in navigation. A focused terminal uses its configured
pane prefix followed by `?`. The configured Help shortcut closes the overlay.
Esc and Ctrl-C leave Help pinned; in its search field they clear the query and
leave the overlay open. `/` starts search and Enter pins the results. Up/Down
scrolls while the search field owns input.

Help describes operations in their actual context. A shortcut available in the
underlying pane is not automatically a clickable Help action: the Help input
owner must also dispatch that action. Informational examples and multi-step
instructions must never send their displayed text to a terminal. Configurable
shortcuts must come from the active configuration.

Closing Help restores its originating pane after that pane can accept input.
Asynchronous output must not steal Help focus. See [route.help](pane-routing.md)
and the [verification contract](ui-routing-verification.md).
Help owns mouse reports even after its underlying PTY writer becomes ready.
Blank/outside clicks, wheel events and releases must not tear down that writer;
actionable clicks dispatch locally before ordinary pane mouse handling.

## Coverage checklist

When changing a handler, review its Help entry and every visible instruction
that leads to it, including success/error messages and child-dialog footers.
The handler is the authority for accepted keys, selection requirements and
cancel/back behavior. Do not add a claimed key because another dialog accepts it.

| Surface | Operations to document | Dispatcher / renderer |
| --- | --- | --- |
| Session lists | Selection, attach, search, organization/groups, sorting/reordering, state-dependent lifecycle menu, handle rename | `main.go` |
| Project navigation | Create/delete, empty Project Enter, add/move/detach panes, notification focus, SSH hosts, tab/pane navigation | `main.go`, `workspace_pane_modal.go` |
| Terminal | Focus/unfocus, copy mode, prefix commands, tab/pane movement, rename/config, terminal search and bookmarks | `pane_prefix.go`, `terminal_tools.go` |
| Quick shell | Single suffix opens wizard; repeated `--`, `\\`, or `tt` within 500 ms uses the focused live Session host/CWD and requested placement | `pane_prefix.go` |
| Detailed list | Search, state filter, preview, focus, jump to Project | `main.go`, `workspace_detail*.go` |
| Notes | Project/list direct `o`; terminal prefix; scope, descendant search, entry selection and scrolling, copy content, simple/full-book editing and return | `workspace_pane_modal.go`, `notes*.go` |
| Project files | Both endpoints, directories/filter, marking, copy preview, conflict policy, cancellation, history/highlights and icon fallback | `project_files.go` |
| Project configuration | Target-specific config, rename, appearance, shortcuts, import/export path/preview/confirmation | `workspace_area_config.go`, `project_transfer_ui.go` |
| Host management | Host list/settings, connections/reconnect, retention/hooks, resources/retry/refresh, add/remove Host | `main.go` |
| Host Skills | Repository and agent tree, selection/expansion, none/push/pull, deploy, import, tracking sources, diff confirmation, target, local rename and remote delete | `host_skills_modal.go` |
| Command palette | Prefix+Space, filtering, selection, action dispatch and close | `command_palette.go` |
| Settings/forms | Configured shortcut editing, notification choices/staging/save, session wizard and directory bookmarks, group and lifecycle dialogs | `main.go`, `notification_config_modal.go` |
| Mouse | Pane focus/config targets, Project/tab reorder and copy selection; only supported Help actions are clickable | `main.go`, `modal_mouse.go` |

Notes uses Left/Right and `h`/`l` to change book scope; Up/Down and
`j`/`k` select entries and scroll the visible list. Global and Project search includes descendant books.
Enter copies an entry's **content**, without its title. Simple editing and
external full-book editing have separate instructions.

Terminal search operates on retained output. Bookmarks store anchors rather
than durable terminal transcripts; unavailable anchors use the existing
current-history fallback. Help must not promise persistent terminal output.

## Verification

Use regression tests for catalog coverage, configured bindings, context
highlighting, actionable mouse rows and each corrected misleading hint. Exercise the full
render sequence: inactive modal renderers must preserve the visible modal’s
mouse targets.
`TestDucklordPrefixNavigationContainerE2E` exercises Help through a real terminal:
open from focused Session, search for a newly covered operation, pin/clear the
query, click a supported action, confirm the current screen has closed Help,
and execute a sentinel in the exact originating shell. Native
session logs must show no input leakage while Help owns input.

The complete default routing suite remains the acceptance gate. A targeted Help
test or a render snapshot alone does not establish all pane routing or demo
readiness. Record actual commands and outcomes in `HANDOFF.md`.
