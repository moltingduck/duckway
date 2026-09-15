# Ducklord / Ducklion Remote Agent Control MVP

## Ducklord Project and Pane Redesign — Agreed Decisions

This section is the implementation target for the Project and pane redesign.
The operator-facing behavior already available is described in the
[Ducklord quick guide](ducklord-user-guide.md); any remaining gaps against
this target still require implementation and verification.

### Terminology: Projects, Bookmarks, and Panes

- A **Project** organizes local Ducklord Session panes across Hosts; it is not
  a remote working directory.
- A **bookmark** is a named directory saved on a Host by Ducklion. Creation
  offers saved bookmarks or a typed directory, with directory suggestions and
  explicit confirmation before recursive directory creation. The operator
  chooses **Add path to bookmarks** or **Use path once**. The default working
  directory for a new shell-first Session is that Host user's home directory.
- `ducklion bookmarks` manages Host directory bookmarks; `ducklord bookmarks
  <host>` lists them through SSH. The old `projects` command remains an alias.
  Existing `cc-projects.json` storage and wire identifiers remain unchanged;
  this terminology change does not discard saved paths or require migration.
- The **Project pane** and **Session list pane** are navigation regions. The
  **Terminal area** holds the current Project's **Terminal tabs**. Each tab
  contains one or more **Session panes**, arranged in horizontal/vertical
  splits. A Session pane references a Ducklion Session, not a separate process.

### Project Membership

- A Project is a Ducklord-local entity with a stable ID. Its display name is
  not an identifier. It may reference sessions from any number of hosts.
- Project-to-session membership is many-to-many and is driven by Session pane
  placement, not a separate manual membership list. A session is stored once;
  each Project's Session pane holds a reference rather than a copy. A session
  may appear in several Projects, each with its own tab and split placement.
- A session's control owner, runtime state, lifecycle, and notification seen
  state are shared across every Project that references it. Viewing a
  notification through one Project marks the same session notification seen
  in all other Projects. Destroying a session removes its references from all
  Projects; removing a Project reference does not destroy the session.
- Ducklord provides one built-in Default Project for sessions with no explicit
  Project membership. Its membership is derived: adding a session to any
  user-created Project removes it from Default, and removing its last explicit
  membership returns it to Default. Default does not duplicate a session that
  is already classified elsewhere.
- Project progress accounting is deferred. A session referenced by multiple
  Projects must not be silently counted as independent work in each Project;
  its contribution will be specified before progress management is added.
- Deleting a user-created Project removes only its local panes; Ducklion
  Sessions continue running, and Sessions without another explicit Project
  return to Default. Default Project cannot be deleted. The centered delete
  confirmation defaults to Cancel, and deleting a notification-focused
  Project turns that focus off.

### Project Notification Badge

- The first version uses a binary unread indicator. A Project shows the
  indicator whenever at least one member session has an unread notification.
- Notification seen state belongs to the shared session, not the Project
  reference. Viewing the session from any Project updates every Project badge
  that includes it.
- The badge does not show a numeric count in the first version. A future count
  would need an explicit definition of whether it counts sessions or events.

### Session List Navigation and Ordering

- The Session list pane is one cross-Project quick-navigation list. It has no
  group hierarchy; group organization belongs to the Project pane. It offers
  user-switchable Host, Session type, event time, and event importance sorting,
  but no custom ordering. A separate detailed Session list pane will support
  searching Sessions and inspecting their state. The quick list's selected
  sort mode is saved in Ducklord's local
  config and restored after restart. The event-time mode uses each Session's
  most recent notification-event timestamp, not its creation time, and supports
  a newest-first/oldest-first toggle; its direction is saved alongside the sort
  mode. Sessions with no notification-event history come after those with
  history in either direction and maintain a stable relative order.
- Session-type sorting uses the currently detected foreground runtime rather
  than the shell root process: Codex, Claude, other agent, then Shell. If
  detection is uncertain, classify it as Shell without claiming agent status.
- The quick list defaults to event-time sorting with newest notifications
  first; Sessions without notification history follow those with history.
- Event time is Ducklion's persisted `session_activity.updated_at_ms`, carried
  in the authoritative Session snapshot. Ducklord must not substitute the
  time it first observes a snapshot, which could make old events look new.
- Qualifying unread events may temporarily promote Sessions across the whole
  list without changing Project order or the saved base order. Promotion is
  the first sort partition: qualifying Sessions come before other Sessions,
  and each partition uses the currently selected sort mode. Event
  importance is determined by each Session's most recent event class:
  user action needed, task failed, task completed, then generic terminal
  attention. Sessions with equal importance sort by newest event first.

### Detailed Session List Mode

- Detailed Session list mode replaces the normal three-region Project pane,
  quick Session list pane, and Terminal area layout with two columns. The left
  column is a searchable detailed Session list and status view; the right
  column displays only the selected Session's pane, not its entire Project
  layout.
- Moving up or down in the detailed list immediately switches the Session
  displayed on the right. Enter moves keyboard focus into that right-hand
  Session pane; it does not navigate to a Project. The right-hand pane obeys
  the same Ducklion writer ownership as any other view: non-owners are
  read-only, and entering this mode never implicitly calls `yield`.
- The existing shell exception still applies: shell Sessions are tmux-like,
  terminal-only shared-writer Sessions. Another Ducklord may explicitly focus
  and write to a shell without `yield`; mere preview never sends input or
  resize. The non-owner read-only restriction above applies to managed agent
  Sessions, not to this shell exception. Detecting an agent launched inside a
  shell does not convert its ownership policy to managed-agent mode.
- Switching detailed-list previews never requests a remote PTY resize. Only
  after Enter focuses the right-hand Session pane may that focused pane drive
  the remote PTY dimensions, subject to writer ownership.
- Moving through the detailed list only previews Sessions and does not clear
  unread marks. Enter clears the selected Session's unread state only after
  its right-hand pane actually receives keyboard focus.
- While that pane has keyboard focus, retain its live inventory row even if
  clearing unread makes it fail the current filter. Leaving pane focus
  reapplies the filter and reconciles selection. This never retains a removed
  or stopped Session or bypasses runtime/ownership checks.
- Sessions on disconnected Hosts remain searchable in the detailed list, with
  gray styling and an explicit disconnected state. The right-hand pane shows
  the last locally available view or a connection/error placeholder, not a
  false impression of live output.
- Each detailed-list row shows the Session name, Host, Project membership,
  Session type, current writer, connection/runtime state, last notification
  time, and unread indicator. The design leaves room for a later per-Session
  detail view with more information; that view is not defined in this version.
- Detailed-list search performs live fuzzy matching on Session name, Host
  name, and Project name. State filters are separate controls, not magic
  tokens embedded in the search text. The first version offers All, Unread,
  Needs action, and Disconnected filters.
- Search operates only on Ducklord's already-known Session metadata; typing
  does not query remote Hosts. Matching is Unicode-aware and case-insensitive,
  and one Session appears once even if several of its Project names match.
  Apply the chosen state filter first, then rank text matches by match quality;
  ties retain the detailed list's current sort order and stable Session ID.
- `/` focuses the search field. Results update as text changes without changing
  remote state or clearing unread. Keep the selected Session if it remains in
  the results; otherwise select the first result and update the right-hand
  preview. An empty result shows a clear no-matches state rather than the
  previous Session preview. Escape clears a nonempty query first, then returns
  keyboard focus to the detailed list. Search text and filter choice are
  temporary to this detailed-list visit and reset on exit.
