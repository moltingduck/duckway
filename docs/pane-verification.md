# Project and pane verification

Updated: 2026-09-15. Scope: the **Agreed Decisions** section of
[the specification](ducklord-ducklion-spec.md). This is an evidence index, not
an index of implementation, regression tests, and completed requirement audits.

## Verified behavior groups

| Requirements | Implementation and regression evidence | Integration evidence |
| --- | --- | --- |
| Local Projects, cross-Host shared identities, one pane per Session per Project, derived Default membership, persisted layout | `internal/ducklord/project_layout.go`, `project_layout_test.go`: membership/detach, Default suppression, persistence, atomic moves | MixedHostProject, DefaultDetach, ProjectDelete |
| New shell from Host home or bookmark/typed path; recursive mkdir confirmation; new tab/split; existing Session and drag/move confirmation | `cmd/ducklord/main_test.go`, `workspace_pane_modal_test.go`, `workspace_mouse_test.go`: home/path, asynchronous creation fencing, stale identity rejection, empty-area drop, cancellation | WorkspacePreview, WorkspaceDefaultNewShellSplit, WorkspaceTwoLivePanes |
| One-way quick navigation, per-Project tabs, selected/focused pane distinction, shared control without implicit yield | `workspace_state_test.go`, `workspace_preview_test.go`: navigation history, Project selection, tabs, visible leaf preflight | WorkspaceProjectEnterFocus, PaneControlContract |
| Four quick sorts, persisted event timestamps/direction, foreground runtime labels, suppressed events do not promote, selected identity survives reorder | `quick_sort_test.go`, `notification_reorder_ingestion_test.go`, `internal/ducklord/activity_state_test.go` | ForegroundAgentLabels, SharedProjectNotification |
| Two-column detailed mode, Unicode fuzzy metadata search, separate state filters, configurable navigation, preview without acknowledgement, focus and workspace restoration | `workspace_detail_test.go`, `internal/ducklord/detail_list_test.go`, `workspace_state_test.go` | DetailedList, DetailedUnreadFilter (one and two unread), DetailedOffline |
| Session-shared binary unread and Project badges; global/Host/Session policy; explicit Project focus threshold; no focus replay | `internal/ducklord/notification_policy_test.go`, `activity_state_test.go`, `workspace_state_test.go`, `cmd/ducklord/notification_reorder_ingestion_test.go` | SharedProjectNotification, LocalNotification |
| Local configurable audio, independent desktop/audio failures, metadata-only notifications, per-Session attention coalescing | `notification_delivery_test.go`, `notification_backend_test.go`, `notification_audio_integration_test.go`, `notification_sound_backend_test.go` | LocalNotification, SharedProjectNotification |
| Explicit hook activation, installation epochs, historical callbacks cannot activate a reinstall, unchanged install preserves activation | `internal/ducklion/store/hook_installation_test.go`, daemon hook/Host-config tests, TUI status tests | HostHookConfig (callback, remove, reinstall pending) |
| Shell exit/end retirement, failed replacement retirement, immutable failure receipts, retained logs and configurable retention | `internal/ducklion/store/retained_shell_test.go`, daemon lifecycle/retention tests: source/replacement generations, stale fences, definitive preparation failure | ShellRetirement, ExplicitShellEndRetainsLog, ShellEndModal, HostRetention |

Names in the integration column abbreviate
`TestDucklord<Name>ContainerE2E` in `cmd/ducklord`.

## Executed gates

- Full Podman Go suite: passed; lint: zero issues.
- After the final lifecycle fix: full Podman Go suite passed again; focused
  lifecycle race tests passed (store 1.439s, daemon 7.102s). Four affected
  shell/pane container E2E tests passed (27.807s). Demo rebuilt with the fix.
- Twenty pane container E2E tests: passed, 163.848 seconds. This includes the
  groups above plus shell retirement, explicit shell-end retained logs, shell
  end confirmation, shell-first hook, and Host retention tests.
- Isolated Claude live split E2E with updated injected credentials: passed,
  31.002 seconds. Codex on the current demo: passed, 41.061 seconds. Both cover
  three completions, background unread delivery, and return-to-focus.
- Hook configuration and live-agent tests run sequentially: changing Host
  hooks concurrently would invalidate the live tests' callback isolation.
- Logs and credential contents are intentionally excluded from this document.

## Completed requirement audits

- Creation/layout/navigation, notifications/focus, detailed mode, and
  lifecycle/retained-log assertions were audited; identified gaps are fixed.
- Failed replacement-shell startup and definitive preparation failure now
  remove shell inventory atomically with retained logs and a failed receipt.
  Managed-agent recovery behavior is unchanged. Focused review found no
  high/medium issues.
- Generic read-only wording now explicitly distinguishes managed agents from
  the user's tmux-like shell multiwriter exception. PaneControlContract covers
  both shared-shell and foreign-managed-owner behavior.
- This index groups related requirements; audits inspected individual
  assertions rather than treating test names alone as evidence.

For current work and outstanding processes, see
[the implementation checkpoint](pane-implementation-status.md).

## Stacked panes and mouse-first follow-up (2026-09-15)

- Project pane now sits above Session list pane; theme validation, persistence,
  focused headings, separators, and detailed-list geometry have unit coverage.
- Mouse tests cover pane focus requests, Project/list navigation, modal choices,
  disabled/clipped controls, host toggles, confirmations, and pinned help actions.
- Full Podman Go suite and Ducklord race tests passed. Podman container E2E
  passed for TwoLivePanes (drag/move/create) and ProjectEnterFocus (mouse focus
  with input reaching the intended shell, not the independent list selection).
- Screen hit-testing in E2E strips generated ANSI styling and accounts for cell
  width; drag tests explicitly select focus instead of assuming old behavior.
- Provider-consuming agent tests were not repeated for these UI-only changes.
