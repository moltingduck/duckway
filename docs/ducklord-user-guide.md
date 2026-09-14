# Ducklord quick guide

Start the TUI with `ducklord tui`. The normal workspace has three regions:

- **Project pane**: your local Projects. A Project can contain Sessions from
  several Hosts, and the same Session can appear in several Projects.
- **Session list pane**: one quick-navigation list across all Projects. Moving
  its selection switches the viewed Project and Terminal area, but does not
  take PTY control or clear unread notifications.
- **Terminal area**: the selected Project's tabs and split Session panes. A
  Session pane is a view of one Ducklion Session, not another agent process.

The highlighted border shows where keyboard input goes. Press `?` for a pinned,
searchable shortcut guide; press `?` again to close it. Shortcuts can be changed
with `S`; Ducklord asks whether to restart to load the new bindings.

## Navigate and arrange

| Key | Action |
| --- | --- |
| `j` / `k`, `↑` / `↓` | Navigate the focused list |
| `Enter` | Focus the selected Session pane; never implicitly yield ownership |
| `Ctrl+]` | Leave PTY input and return to navigation |
| `P` | Move focus to the Project pane |
| `N` / `Z` | Create / delete a local Project |
| `p` | Add a Session pane in the selected Project |
| `[` / `]` | Previous / next Terminal tab |
| `H` / `L` | Previous / next visible Session pane |
| `M` / `x` | Move / detach the selected local Session pane |
| `t` / `T` | Cycle quick-list sort / reverse event-time direction |
| `D` | Open the detailed Session list |
| `v` | Freeze redraws for terminal text selection |

Use `p` while the Project pane is focused: choose a new tab or horizontal or
vertical split, then **New shell session** or **Add existing session**. The
existing-session picker adds a local view without starting another process.
If that Session already has a pane in the Project, Ducklord asks whether to
move it; it will not duplicate it there. You can also drag a Session from the
quick list onto the Terminal area to choose its placement. Detaching a pane or
deleting a Project never stops the remote Session. Only explicit Session
**Destroy** does that. The built-in Default Project holds unclassified Sessions
and cannot be deleted.

In detailed-list mode, `/` searches Session, Host, and Project names; `f`
cycles All, Unread, Needs action, and Disconnected filters. Up/down previews
one Session on the right without clearing its unread mark. `Enter` focuses
that Session pane; `g` jumps to its Project in the normal three-region layout.

## Create and use Sessions

Press `c` for a shell-first Session, or choose **New shell session** from `p`.
Select a Host and directory; if you leave the directory unset, the Host's home
directory is used. The shell stays alive while you run Codex, Claude, or other
commands with your own CLI options. An agent exiting returns to that shell.

Ducklord may show `[codex?]`, `[claude?]`, or `[other agent?]` beside a shell
pane. The question mark means foreground-process detection is advisory: it
does not change writer ownership or claim that a task completed. Exact agent
completion/failure notifications require installed Host-side hooks. Install
or remove those through `h` → agent notification hooks after reviewing the
confirmation; Codex may also require trust approval in `/hooks`.
The Host hook dialog shows configuration presence separately from the last
observed callback. A callback is an advisory report from a process inside a
Session; it is not proof of the agent binary's identity and never authorizes
PTY control. A previous callback may remain visible after removing the hook.

When the root shell exits, its Session and panes disappear from the live
inventory. Ducklion retains recent PTY output separately for the configured
period (one week by default). Use `ducklord retained <host>` to find its Session
ID and generation, then
`ducklord read-retained <host> <session-id> <generation>` to inspect it.
Retention can be changed through Ducklord's Host
settings without restarting the shell Sessions.

## Notifications and control

A notification belongs to the shared Session, even if it appears in multiple
Projects. Project badges aggregate unread Sessions; they do not reorder the
Project pane. The quick Session list can temporarily promote eligible unread
events. A Session becomes seen when its pane actually receives keyboard focus,
not merely when you select its quick-list row or preview it in detailed mode.

Use `n` for the selected Session's notification level, `O` for global defaults,
and `h` for Host defaults. Each of task completed, task failed, action needed,
and terminal attention can be off, in-app, sound, or system notification.
Global settings also choose local WAV/MP3/OGG sounds. With the Project pane
focused, `F` toggles focus mode for that Project; other Projects then deliver
only events meeting the configured threshold, while retaining their unread
marks.

The Ducklion writer shown for an agent Session is authoritative. A read-only
view cannot send input. Use `y` to request immediate yield, or `Y` to wait
until the current task ends. A Discord Session can take control back with
`!yield` in its channel. Shell Sessions remain multi-writer and cannot be
controlled from Discord. `E`, `R`, and `X` end, restart, and destroy the
selected Session, each with a centered confirmation.

## Copy text and manage Hosts

Press `Ctrl+]` if the PTY is focused, then `v` to freeze redraws. Select text
with your terminal's mouse and use its normal Copy action. `Esc`, `q`, `v`, or
`Ctrl+C` resumes Ducklord. Copy mode does not detach or yield the Session.

Press `a` to add a Host from your SSH configuration. `h` opens Host actions,
including connect/disconnect, Host notification defaults, hook integration,
and retained-log settings. Ducklord keeps its local configuration under
`~/.ducklord`. Most settings require a Ducklord restart; it prompts rather
than restarting automatically. Host log retention is the hot-reload exception.

Central dialogs use `↑`/`↓` to select, `Enter` to continue, `Esc` to go back,
and `Ctrl+C` to close. Left/right arrows do not dismiss a dialog.

## Agent sign-in and live checks

Ducklion uses each agent's normal credential files
(`~/.codex/auth.json` and `~/.claude/.credentials.json`); Ducklord never
displays or stores them. Complete any per-project trust screen inside the
PTY. The demo preconfigures only Claude's non-sensitive onboarding appearance.

For an isolated, credential-backed interaction check, place mode-`600`
copies under `live-credentials/` and run:

```bash
CONTAINER_RUNTIME=podman scripts/ducklord-agent-tui-live-e2e.sh
```

Use `CONTAINER_RUNTIME=docker` for Docker, or `--codex-only` / `--claude-only`
to isolate a runtime. This test uses real provider calls and can incur usage;
never commit `live-credentials/`.