- The rebindable `g` shortcut navigates from the selected Session to an
  appropriate Project's normal Terminal area, restores the three-region
  layout, and focuses that Session pane. Project choice follows the shared
  Session-to-Project navigation rules.
- Initial detailed-mode bindings are `D` to enter the mode from the normal
  layout, `/` to search, arrow keys or `j`/`k` to move through results,
  `Enter` to focus the preview pane, `Ctrl-]` to return focus to the detailed
  list, and `g` to jump to the selected Session in its Project layout.
  Ducklord's existing configurable shortcut mechanism covers these actions;
  defaults are provisional and may be revised after use.
- Leaving detailed mode without using `g` restores the Project, Terminal tab,
  Session pane, and keyboard focus that were active before entering it.
  Merely previewing other Sessions does not change that saved workspace state.

### Terminal Layout Terminology

- **Terminal area** means the entire right-side terminal region. It contains
  the selected Project's tabs and split layout; it is not itself a pane.
- **Terminal tab** means one page within a Project's Terminal area. A tab may
  contain horizontally and vertically split panes.
- **Session pane** means one split cell displaying one Ducklion session. It is
  a view, not the remote PTY process itself. The same session may have a
  Session pane in multiple Projects, but at most one Session pane in any one
  Project, across all its tabs.
- **Project pane** and **Session list pane** name the two left-side UI regions.
  Older sections of this document that say “right PTY pane” refer to the
  Terminal area; they do not mean a single Session pane.

### Project Terminal Layout

- The Terminal area is user-arrangeable. Each Project owns its own Terminal
  tabs, pane placements, and split geometry.
- Session identity, runtime, output source, notification seen state, and
  control owner remain shared across Projects.
- Multiple Project views within the same Ducklord refer to the same Ducklion
  session and Ducklord owner; switching among them never invokes `yield`.
  Only the currently focused Session pane sends input and requests a remote PTY
  resize. Other views display the shared output without driving PTY size.
- Creating a Session pane is how a session is added to a Project. Within the
  selected Project, the user first chooses a horizontal split, vertical
  split, or new Terminal tab, then chooses **New session** or **Add existing
  session**.
- **New session** uses the normal host, runtime, and directory selection flow.
  When the user has not chosen a directory, the default is the selected host's
  home directory, not Ducklion's working directory.
- **Add existing session** opens a centered picker of existing Ducklion
  sessions. It adds a view of the chosen session without starting another
  process.
- A session can also be dragged from the Session list pane onto a Session pane
  in the Terminal area. A centered choice then asks whether to split
  horizontally, split vertically, or open a new Terminal tab. If the dragged
  session already has a Session pane in the selected Project, Ducklord asks
  whether to move that existing pane to the new position; it never silently
  duplicates the session within the Project.
- Dropping onto empty Terminal area offers a new tab only. Dragging does not
  change the quick-list selection, focus a PTY, or request writer ownership.

### Detach and Destroy

- Closing or detaching a Session pane removes only that local view. It never
  stops or destroys the Ducklion session, including when the pane is in the
  Default Project. Other viewers and the remote process are unaffected.
- Closing a Default pane persists the closed view while retaining the session's
  implicit Default membership. Inventory refresh, startup, and Detailed Sessions
  preview do not reopen it. Explicit quick-list navigation, a Detailed Sessions
  jump to Project, or Add Existing restores its pane. Destroy or authoritative
  session removal clears the closed-view record too.
- If the last Session pane reference in user-created Projects is detached,
  the still-live session returns to the built-in Default Project.
- **Destroy** is a separate, explicit action on the underlying Ducklion
  session, not an effect of removing a pane or a Project reference. It ends
  the remote session and invalidates every viewer's Session pane. A read-only
  viewer cannot destroy it.

### Shell-First Runtime and Retained Logs

- Every newly created Session pane starts with an interactive shell as the
  Ducklion session's long-lived root process. Agents such as Codex and Claude
  are launched inside that shell, so the user can supply arbitrary agent CLI
  options. An agent process exiting returns to the shell; it does not remove
  the Ducklion session or its Session pane.
- If the root shell itself exits, Ducklion removes the live/stopped session
  from its session inventory and Ducklord removes its Session pane references.
  A stopped session is not retained as a selectable session.
- PTY output logs are retained separately by Ducklion after the session is
  removed. Ducklord can retrieve retained logs when needed for diagnosis.
  The default retention is one week. Ducklion rotates and deletes logs older
  than the configured retention period.
- Log-retention configuration is adjustable from Ducklord and takes effect in
  Ducklion by hot reload without restarting the shell sessions. This is an
  explicit exception to the earlier restart-required configuration policy.
- Shell-first launching changes notification authority: the shell remaining
  alive proves neither that an agent turn is active nor that it completed.
  Foreground-process/screen detection improves visibility only: it may label
  a likely agent and tentative working/idle/blocked state, but it does not
  claim an exact task completion or authorize control transfer.
- Exact completion/failure notifications and their sounds require explicit
  agent hook events. Without a hook, allowlisted BEL/OSC may produce only a
  generic terminal-attention notification, not a claimed agent completion.
  Shell-first Codex/Claude hooks are installed explicitly into the agent's
  host-side configuration; the current direct-exec CLI-flag injection is not
  assumed to work unchanged.
  The shell-first hook endpoint carries no bearer token in the shell
  environment. Ducklion accepts a payload-free advisory completion/failure
  only from a process in that Session's PTY descendant tree. Same-user code
  inside the Session can still invoke the hook helper, so a hook report is a
  notification hint, never authorization for yield or managed task state.

### Host Configuration Ownership

- Ducklord is the operator-facing TUI for host settings. It presents the
  intended changes and obtains explicit user approval for agent integration
  installation or removal before sending a structured request to Ducklion.
  Installing an integration may modify the selected host's Codex/Claude
  configuration, and the confirmation must make that effect clear.
- Ducklion alone validates and applies host-side configuration changes,
  including agent hooks and log rotation/retention. Ducklord does not edit
  remote configuration files over SSH or copy a completed config file into
  place; it sends a configuration object/operation for Ducklion to apply.
- Log-retention changes take effect through Ducklion hot reload. The exact
  merge and removal policy for agent hooks is structured append: Ducklion
  parses the existing agent configuration and adds only its own identifiable
  hook entries to the appropriate event arrays. It never appends raw text to
  a JSON file or replaces existing user hook arrays.
- Installation is idempotent: repeating it does not create duplicate Ducklion
  hooks. Removal deletes only Ducklion-owned entries and preserves user and
  third-party hooks. Before changing a file, Ducklion retains a recoverable
  backup and writes the merged configuration atomically.
- Codex may require its own hook trust review after installation. Ducklord's
  approval to modify host settings does not substitute for that Codex trust
  decision. After installation, Ducklord shows the integration as installed
  but pending activation, tells the user to review and trust it through
  Codex `/hooks`, and marks it operational only after receiving a valid
  session-scoped hook event. No event means status remains unverified, not
  that the hook is assumed active.
- Activation belongs to the current installation and persists across daemon
  restarts. Removing and reinstalling the integration resets it to pending;
  historical callbacks remain diagnostic history, not activation evidence.
  Repeating an unchanged installation preserves its verified state.

### Local Notification Audio

- Ducklion sends notification events and does not store, stream, or play
  notification audio. Ducklord plays sounds on the operator's local machine.
