# Current batch

## Objective
Repair terminal output-bookmark retention and make output-search results move
the visible terminal viewport to the selected match. Document the Project
export/import route for direct use.

## Delivery order
1. Preserve or deterministically recover bookmark anchors when retained terminal
   output changes.
2. Couple local search selection to the terminal viewport without leaking input
   to the PTY.
3. Add direct user-facing instructions for Project transfer.

## Acceptance criteria
- A bookmark selection reveals its retained anchor, or stays in the picker with
  a clear fallback when the anchor can no longer be recovered.
- Output search visibly reveals the selected match and preserves modal input
  ownership and exact focus restoration.
- Focused state tests and real PTY container E2E cover both behaviours.
- The routing specification, verification mapping, direct user instructions,
  affected checks, and restarted demo are current.

## Status
Complete — bookmark recovery, viewport-coupled search, documentation, routing
verification, and demo restart are recorded in `HANDOFF.md`.
