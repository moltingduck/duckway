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
with `s`; Ducklord asks whether to restart to load the new bindings.

## Search terminal output and save output bookmarks

With a focused terminal, press the pane prefix followed by `/` to search the
retained terminal output. Type a query, then use Up/Down to select matches; the
terminal viewport moves to the selected line. Esc closes the local search and
restores terminal input to the same pane.

Press the pane prefix followed by `m` to save a labeled bookmark for the current
retained output position. Press the prefix followed by `M` to open the bookmark
picker. Enter reveals the saved line after later output is appended. Bookmarks
store only a session identity and line metadata, never terminal transcript text.
If the line has rolled out of retained scrollback, Ducklord marks it unavailable
and Enter shows the current retained history instead.

## Export or import a Project

Focus the Project pane with `b`, select the Project, then press the pane prefix
followed by `c` (by default, `Ctrl+B`, then `c`). Choose **Export Project**,
enter an absolute destination path, and press Enter twice: once to review the
path and once to write the file. The transfer contains the Project layout,
scoped notes, Session descriptors, and bookmark metadata; it excludes terminal
output, credentials, Host secrets, and SSH settings.

To import, open the same Project config menu and choose **Import Project**.
Enter the absolute path to the exported file, inspect **Project Import Preview**,
then press Enter on **Import and persist**. The preview reports Sessions that do
not exist locally and a Project-name collision. Missing Sessions are not created
or connected; a colliding Project receives a deterministic suffix after your
confirmation. Press Esc at any step to cancel or go back.

When Ducklord runs in the demo container, its path is a container path. Transfer
a file with `podman cp`, for example:

```sh
podman cp ducklord-verified-ducklord-dev:/root/project.duckproj.json ./project.duckproj.json
podman cp ./project.duckproj.json ducklord-verified-ducklord-dev:/root/project.duckproj.json
```

## Exchange Project files

Open **Project files** with `f` from the Project or quick Session list, with the pane
prefix then `f` (by default `Ctrl+B`, `f`) from a focused terminal, or by
choosing **Project files** in the Command palette (`Ctrl+B`, Space). The modal
has two directory columns; either column can be the source. `Tab` switches columns;
`h` chooses local, a Host, or the current Project shelf; `g` enters a path;
`/` searches the current directory only; Space marks entries; and Enter opens a
directory or confirms an action. Backspace opens the parent directory and clears
the filter. Choosing another endpoint or confirming a new path also clears the
current-directory filter. Wait for the directory to load and mark entries with
Space, then press `c` to copy them to the other column, or drag an
entry across columns; dropping onto a directory selects that directory as the
destination. Review the source, destination, and `skip`, `rename`, or `overwrite`
policy before confirming. Esc backs out of a form or closes the browser. During
a transfer, Ctrl-C requests cancellation; wait for cleanup to finish before
closing. Esc does not cancel a running transfer.

### Read the panels and transfer results

The left panel is cyan and the right panel is lavender. Each has its own border,
host name, current path, filter and selection counts. **Active** identifies the
panel receiving keyboard commands. On narrow terminals the panels stack.
Folders have a 📁 icon; press `i` for ASCII folder markers if your terminal renders
emoji poorly. Color is always accompanied by words or markers.

| Status | Meaning |
| --- | --- |
| Amber / copying | This item is being transferred; it is not yet confirmed complete |
| Green / copied, Sent or Received | The destination was committed; the source remains intact |
| Yellow / skipped | The destination already existed and the skip policy kept it |
| Red / failed | This attempted item failed; inspect the error |
| Cancelled / not started | The active item was interrupted, or this item was never attempted |

Preview shows the direction, endpoints, selected entries and conflict policy.
Progress counts actual completed items, not estimated bytes. Renamed results
show the destination name actually used. Highlights describe the latest batch
and only appear for its matching host and path.

Press `l` in the browser to see this Project's recent transfers. Left/Right
selects a batch; Up/Down selects an item and its Source/Destination path details.
Each row shows the source and actual destination filenames. Esc returns to the
same browser panel and cursor. Press `x` inside history to clear that Project's history and
highlights; this does not delete files. Up to 20 batches remain available after
closing/reopening the modal, until Ducklord exits. Completed entries remain
visible in history even if another entry failed or the transfer was cancelled.