- Ducklord exposes notification settings at three scopes: global defaults in
  its settings menu, Host defaults in Host actions, and per-Session overrides
  in Session actions. All three use the same four event classes and ordered
  delivery-level controls.
- Custom sound file paths are Ducklord-local settings. Different Ducklord
  users observing the same Ducklion session may choose different sounds.
  Remote hosts do not receive copies of the audio files.
- Custom audio supports WAV, MP3, and OGG in the first version. If a file is
  missing or cannot be decoded, skip only the sound; unread state and any
  other configured delivery remain unaffected.
- Missing, invalid, or unplayable local audio is reported in the settings UI,
  not as a repeated error popup for every notification event.
- Ducklord configures one local sound file per notification event class.
  Hosts and Sessions override delivery levels, not sound-file mappings.
- Four notification event classes are available for independent user control:
  agent task completed, agent task failed, user action needed, and generic
  terminal attention (allowlisted BEL/OSC). Terminal attention never claims
  an agent task completed. Each class may be enabled or disabled separately.
- Coalesce bursts of generic terminal-attention events from the same Session
  into one delivery. Do not coalesce agent task completed, task failed, or
  user-action-needed events. Deliver the first generic-attention event
  immediately, then suppress repeated generic-attention delivery from that
  Session for two seconds.
- Delivery may escalate from an in-app indicator to sound to a system
  notification. The ordered levels are **off**, **in-app indicator**,
  **sound**, and **system notification**; a higher level includes lower
  delivery levels. A level is selected independently for each event class.
- The baseline policy inherits from Ducklord's global default to a Host
  default to an optional per-session override. A session may explicitly
  choose **inherit Host** so later Host changes affect it. Host settings are
  defaults, not a cap: a session may choose a higher or lower level.
- If the effective Host/Session policy for an event class is **off**, do not
  deliver that event or create an unread mark. This differs from Project-focus
  suppression, which retains unread state without immediate delivery.
- Project focus is an explicit mode the user turns on or off. Merely selecting,
  browsing, or switching to a Project does not silently alter notification
  policy. Ducklord starts with Project focus off by default and does not
  restore the previous focus state after restart. When enabled, the selected
  Project keeps its normal notification policy; notifications from other
  Projects are delivered only when their
  effective per-session level meets or exceeds the user-selected focus
  threshold. The threshold is one Ducklord-wide setting, reused when switching
  the focused Project; it is not configured separately for each Project. Its
  default is **system notification**, the highest delivery level.
  Focus does not promote notifications to a higher level.
- While Project focus is active, mark the focused Project prominently in the
  Project pane and show the active other-Project threshold in Ducklord's
  status line.
- Project-pane actions expose a focus toggle for the currently selected
  Project, with a rebindable shortcut. The other-Project threshold is edited
  in Ducklord's global notification settings.
- Deleting the focused Project turns Project focus off immediately and shows a
  Ducklord notice; it does not silently choose another Project.
- Notifications originate from the Ducklion Session, not from each Project
  view. Ducklord evaluates the Host policy, then the Session policy, then the
  Project focus threshold (when enabled), before choosing in-app, sound, and
  system-notification delivery. A Session shown in multiple Projects produces
  one notification event, not one per view. If any of its Projects is focused,
  treat the Session as focused and do not apply the other-Project threshold.
