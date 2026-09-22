# Pane implementation status

Updated: 2026-09-17. The active route and reliability work is tracked in
[TASK.md](../TASK.md) and [HANDOFF.md](../HANDOFF.md).

## Current acceptance route

- Help is toggled only by `?`.
- Project and focused-session Notes routes are discoverable.
- Notes container TUI E2E state is isolated per run.
- The relevant full `go test` suite passes.
- High-output forwarding obeys its byte bound and lag recovery reaches the
  prompt and `FLOOD_RECOVERED`.
- The rebuilt demo restarts successfully after validation.

## Historical context

The implementation has previously covered stacked Project/Session panes,
Notes modal and scoped-book routing, form/editor and clipboard behavior,
Project Ctrl-C cancellation, and bounded supervisor output forwarding. Exact
commands and outcomes belong to the handoff for the batch that ran them; this
summary does not promote them to current verification.

## Documentation contract

New routes must update [pane-routing.md](pane-routing.md), the executable
`TestPaneRouteContract` matrix, and the relevant isolated container E2E when
terminal, modal, editor, PTY, or persistence behavior is involved. See the
[developer guide](developer-guide.md#ducklord-ui-route-contracts).
