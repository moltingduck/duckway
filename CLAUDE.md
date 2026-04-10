# Duckway

## Duckllo Workflow Rules

These rules are mandatory for all agents. Violations are not acceptable.

### Card Lifecycle (never skip steps)
1. **Before coding**: Create a card via the API. As an agent, your card always goes to the `Proposed` column with `pending` approval. Wait for the product owner to approve it — approved cards are auto-moved to `Todo`.
2. **Start work**: Use `POST /cards/:cid/pickup` to move the card to `In Progress` and assign yourself. This is required — never leave In Progress cards unassigned. The pickup endpoint does both atomically.
3. **While coding**: Add comments to the card describing your approach and key decisions.
4. **After coding — MUST test**: Run all relevant tests. Update the card with:
   - `testing_status`: `passing`, `failing`, or `partial`
   - `testing_result`: Paste the actual test output (not just "tests pass" — include real output)
5. **After coding — MUST demo**: If the card has any UI/UX/frontend/user-facing changes, upload a demo GIF or screenshot to the card. This is NOT optional.
6. **After coding — commit ref**: Add a comment with the git commit hash.
7. **Move to Review/Done**: The server enforces quality gates. Cards CANNOT move to Review or Done without:
   - Test results (testing_status + testing_result)
   - Demo media for UI-related cards (labels: `ui`, `ux`, `frontend`, `user-operation`, `user-facing`, `demo-required`)

### Rules you MUST follow
- **NEVER** tell the user a feature is "done" without running tests first and posting results to the card.
- **NEVER** skip uploading a demo GIF/screenshot for any user-visible change.
- **NEVER** move a card to Done without both test results AND demo media (if applicable).
- **ALWAYS** create the kanban card BEFORE you start coding.
- **ALWAYS** update the card with real test output, not summaries.
- If tests fail, fix them. Do not mark the card as done with failing tests.

### Proposed → Todo Approval Flow
- Agent cards always go to the **Proposed** column — you cannot create cards in Todo directly.
- The product owner reviews your proposal and approves or rejects it.
- **Approved** cards are auto-moved to **Todo** — ready for implementation.
- **Rejected** cards stay in Proposed — update your plan and the owner can re-review.
- Cards already in Todo are approved. Do NOT wait for approval on Todo cards.

### Quality Gate Labels
Cards with these labels MUST have a demo GIF/media:
`ui`, `ux`, `frontend`, `user-operation`, `user-facing`, `demo-required`

<!-- Duckllo server: http://localhost:3000 | Project: duckway -->