- A notification event suppressed by the Project focus threshold still marks
  its Session unread (and contributes to its Projects' unread indicators), but
  does not temporarily move that Session to the top of the Session list pane.
  Turning focus off does not retroactively promote previously suppressed,
  still-unread Sessions; only new qualifying events can trigger promotion.
- Notification activity never reorders Projects in the Project pane. Only the
  Session list pane may temporarily reorder Sessions for qualifying events,
  across the entire list rather than within Project groups.
  A reorder preserves the identity of the currently selected Session-list
  item; its screen position may change, but the cursor must not silently
  select another Session. If its position moves outside the visible list,
  scroll the list to keep that selected item visible.
  Selecting a Session in that list navigates to an appropriate Project and
  switches the Terminal area to show that Session. If the Session appears in
  multiple Projects, stay in the currently viewed Project when it contains
  the Session; otherwise use the Project in which this user last viewed that
  Session. If neither applies, choose the first Project containing it in the
  user's Project-pane order.
- Moving the selection cursor in the Session list pane does not mark a Session
  as read, but immediately navigates the Project pane and Terminal area to
  that Session; Enter is not required. Its unread state clears only after the
  corresponding Session pane is actually displayed in the Terminal area and
  receives keyboard focus. Other visible Session panes in a split layout stay
  unread until each receives focus.
- Project focus can be enabled only for the currently selected Project. The
  Project pane selection follows the Project shown in the Terminal area, and
  the Terminal area switches to follow the Session selected in the Session
  list pane. Selecting a Project directly in the Project pane immediately
  switches the Terminal area to that Project's tabs and pane layout.
  The Session list pane is a one-way quick-navigation source: changing Project
  or Terminal-area selection does not change its selected row. Navigation
  alone does not enable or retarget Project focus.
- A new event for the currently keyboard-focused Session pane does not add an
  unread mark, but its configured sound and system-notification delivery still
  applies.
- A system notification identifies the Project, Session, and event class; it
  does not include raw agent output or prompt content. If the Session belongs
  to multiple Projects, show the focused Project when it contains the Session;
  otherwise use the currently viewed Project when it contains the Session,
  then the user's last-viewed Project for that Session, then the first
  containing Project in Project-pane order.
- In the first version, activating a desktop notification does not navigate
  or focus the Ducklord TUI; the user enters via the Session list pane.
- If the local environment cannot deliver desktop notifications, skip only
  that delivery channel. Its included sound and in-app indicator still work,
  and the settings UI reports that desktop notifications are unavailable.


## Goal

Duckway will add a developer-facing remote agent control plane that lives in the
Duckway repository but is delivered as independent binaries:

- `ducklord`: runs on a developer laptop and presents the management UI.
- `ducklion`: runs on a remote agent host and owns local session operations.
- `duckway ducklion`: compatibility wrapper that finds and executes the
  standalone `ducklion` binary.

The first MVP assumes the developer can reach remote hosts over ordinary SSH.
It does not require Tailscale SSH, but it works well inside a Tailscale network
because MagicDNS names and private addresses can be used as SSH targets.

## Non-Goals For MVP

- Do not relay terminal bytes through the Duckway server.
- Do not add Duckway server discovery APIs yet.
- Do not merge Discord CC sessions with Ducklord sessions.
- Do not require the remote host to run the normal `duckway` proxy/client daemon.

## Architecture

```text
developer laptop
  ducklord tui
    |
    | ordinary SSH, using the user's SSH config/keys/agent
    v
remote agent host
  ducklion
    |
    | local PTY supervisor
    v
  agent process / shell / codex / claude
```

## Standalone Host Provisioning (Target Design)

This section specifies the Ducklord-managed flow for a host that uses
Ducklion without the Duckway proxy/client.

Ducklord uses the operator's existing SSH access to detect the remote OS, CPU
architecture, and shell. It selects a compatible local Ducklion binary,
uploads it to a temporary path, verifies its checksum, and atomically installs
it at `~/.local/bin/ducklion`. It creates `~/.duckway/ducklion/` with private
`state/`, `logs/`, and `run/` subdirectories and records the installation mode,
manager, and version (`mode: standalone`, `managed_by: ducklord`). No Go,
container runtime, systemd, or Duckway proxy is required on the host.

Ducklion itself provides `ducklion daemon start`, `status`, `restart`,
`restart -f`, and `stop` for its daemon. Ducklord invokes those commands over SSH; it does not manage
the daemon PID. After starting, Ducklord verifies the SSH stdio bridge and
health endpoint. Only a successful probe allows Ducklord to save the host
entry. A failed installation or probe leaves an explicit error and no new
connected host entry.

For updates, Ducklord verifies the new binary's platform, checksum, and
version, then atomically replaces the installed binary. Installing a new
binary never automatically restarts Ducklion. Ducklord offers **Restart
safely**, **Force restart**, and **Later**. A safe restart waits for running
agent tasks to finish before replacing the daemon process; `-f` requests an
immediate daemon restart. Session PTY supervisors remain independent of the
daemon and survive either restart. The new daemon reconnects to supervisors
and restores sessions, output, and bindings. `stop` stops only the daemon;
session deletion requires an explicit session lifecycle operation.

Only one installation manager may own Ducklion on a host. A Duckway client
must not silently take over a `managed_by: ducklord` installation or start a
second daemon. Switching to Duckway-integrated management requires the
explicit local command specified below.

## Duckway Proxy-Integrated Host Provisioning (Target Design)

On a host using the Duckway proxy, the remote Duckway client is the sole
installer, version owner, and daemon manager for its bundled Ducklion. Ducklord
only connects over SSH and uses the Ducklion bridge. It must not replace the
binary or start/stop/restart Duckway services as a side effect of connecting.

The operator installs Duckway client and configures its server URL, token, and
proxy. Ordinarily, `duckway start` starts the proxy, CC watch, and separate
Ducklion daemon. Ducklord's Add Host flow probes the SSH bridge and Ducklion
health, then saves the host only after a successful probe.

If Duckway setup detects an existing standalone Ducklion, it displays a clear
instruction to run `duckway integrate ducklion` on that host. Until conversion
completes, `duckway start` may start proxy and CC watch but must skip Ducklion,
report that Ducklion remains standalone, and repeat the integration instruction.
It must not start a second Ducklion daemon against the same state root or
silently claim the standalone installation's ownership.

`duckway update` installs a mutually compatible Duckway client and Ducklion
version, verifies the installation, and reports that a restart is required. It
does not restart automatically. Ducklord may display a version mismatch but
must not perform the update on Duckway's behalf.
Before `duckway integrate ducklion` succeeds, an update may replace Duckway's
own bundled artifacts but must not overwrite the standalone Ducklion binary
or modify its state and manager marker. Ducklord remains responsible for
standalone Ducklion updates until conversion completes. `duckway update
--restart` is rejected; the operator explicitly runs `duckway restart` after
reviewing the update.

`duckway restart` waits for active agent tasks to finish before restarting the
three daemons. `duckway restart -f` skips the wait and must warn that active
work may be interrupted. Neither form destroys PTY sessions: independent PTY
supervisors remain alive and the restarted Ducklion reconnects to them. During
the restart Ducklord shows the host as temporarily disconnected and reconnects
its bridge when available.

Ducklord **Reconnect host** only re-establishes its own SSH/bridge connection.
It never invokes `duckway restart`; restarting remote services requires a
separate explicit operator action.

### Converting A Standalone Host

The operator runs `duckway integrate ducklion` on the remote host after
installing and configuring Duckway client. Reserve `duckway integrate <component>` as the command
family for future integrations; only `ducklion` is defined here. The command
must be run by the operator on the remote host; Ducklord does not offer a
button or SSH action that executes it on the operator's behalf. Duckway setup
only detects the standalone installation and prints the instruction; it does
not convert automatically. The command
converts `mode: standalone, managed_by: ducklord` to
`mode: integrated, managed_by: duckway` without changing Ducklion session IDs,
bindings, or the state root at `~/.duckway/ducklion/`.
This conversion is one-way. There is no supported command to return an
integrated host to standalone management; removing or stopping the Duckway
proxy does not transfer Ducklion ownership back to Ducklord.

The command takes an exclusive management lock, checks the installation mode,
Duckway/Ducklion compatibility, state permissions, and absence of running
agent tasks. If any agent task is running, it immediately fails with a clear
busy result; it does not queue a conversion or wait for the task to finish.
Open shell sessions do not count as busy and must remain alive across the
daemon handoff. The operator may retry later. After preflight, it stops only
the standalone Ducklion daemon, starts the Duckway-managed Ducklion daemon
against the same state root, and verifies that the daemon has reconnected to
the existing PTY supervisors. Only after health
and session-inventory checks succeed does it atomically write the new manager
marker. The proxy and CC watch remain under ordinary `duckway start` control;
integration must not implicitly start or restart them.

If a preflight fails, nothing changes. If takeover or verification fails,
the command stops the new daemon and restores the standalone daemon and
original manager marker. The design assumes the operator maintains a working
standalone installation; recovery from a failure to restart that original
daemon is outside this scope. The command must never run two Ducklion daemons
against the same state root. Ducklord may reconnect to the integrated host,
but no longer offers standalone install/update/restart actions.

After a successful conversion, Ducklord retains the existing host entry,
session identities, local groups, and sort order. On its next connection it
re-probes the Ducklion command path and management mode, updates only the
resolved connection metadata, and resumes the same session inventory. The
operator does not remove and re-add the host.

## Terminology

- Host entry: a Ducklord config item under `~/.ducklord/config.yaml` that
  describes one SSH-reachable remote host, its display name, group, SSH command,
  and Ducklion command path.
- Ducklion session: a remote PTY session owned by Ducklion on a host entry.
  These are listed with `ducklion list` and can be attached, read, started, or
  stopped.

Use `host entry` for Ducklord inventory records. Use `Ducklion session` for the
actual remote PTY process.

`ducklord` is the operator UI and SSH orchestrator. It does not store SSH
private keys and does not run remote commands through a shell locally.

`ducklion` is the remote command surface. The canonical entry point is the
standalone `ducklion` binary installed beside `duckway`. `duckway ducklion`
remains as a legacy compatibility wrapper, but it must not own supervisor
processes. Ducklion owns a self-managed PTY backend and returns typed JSON for
list/read/project operations.

## Configuration

MVP `ducklord` uses a local config file:

```yaml
name: dev-laptop
hosts:
  - name: vulns
    host: vulns.tailnet.example
    user: cjiso1117
    group: ctf
    ducklion: ducklion
```

Default path:

```text
~/.ducklord/config.yaml
```

`ducklord tui` does not require the file to exist. If it is missing, Ducklord
starts with an empty menu and lets the operator press `a` to add a host from
`~/.ssh/config` or a full SSH command. The config file is created when the first
host is saved. Non-TUI commands such as `ducklord sessions <client>` still need
a config entry because they must resolve `<client>`.

Override:

```bash
ducklord tui --config ./ducklord.yaml
```

Ducklord discovery is intentionally local and SSH-based:

```bash
ducklord ssh-hosts
ducklord import-ssh-hosts --config ~/.ducklord/config.yaml
```

Ducklord does not use Duckway server for discovery, authorization, or command
relay. The only security boundary is whether the operator can SSH to the
configured host.

## Installing Ducklord And Ducklion

The gateway `/install.sh` is interactive when run from a terminal. It shows a
checkbox menu:

```text
[x] Duckway client + Ducklion
[ ] Ducklord
```

Use `j/k` to move, `Space` to toggle, and `Enter` to continue. On a remote
agent host, keep `Duckway client + Ducklion` selected:

```bash
curl -fsSL http://your-duckway-gateway/install.sh | sh
```

The installer downloads the platform-specific `duckway-client-*` binary and the
matching `ducklion-*` binary, then installs them into the same directory:

```text
/usr/local/bin/duckway
/usr/local/bin/ducklion
```

or, for user-local installs:

```text
~/.local/bin/duckway
~/.local/bin/ducklion
```

On a developer laptop that only needs the SSH TUI, select only `Ducklord`. That
installs the platform-specific `ducklord-*` binary as:

```text
/usr/local/bin/ducklord
```

or, for user-local installs:

```text
~/.local/bin/ducklord
```

Non-interactive `curl ... | sh` installs `Duckway client + Ducklion` by
default. To script other modes:

```bash
DUCKWAY_INSTALL_COMPONENT=ducklord DUCKWAY_INSTALL=user \
  sh -c "$(curl -fsSL http://your-duckway-gateway/install.sh)"
```

For manual installs, `ducklord` expects the remote host to expose either:

```bash
ducklion version
```

or the compatibility wrapper:

```bash
duckway ducklion version
```

The standalone `ducklion` binary should be preferred because CC PTY runners and
Ducklord both look for a real `ducklion` executable first. When `duckway` and
`ducklion` are installed side by side, Duckway client PTY runners can still find
the companion binary even if the service `PATH` does not include that directory.

Ducklord can also install Ducklion over SSH when the developer laptop already
has a local `ducklion` binary:

```bash
ducklord import-ssh-hosts --config ~/.ducklord/config.yaml
ducklord install-ducklion vulns --source ./ducklion-linux-amd64 --config ~/.ducklord/config.yaml
ducklord probe vulns --config ~/.ducklord/config.yaml
```

By default the remote binary is written to `~/.local/bin/ducklion`. Use
`--dest /usr/local/bin/ducklion` only when the SSH user can write that path.
Ducklord stores the resolved remote path in its local config after the remote
`ducklion version` check succeeds.

## Operator Tutorial

This walkthrough creates a reproducible local demo with one developer laptop
container and three remote agent containers.

### 1. Build And Start The Demo

```bash
scripts/ducklord-podman-demo.sh
```

The script builds local `ducklord`, `duckway`, and `ducklion` binaries, creates
a private podman network, starts `ducklord-dev`, `ducklion-client-a`,
`ducklion-client-b`, and `ducklion-client-c`, writes
`/root/.ducklord/config.yaml` in `ducklord-dev`, and creates sample PTY
sessions.

`client-a` and `client-b` are pre-registered in Ducklord config. `client-c`
only exists in `/root/.ssh/config`, so it can be used to test the TUI add-host
flow.

### 2. Inspect Remote Clients And Sessions

```bash
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord ssh-hosts
podman exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord bookmarks client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.yaml
podman exec -it ducklord-dev ducklord attach-host client-a --config /root/.ducklord/config.yaml
```

Expected shape:

```text
CLIENT       SESSION            STATUS     AGENT        LAST
client-a     alpha              running    shell        ...
client-a     bash               running    shell        client-a:~$
client-a     build              running    shell        ...
```

`ducklord ssh-hosts` should include `client-a`, `client-b`, and `client-c`.
`ducklord probe client-a` should report `ducklion: available`. `ducklord
bookmarks client-a` should show the remote directory bookmark registry, including
`alpha-project`.

### 3. Open The TUI

```bash
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.yaml
```

Useful keys:

Press `?` to pin or unpin the centered shortcut reference. It stays visible
while normal shortcuts operate underneath it, is temporarily hidden by another
centered dialog, and reappears when that dialog closes. `Esc` does not close
the reference. It groups operations by their target: **Host**, **Session**,
**Session List & Groups**, and **PTY Panel**.
Host connect/disconnect/reconnect attaches or detaches every session and all
notification delivery for that host only for the current Ducklord process; it
does not stop remote PTYs or edit the host config. Add writes a host config and
Remove deletes it. Create/restart/end/destroy always target one session.

- `j` / `k` or arrow keys move across both group headers and sessions. Press
  `Enter` on a group to collapse or expand it without detaching the active PTY.
  On a group, `Left` always collapses and `Right` always expands; on a session,
  `Left` first selects its parent group.
- Mouse click selects a row.
- Drag a session onto another session to save its order. In host/type modes the
  target must be in the same group; custom mode may also move it across custom
  groups by dropping on a session or group header. Ducklord requests a
  `grabbing` mouse cursor through OSC 22 while dragging and restores `default`
  afterward; terminals without OSC 22 support simply ignore the hint.
- In custom mode, `Ctrl-J` / `Ctrl-K` moves a session through the global custom
  order and transfers it into the adjacent session's group at a boundary. An
  empty newly-created group is also a valid keyboard destination.
- Mouse-wheel input over the PTY pane scrolls its local framebuffer by three
  rows per notch. Mouse reports are consumed by Ducklord and never injected
  into the remote PTY.
- Copy mode keeps a frozen local framebuffer and accepts wheel or `j` / `k`
  scrolling. Use Shift-drag for terminal-native text selection.
- `Enter` or right-click focuses the selected session in the right pane.
- `Ctrl-]` returns keyboard focus to the left menu.
- `a` adds a Ducklion host from `~/.ssh/config`; use `client-c` in the demo.
- `c` creates a new remote session with the wizard. Choose `agent` or `shell`,
  then follow the type-specific flow.
- `m` opens session actions. Choose **Reconnect PTY output** to replace a
  broken viewer stream and rebuild the screen from Ducklion without restarting
  the shell/agent or changing its writer.
- `n` configures notification categories for the selected session.
- Hosts use stable, distinct palette colors in the session list. In custom
  organization mode, drag a session row onto a group header to move it; the
  same exact-identity operation remains available under `g` → **Move selected
  session** for keyboard-only use.

When the create wizard receives an absolute remote path that does not exist, it
shows a centered **CREATE REMOTE DIRECTORY** confirmation. Confirming asks
Ducklion to create the directory and all missing parents as the Ducklion Unix
user; choosing Back or pressing Esc leaves the remote filesystem unchanged.
Directory creation does not automatically register a bookmark—the existing
**Add path to bookmarks / Use path once** choice follows afterward.
- `E`, `R`, and `X` open confirmation views for end, restart, and destroy.
- `r` refreshes immediately.
- `q` quits.

Create and host-management workflows use centered modal dialogs. `Esc` moves
back one page; on the first create page it closes the dialog. Removing a host
requires choosing the configured host and confirming in a separate danger
dialog. It removes only the local Ducklord configuration, never remote sessions.

Remote shell discovery preserves the configured default `$SHELL` and offers
installed `zsh`, `bash`, and `sh` executables as explicit choices.

Shortcut bindings are optional in `~/.ducklord/config.yaml` and take effect
after Ducklord restarts. Values are one printable Unicode key, a `ctrl-X`
token, or `enter`. Unknown actions, control/format characters, and duplicate bindings fail
closed during config loading:

```yaml
shortcuts:
  help: "?"
  host_actions: "h"
  session_create: "c"
  session_restart: "R"
  list_search: "/"
  detail_previous: "k"
  detail_next: "j"
  detail_focus: "enter"
  pty_unfocus: "ctrl-]"
  shortcut_settings: "S"
```

In the redesigned Session list pane, qualifying unread sessions temporarily
sort to the top across Projects and return to the selected sort order when seen.
This projection preserves the selected Session identity and scrolls to keep it
visible. Set `promote_unread_sessions: false` to disable it; it never rewrites
the selected sort mode.

In host organization mode, the group header owns the host label, so nested
session rows do not repeat it. Disconnect keeps the host's last session rows
as gray read-only snapshots. **Host connections** opens one desired-state page:
checked means connected and unchecked means disconnected. Space toggles a host,
changed rows are highlighted with `◆`, and Enter applies every change together.
The independent Reconnect action forcibly rebuilds one selected live connection.

`S` opens the shortcut editor in the TUI. Saving writes the config atomically
but leaves the current keymap unchanged. Ducklord then asks whether to restart
its local TUI process; this never restarts or changes ownership of a remote
Ducklion session.

The session list shows `💀` when a process has stopped or an agent adapter is
unhealthy/non-responsive. A temporary host reconnect retains the last screen
instead of immediately declaring each remote process dead.

### 4. Interact With A Bash Session

1. Select `client-a / bash`.
2. Press `Enter`.
3. Type:

```bash
echo hello-from-ducklord
```

4. Press `Ctrl-]`.
5. Press `q`.

Verify the command reached the remote PTY:

```bash
podman exec ducklord-dev ducklord read client-a bash --lines 20 --config /root/.ducklord/config.yaml
```

### 5. Add A Host Entry From The TUI

Inside the TUI:

1. Press `a`.
2. Choose `client-c` by number, type `client-c`, or paste a full SSH command
   such as `ssh -p 2222 -i ~/.ssh/id_ed25519 duck@client-c`.
3. Press `Enter`.

Ducklord probes the remote host over SSH. It checks `ducklion version`, runs
`ducklion list --json` to verify the session manager entry point, and records
the host entry in `/root/.ducklord/config.yaml`. If Ducklion is missing,
Ducklord attempts to install the local `ducklion` binary to
`~/.local/bin/ducklion` over SSH, probes again, then records the resolved remote
path. If installation is not possible because the local binary is missing or SSH
cannot write the target path, Ducklord still adds the host and shows a clear
status message so the operator can enable the remote entry point manually.

Press `d` on a selected row to remove that host entry from the current
`config.yaml`. Removing a host entry does not stop remote Ducklion sessions; it
only removes the host from Ducklord's local inventory.

For a host that is reachable over SSH but still missing Ducklion, install the
local binary explicitly:

```bash
podman exec ducklord-dev ducklord install-ducklion client-c \
  --source /usr/local/bin/ducklion \
  --config /root/.ducklord/config.yaml
```

Verify the config was updated:

```bash
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord probe client-c --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord bookmarks client-c --config /root/.ducklord/config.yaml
```

### 6. Create A Remote Session From The TUI

Inside the TUI, press `c`, then follow the wizard:

```text
shell -> host -> bookmark or directory (default: Host home) -> shell -> handle
```

For example:

1. Create a shell-first Session pane.
2. Choose `client-a` by number or name.
3. Choose the `alpha-project` bookmark, use the Host home directory, or enter
   a directory. Confirm before creating missing directories recursively.
4. Choose an available shell; launch Codex or Claude inside it with your options.
5. Enter a Unicode display handle, or press Enter to use the folder name.

Ducklord fetches bookmarks with `ducklion bookmarks --json`, then revalidates the
directory and discovers available commands with
`ducklion agents --cwd <path> --json`. Discovery and final revalidation run in
cancellable workers so SSH latency never freezes navigation. A host generation
change discards stale results. Immediately before creation Ducklord re-reads the
bookmark registry and runtime capabilities; stale choices return to their
relevant step without a partial session. It then starts asynchronously and
selects the exact returned six-character session ID, including for duplicate
handles.

Verify from the terminal:

```bash
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord read client-a shell-alpha --lines 20 --config /root/.ducklord/config.yaml
```

The same start operation is available from the CLI:

```bash
podman exec ducklord-dev ducklord start client-a \
  --name scratch \
  --kind shell \
  --cwd /home/duck \
  -- bash \
  --config /root/.ducklord/config.yaml
```

### 7. Clean Up

```bash
podman rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c
podman network rm ducklord-demo
```

## CLI Contract

Remote host:

```bash
ducklion list --json [--tail-lines N]
ducklion bookmarks --json
ducklion start --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
ducklion read <name> [--lines N] [--json]
ducklion send <name> <text>
ducklion attach <name>
ducklion stop <name>
ducklion version
```

Developer laptop:

```bash
ducklord clients [--config <path>]
ducklord ssh-hosts
ducklord probe <client> [--config <path>]
ducklord sessions <client> [--config <path>]
ducklord bookmarks <client> [--config <path>]
ducklord tui [--config <path>] [--refresh 2s]
ducklord attach-host <client> [--config <path>]
ducklord attach <client> <session> [--config <path>]
ducklord read <client> <session> [--lines N] [--config <path>]
ducklord send <client> <session> <text> [--config <path>]
ducklord start <client> --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
ducklord stop <client> <session>
ducklord end <client> <session> [-w|--wait|-f|--force]
ducklord restart <client> <session> [-f|--force]
ducklord destroy <client> <session> [-w|--wait|-f|--force]
ducklord version
```

## TUI MVP

The current TUI supports:

- left-side grouped session list
- right-side selected session output preview
- keyboard navigation with arrow keys or `j` / `k`
- `Enter` or right-click to focus the selected session in the right pane
- keyboard input routing to the focused remote PTY session
- `Ctrl-]` to return focus to the left menu
- `c` to create an agent or shell session with separate type-specific flows
- `n` to configure notification categories for the selected session
- `ducklord attach-host <client>` to open the same split-pane view scoped to
  one remote host and its advertised Ducklion sessions
- `r` to refresh immediately
- `E`, `R`, and `X` open end, restart, and destroy confirmation panels;
  lifecycle requests run asynchronously so SSH recovery never freezes the UI
- `q` to quit
- basic xterm mouse click selection when the terminal supports SGR mouse mode
- a changed marker when recent remote output changes since the previous refresh

The right pane uses a bounded VT framebuffer with primary/alternate screens,
cursor movement, common erase/edit operations, SGR styles, wide and combining
characters, and soft-wrap-aware resize. Remote escape sequences are interpreted
inside that model and never replayed directly into Ducklord's own terminal;
rendering emits only locally generated, allowlisted SGR sequences.

## Target TUI Flow

The next TUI flow is two-stage:

1. Add or enable a Ducklion host.
2. Start a shell or agent session on one enabled host and one project.

Stage 1 uses local SSH metadata:

```text
ducklord -> ~/.ssh/config -> concrete Host choices
ducklord -> ssh <host> 'sh -lc <ducklion probe script>'
```

The probe distinguishes:

- `ducklion`: use the standalone binary
- `duckway ducklion`: legacy wrapper found on older hosts
- missing: ask the operator whether to install or enable Ducklion

Stage 2 asks for:

```text
shell -> host -> bookmark or directory (default: Host home) -> shell -> handle
```

Bookmarks and runtime availability come from:

```text
ducklord -> ssh -> ducklion bookmarks --json
ducklord -> ssh -> ducklion agents --cwd <directory> --json
```

`ducklion bookmarks --json` reads the existing shared directory registry under
`~/.duckway/cc-projects.json` and returns each entry with
`source: "duckway-client"`. Compatibility entries may still use the legacy
`source: "ducklion-default"` identifier; new shell-first Sessions default to
the Host user's home directory.

## Technical Details

### Listing And Preview

`ducklord tui` polls each configured client:

```text
ducklord -> ssh -> ducklion list --json --tail-lines N
```

`ducklord attach-host <client>` uses the same polling and preview path, but it
first narrows the loaded config to the selected client. The host-scoped TUI
keeps the left menu visible, opens focused sessions with `Enter` or right-click,
and disables add-host / new-session shortcuts so it acts as an attach surface
for the remote daemon's advertised sessions.

`ducklion list` returns records with status, agent type, last non-empty line,
and a hash of recent output. `ducklord` overwrites the remote `client` and
`group` fields with local config metadata, sanitizes remote display text, sorts
by group/client/session, and renders the left menu.

When a row is selected, `ducklord` reads a longer preview:

```text
ducklord -> ssh -> ducklion read <session> --lines 80
```

The passive `ducklord sessions` command also sanitizes remote last-line output
before printing it to the local terminal.

### Focusing A Session And Sending Input

When the operator presses `Enter` or right-clicks a session, `ducklord` starts
a streaming attach over SSH:

```text
ducklord tui
  -> exec.CommandContext("ssh", SSHArgs(..., "ducklion attach <session>"))
  -> stdin/stdout pipes
  -> remote ducklion attach
  -> local ducklion Unix socket
  -> session PTY
```

Input flow:

1. `readInput()` reads raw bytes from local `os.Stdin`.
2. `nextInputEvent()` splits coalesced input into key and mouse events.
3. In focused mode, `Ctrl-]` is intercepted by `ducklord` to return to the menu.
4. All other bytes are written directly to the attach process stdin.
5. SSH delivers those bytes to `ducklion attach`.
6. `ducklion attach` sends an attach request to the session control socket.
7. The supervisor handles `attach` with `io.Copy(sessionPTY, conn)`.

Output flow:

```text
remote process PTY
  -> supervisor capture goroutine
  -> per-session 0600 log
  -> attached Unix socket listeners
  -> ducklion attach stdout
  -> ssh stdout
  -> ducklord right pane renderer
```

The attach stream is generation-tagged inside `ducklord`, so stale output or
completion from an older attach cannot cancel or pollute a newer focused
session. `AttachSession.Done` is observed so SSH or remote attach errors are
shown instead of being mistaken for a clean EOF.

### Creating A Remote Session

TUI creation starts when the operator presses `c`.

State transitions:

```text
normal mode
  -> c
  -> host step: configured Ducklord client number or name
  -> directory step: saved bookmark, Host home, or typed directory
  -> optional confirmation: recursively create missing directory and/or save bookmark
  -> shell step: available shell reported by that host
  -> handle step: Unicode display handle; empty uses the directory name
  -> Enter starts the remote PTY session
```

The wizard is local and does not execute through a local shell:

1. The host step resolves a connected Ducklord client by number or name.
2. The directory step uses `ducklion bookmarks --json`, supports typed absolute
   paths with suggestions, and defaults to the Host home directory.
3. Ducklion validates that cwd and resolves its own `PATH` and login shell.
   `ducklion agents --cwd ... --json` always reports `shell` and reports Codex
   or Claude only when its executable is available on the remote host.
4. The shell-first flow offers available shells. The operator launches an
   agent from the resulting interactive shell with their own options.
5. The handle is an independent 1–128-code-point Unicode display name and may
   repeat. Empty input uses the final directory name. The six-character
   session ID, not the handle, remains the mutation/routing identity.
6. Remote discovery is asynchronous and request-fenced by wizard request ID,
   host generation, and Ducklion instance ID. Escape cancels immediately;
   shutdown joins discovery workers. Final creation revalidates the exact
   `(source, name, path)` bookmark tuple and runtime, then selects by returned ID.

The non-TUI `ducklord start` CLI accepts either `--kind shell` with exactly one
shell executable or `--agent <type>` with an agent command. It uses the same
validation as the wizard before connecting over SSH.

`ducklord` then starts the remote session asynchronously:

```text
goroutine:
  runner.Start(startCtx, client, startArgs)
    -> ssh
    -> ducklion start --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD
```

The TUI remains responsive while SSH start is in progress. `Esc`, `Ctrl-C`, or
`q` cancels the start context. On success, the main loop receives a
`startDoneEvent`, refreshes sessions, selects the new row, and reads its output.
On failure, the prompt remains open and shows the error.

The CLI path uses the same start-argument builder before connecting over SSH, so
invalid names, invalid agents, unknown options, and missing commands are
rejected locally.

### Ducklion PTY Supervisor

`ducklion start` creates state under `~/.ducklion/`:

```text
~/.ducklion/
  sessions.json
  sessions/<name>/
    control.sock
    output.log
    supervisor.err
```

The supervisor process is launched as:

```text
ducklion __supervise --name <name> --agent <agent> --cwd <dir> \
  --socket <socket> --log <log> -- CMD [ARGS...]
```

Inside `RunSupervisor`, `pty.StartWithSize(cmd, 40x120)` creates the child PTY
after the supervisor opens its `0600` Unix socket. The supervisor listens for:

- `send`: writes one command line to the PTY
- `attach`: streams bidirectional bytes between the socket and PTY
- `stop`: terminates the process group and closes the PTY

The capture path writes logs first, snapshots listener sockets while holding the
mutex, then writes to listeners outside the lock with deadlines. A slow attach
consumer therefore cannot block log capture or the whole PTY supervisor.

## Notifications

MVP notification is intentionally simple:

- `ducklion list --json --tail-lines N` returns a tail hash and last non-empty
  line for each session.
- `ducklord tui` polls remote clients on a refresh interval.
- If a running session's tail hash changes, the TUI marks it as changed and can
  ring the terminal bell.
- If a session changes from `running` to `stale` or `stopped`, the TUI marks it
  as changed.

Future notification upgrades:

- remote `ducklion watch --json` event stream over SSH
- server-side out-of-band events for "done", "blocked", and "needs input"
- desktop notifications from the local `ducklord` process

## Durable Agent Lifecycle

Discord task channels support `!end` and `!destroy` in three modes. With no
flag, an active or replying turn is rejected immediately and no barrier is
left behind. `-w`/`--wait` durably rejects new prompts, input, resize, and yield
requests while allowing the current final response and ACK to drain. There is
no wait timeout. `-f`/`--force` emits a failed cancellation event from the PTY
supervisor, fences later adapter output for that task, and terminates only after
the cancellation result is available for Discord delivery.

The request is identified by requester, request ID, session ID, operation,
mode, ownership epoch, and runtime generation. A different payload using the
same requester/request ID is an idempotency conflict. Ducklion reconstructs
workers from `pending_lifecycle_operations` after restart. Completed operations
are immutable entries in `lifecycle_outcomes`, so releasing a barrier never
loses replay safety. `end` archives Discord while retaining the stopped session
and binding; `destroy` hard-deletes both after runtime cleanup succeeds.

Agent `restart` waits for idle by default; `-f` first delivers a cancellation. It
retains the session ID, handle, writer, binding, working directory, and launch
command, then starts runtime generation `N+1`. Mode-0600 `runtime.json` remains
after process exit while the generation-specific recovery key is removed.
Restart creates a new key, atomically changes the session and lifecycle row to
`recovering`/`launching`, then starts the replacement supervisor. Ducklion
reconstructs `launching` work after daemon restart. A per-session
`runtime.lock`, held with a non-blocking OS file lock, makes repeated launch
attempts safe: at most one supervisor can own the PTY.

Shell sessions use the same lifecycle RPC from Ducklord only. Since shell PTYs
have no agent adapter or exclusive writer, restart/end/destroy are always
immediate: they terminate the current shell process directly and reject wait or
force modes. A retained shell launch spec contains one resolved executable and
the CWD, never arbitrary arguments or environment overrides. A definitive
replacement-launch failure removes the shell session from selectable inventory,
retains its diagnostic logs, and records an immutable failed receipt. A
definitive preparation failure after the old shell exits follows the same rule;
it must not leave a stopped shell retrying indefinitely. The operator can create
a new shell after correcting the cause. Managed-agent restart semantics remain
unchanged. Discord CC is never authorized to manage shell sessions.

Ducklord does not cancel an accepted durable lifecycle operation when its CLI
wait is interrupted or the TUI exits. The CLI reports that the request remains
durable, while the TUI permits normal navigation as soon as admission succeeds;
the eventual result is shown if that TUI remains open. A lifecycle cancellation
protocol is intentionally outside this version.

Schema v13 adds the immutable lifecycle failure text used by restart recovery.
No manual migration is required; Ducklion applies it transactionally at startup
and preserves the pre-migration database backup behavior.

## Security Boundaries

- SSH controls real connection permission.
- `ducklord` must validate client names, session names, users, and hosts before
  constructing SSH argv.
- `ducklord` must use `exec.Command` argv, not a local shell.
- Full SSH commands are split into argv and stored as the SSH executable plus
  options; the target host remains a separate validated field.
- `ducklord` disables SSH agent forwarding by default with SSH options such as
  `ForwardAgent=no` and `ClearAllForwardings=yes`.
- `ducklion` validates session names and delegates PTY socket/process
  construction to the PTY manager.
- `ducklion` strips `SSH_AUTH_SOCK` from supervised session environments by
  default. Broader session environment allowlisting remains future work.
- Remote session metadata must not contain secrets or prompts. Do not pass secrets
  in launch argv: agent argv and the canonical shell executable/CWD live in a
  mode-0600 retained runtime spec so restart can reproduce the process. The MVP
  stores only the newest 1 MiB of PTY output in generation-specific `0600`
  diagnostic logs. Logs expire after seven days by default, are capped at 32
  generations per session, and are removed with the session on destroy.
  The Duckway-configured `pty_log_retention_days` is the startup default.
  Ducklord Host control may set 1–3650 days through `host.retention_update`;
  Ducklion persists this in its private `host-settings.json`, applies it
  immediately, and runs a retention sweep. The saved Host setting takes
  precedence over Duckway's startup default on subsequent restarts.
  Ducklord's Host actions open a centered PTY log retention editor: it reads
  the effective Host value, requires explicit old→new confirmation, then
  verifies the value reported by Ducklion after saving. Disconnected Hosts
  cannot be edited.
- Ducklord does not trust Duckway server metadata and does not require Duckway
  server registration. SSH host access is the authorization boundary.

## Migration

Old Duckway clients do not have `ducklion`. They remain compatible because:

- Existing `duckway` behavior and state files are unchanged.
- Existing `~/.duckway/agent-sessions.json` records remain owned by
  `duckway session` and are not reused by `ducklion`.
- New `ducklion` PTY state is stored under `~/.ducklion/`.
- Existing `~/.duckway/cc-sessions.json` records are not imported or renamed.
- `ducklord` shows a clear remote error when `ducklion` is missing.

Operators can migrate old hosts one at a time by installing Ducklion over SSH:

```bash
ducklord tui --config ~/.ducklord/config.yaml
ducklord import-ssh-hosts --config ~/.ducklord/config.yaml
ducklord install-ducklion <client> --source ./ducklion-linux-amd64 --config ~/.ducklord/config.yaml
```

## Podman Demo

The repository includes a demo script that creates:

- one `ducklord-dev` container
- three remote client containers, each running sshd and standalone `ducklion`
- a private podman network
- SSH keys/config for the dev container
- sample PTY sessions on pre-registered clients

Before presenting the TUI, the script also runs a non-interactive release
smoke through the real SSH stdio bridge: it restarts a native shell while
preserving its session ID and advancing to generation 2, proves the replacement
PTY accepts input, rejects shell `--force`, reads retained generation-2 output
after end, validates mode-0600 storage, then destroys the session.

Run:

```bash
scripts/ducklord-podman-demo.sh
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord ssh-hosts
podman exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord bookmarks client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.yaml
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.yaml
```

To run only Ducklord in Podman against real SSH host entries, without starting
the demo remote client containers:

```bash
scripts/ducklord-podman.sh
```

This runs a multi-stage Podman build, so the host only needs Podman. The build
stage compiles Ducklord inside a Go container, creates a local Ducklord runtime
image, and starts:

```bash
ducklord tui --config /home/ducklord/.ducklord/config.yaml
```

The runner mounts the developer's `~/.ssh` and `~/.ducklord` into the container.
Its entrypoint creates a matching container user for the current host UID/GID
and runs Ducklord as that user, so files written through the mounts keep the
developer's ownership instead of becoming root-owned. Useful overrides:

```bash
SSH_DIR=~/.ssh-lab DUCKLORD_DIR=~/.ducklord-lab scripts/ducklord-podman.sh
DUCKLORD_PODMAN_SSH_MOUNT=ro scripts/ducklord-podman.sh
scripts/ducklord-podman.sh version
```

Pass extra Podman `run` options before the Ducklord command and use `--` to
separate them from Ducklord arguments:

```bash
scripts/ducklord-podman.sh \
  --podman-volume "$PWD:/workspace:rw" \
  -- tui --config /home/ducklord/.ducklord/config.yaml
```

Remote Ducklion sessions read the remote host's Duckway client config. For
non-shell agent sessions, Ducklion injects `HTTP_PROXY`, `HTTPS_PROXY`,
`NO_PROXY`, and Duckway CA bundle environment variables when
`~/.duckway/config.yaml` exists and `~/.duckway/proxy.pid` points at a live
Duckway proxy process. Shell sessions are left unchanged. Explicit env values
passed by higher-level callers override these defaults.

Inside the TUI:

- `j` / `k` or arrow keys move selection
- mouse click selects a row
- `Enter` or right-click focuses the selected session in the right pane
- `Ctrl-]` returns keyboard focus to the left menu
- `a` adds a host entry from `~/.ssh/config`; use `client-c`
- `d` removes the selected host entry from the current `config.yaml`
- `c` creates an agent or shell session with the type-specific wizard
- `m` opens the centered session action menu; unavailable actions remain visible
  with their reason. It also provides PTY-output reconnect; destructive
  lifecycle actions require confirmation
- `n` configures notifications for the selected session
- `E`, `R`, and `X` confirm end, restart, and destroy
- `q` exits
# Workspace pane layout / mouse amendment (2026-09-15)

- Project pane sits above Session list pane in the left sidebar; Terminal area occupies the right side.
- Pane headings/backgrounds and separators distinguish regions. Keyboard focus uses a contrasting configurable style via `workspace_theme`; local changes require restart.
- Mouse clicks select Projects, Sessions, Terminal tabs, individual Session panes and enabled modal choices. Modal input must not pass through to the underlying PTY.
- Clicking a PTY follows existing writer authorization, including pending-focus cancellation and stale-completion fencing; it never implies yield.
- Mouse wheel reports remain local scroll input. PTY application colors remain unchanged.