Copies leave the source unchanged. Each transferred item is limited to 1 GiB.
Remote archives also have entry-count and path-depth limits. Copied files keep
the owner execute bit; files and directories are otherwise private to the owner.
Host-to-host copies stream through the controller without a persistent local
copy. Choosing **Project shelf** explicitly stores a persistent copy for that
Project. In a container demo, **Local** means the controller container's
filesystem, not the computer running your terminal. An existing
destination directory is refused for overwrite instead of being merged; use
skip or rename when appropriate. Temporary destination data is cleaned up on
cancel or failure. Remote Ducklions must support the file-exchange commands
before Host exchange is available. Closing restores the Project, Session, or
terminal that opened the modal, even if shell output arrived while it was open.
For a multi-entry copy, completed entries remain if a later entry fails or the
transfer is cancelled; the result reports copied and skipped counts. Symbolic
links and special files are not selectable sources.

## Navigate and arrange

| Key | Action |
| --- | --- |
| `j` / `k`, `↑` / `↓` | Navigate the focused list |
| `Enter` | Focus the selected Session pane; never implicitly yield ownership |
| `Ctrl+]` | Leave PTY input and return to navigation |
| `b` | Move focus to the Project pane |
| `e` / `Z` | Create / delete a local Project |
| `p` | Add a Session pane in the selected Project |
| `Ctrl+B`, then `-` / `\` / `t` | Add horizontal / vertical pane / new tab |
| `Ctrl+B`, then `,` | Rename the current Terminal tab |
| `Ctrl+B`, then `←` / `→` / `↑` / `↓` | Move to the visible pane in that direction |
| `Ctrl+B`, then `PageUp` / `PageDown` | Previous / next Terminal tab |
| `w` | Edit the selected Project's SSH hosts |
| `PageUp` / `PageDown` | Previous / next Terminal tab in Project navigation |
| `(` / `)` | Previous / next visible Session pane in traversal order |
| `u` / `x` | Move / detach the selected local Session pane |
| `i` | Toggle Project notification focus |
| `t` / `Ctrl+T` | Cycle quick-list sort / reverse event-time direction |
| `l` | Open the detailed Session list |
| `v` | Freeze redraws for terminal text selection |
| `s` / `Ctrl+O` | Shortcut / global notification settings |
| `A` | Remove Host configuration |

Up from the first Session list item enters the Project pane; down from the last
Project returns to the Session list. Prefix commands also work while typing in a
PTY. Configure `shortcuts.pane_prefix` (default `ctrl-b`) through `s` or config;
it must be an unused control key. Escape or any unknown suffix cancels locally.
Click `+` beside the tabs to add a new tab.

Shortcut design rules: ordinary actions use lowercase letters or control keys;
uppercase letters are reserved for important or destructive actions, such as
`Y` (yield when idle), `E` (end), `R` (restart), `X` (destroy), `Z` (delete
Project), and `A` (remove Host). Prefix arrows follow the visible pane layout;
PageUp/PageDown switch tabs. New shortcut defaults must avoid conflicts across
active contexts. Explicit customized bindings are preserved; the table shows
defaults. In config, PageUp/PageDown are spelled `pageup`/`pagedown`.

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
The shortcut editor exposes `detail_previous`, `detail_next`, and `detail_focus`
(defaults `k`, `j`, and `enter`). Arrow keys also navigate while the corresponding
`k`/`j` default remains; rebinding that action replaces its arrow alias too.

## Create and use Sessions

Press `c` for a shell-first Session, or choose **New shell session** from `p`.
Select a Host and directory; if you leave the directory unset, the Host's home
directory is used. The shell stays alive while you run Codex, Claude, or other
commands with your own CLI options. An agent exiting returns to that shell.

Saved Host directories are called **bookmarks**, separate from your local
Projects. Choose a bookmark or type a directory with autocomplete. A missing
directory requires confirmation before it is created, including missing parent
directories. Choose **Add path to bookmarks** to reuse it later, or **Use path
once**. From the CLI, list them with `ducklord bookmarks <host>`; on the Host,
save one with `ducklion bookmarks --add /absolute/path --name work`.

Ducklord may show `[codex?]`, `[claude?]`, or `[other agent?]` beside a shell
pane. The question mark means foreground-process detection is advisory: it
does not change writer ownership or claim that a task completed. Exact agent
completion/failure notifications require installed Host-side hooks. Install
or remove those through `h` → agent notification hooks after reviewing the
confirmation; Codex may also require trust approval in `/hooks`.
Hook status remains pending until a valid session callback arrives. Reinstalling
after removal requires a new callback; older callback history does not activate it.
The Host hook dialog shows configuration presence separately from the last
observed callback. A callback is an advisory report from a process inside a
Session; it is not proof of the agent binary's identity and never authorizes
PTY control. A previous callback may remain visible after removing the hook.

Shell Sessions retain tmux-like shared writing: another Ducklord can focus and
use the shell without `yield`. Agent detection inside that shell does not
change this policy. Managed agent Sessions still require writer ownership;
simply previewing either kind never sends input or resizes the remote PTY.

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

Press `a` to add a Host from your SSH configuration. `h` opens the Host list;
press Enter for that Host's settings, including connect/disconnect, Host
notification defaults, hook integration, retained-log settings, and Skills. Ducklord keeps its local configuration under
`~/.ducklord`. Most settings require a Ducklord restart; it prompts rather
than restarting automatically. Host log retention is the hot-reload exception.

In a Host's **Skills** setting, Ducklord opens a two-pane manager. The left
pane is Ducklord's separate managed-skill repository; the right pane is an
expandable list of that Host's agent skill directories. Use `Tab`, Left, or
Right to change pane. On an agent header, Enter or Space expands its remotely
discovered skills. Select a local skill and use `p` to mark it for push to the
active agent, `n` to leave it unmanaged, or `m` to rename the local managed
skill after confirmation. Select a remote skill and use `r` to preview a pull,
or `x` to delete that remote skill after the confirmation names its Host,
agent, directory, and skill ID. `d` deploys the active agent's selected
push skills. `Esc` returns one step; `Ctrl+C` closes the Skills route and
returns keyboard focus to the pane that opened it.

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

# Workspace 外觀與滑鼠

左側上方是 Project pane，下方是 Session list pane；右側是 Terminal area。
標題背景與分隔線區分窗格，取得鍵盤 focus 的窗格會使用不同配色。
點擊 Project、Session、Terminal tab 或 Session pane 可導覽；點擊 PTY
仍會經過既有的 writer 控制權檢查，不會自動 yield。中央選單可點選選項與操作提示，
停用選項不可點擊，危險操作仍需確認。固定的 `?` 說明也可點擊快捷鍵操作。

可在 `~/.ducklord/config.yaml` 自訂顏色，重開 Ducklord 後生效：

```yaml
workspace_theme:
  separator: "#526071"
  background: "#202833"
  foreground: "#c5cfdb"
  focus_background: "#24536b"
  focus_foreground: "#ffffff"
```

未指定的欄位沿用預設；PTY 內 agent 的原始配色不會被覆蓋。

## Project Hosts 與 Terminal tabs

建立 Project 時先勾選要包含的 SSH Hosts。之後新增 Session 或加入既有
Session，只會列出該 Project 允許的 Hosts。選取 Project 後按 `W` 可調整；
仍有 Session pane 使用的 Host 必須先移除對應 pane 才能取消勾選。
Default Project 維持接納所有 Hosts，舊 Project 未設定範圍時也維持原有行為。

點擊 tab 列的 `+` 可新增 tab，再選擇建立 Session 或加入既有 Session。
`Ctrl+B` 後按 `,` 可重新命名目前 tab；空白名稱恢復預設編號。
新 Shell Session 直接使用遠端 SSH 帳號的預設互動式 shell，不再要求選擇 shell。
