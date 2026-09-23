# File exchange visual contract

Route: `route.file-exchange`. Host copies still stream through Ducklord; this is
copy, never move. No direct host connectivity is required.

## Panels and identity

Two independent borders: left cyan, right lavender over dark blue-gray. Active
side has a brighter border and an explicit Active label. Each panel shows host,
path, visible/marked counts and active filter. Selection uses a cursor and [x].
Opening starts on the left at Ducklord's local working directory. The right
starts at the active session's host and working directory when available,
otherwise the current Project shelf. Endpoint identity must reflect those
defaults; reopening does not imply two local endpoints.
Folders use a fixed-width icon column (📁); `i` toggles ASCII [D]. All endpoint,
filename, and error text is sanitized before rendering. Clip by terminal cells.
Narrow screens stack both panels. Render and mouse regions share one layout
calculation; invisible rows have no hit region. Scrolling keeps the cursor visible.

## Transfer feedback

Preview states source, destination (including dragged folder), selected names,
and conflict policy before Enter. Busy state shows direction and actual item
progress, not fabricated byte percentages. Amber means active transfer, green
means committed success, yellow means skipped, red means failed; symbols and
words duplicate color meaning. Rename shows actual destination path. Overwrite
is labeled as the selected policy unless actual replacement is confirmed.
A directory result represents its entire subtree. Sources are explicitly Sent,
destinations Received. Success never appears before backend commit. Latest-batch
highlights are keyed by host identity plus absolute path, disappear for unrelated
paths, and remain until the next batch or history clear.

## Results and history

Keep up to 20 batches in memory per project, surviving close/reopen but not
application restart. Record timestamp, endpoint snapshots, names, policy and
per-item outcomes: queued, copying, copied, skipped, failed, cancelled, not started.
On partial failure/cancel retain completed items; do not label unattempted items
as failed. If validation fails before any item starts, show a batch error and
not-started items. Errors are sanitized and no credentials are displayed.
History rows pair the source basename with the actual destination basename.
Selected-item details show Source/Destination paths; compact heights prioritize
the selected outcome and target name, clipping path details when needed.
`l` opens history; Left/Right selects batches, Up/Down selects items, Esc returns
to the same browser side/cursor. `x` clears history/highlights within history only.

## Routing contract

Origin: project/session list `f`, focused terminal `prefix+f`, or palette action.
Open captures project/tab/pane/session and exact focus. The modal owns every key
and mouse event throughout browser, picker, filter, path, preview, busy, history.
Background output/list completion/progress cannot reclaim focus. History is scoped
to the captured project. Esc unwinds one form state; browser Esc restores origin.
Ctrl-C while busy cancels and awaits cleanup acknowledgement, otherwise closes.
If origin disappears, retain existing route.file-exchange fallback rules.
No modal command may be forwarded to the PTY.

```text
Project/list f / terminal prefix+f / palette
                    |
              FILE EXCHANGE (modal)
              +------------------------------+
              | LEFT cyan < Tab > RIGHT violet|
              | host/path         host/path   |
              | files             files       |
              +------------------------------+
               | h / g / /   -> endpoint/path/filter -> Enter/Esc -> browser
               | Space       -> mark entry
               | c / drag    -> PREVIEW -- Enter --> COPYING
               |                | Esc               | completion / Ctrl-C cleanup
               |                +--> browser <------+
               | l           -> HISTORY -- Esc --> same browser side
               |                arrows: batches/items; x: clear
               + Esc/Ctrl-C   -> exact origin (post-close PTY sentinel)
```

## Evidence requirements

State tests cover endpoint/path isolation, bounded project history, late events,
partial outcomes, cancel acknowledgement, selection restoration and wide/narrow
mouse geometry. Backend tests prove progress ordering against real committed
bytes, skips, failure and cancellation. Real container E2E proves both host
copy directions, resulting bytes/source preservation, rename/skip feedback,
history reopen, async modal ownership and terminal sentinel. Default suite,
review and matching demo artifacts gate completion.
