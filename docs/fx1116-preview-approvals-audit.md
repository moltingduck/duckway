# Rule preview, approvals anywhere, audit log (fx1116)

Design: console canvas https://claude.ai/artifact/NTHVrNXvSwYJkBwBWoz81p, page "Rule preview · approvals · audit".
Boards: RulePreview, ApproveTUI, ApproveDiscord and AuditLog.

User decisions (2026-11-16):
- ducklord approves with a scoped **approver token**, never an admin session.
- Chat channels with no approver list cannot decide approvals until one is added.
- The rule preview covers all three ACL layers: phantom token, credential (api_key) and service default.

## 1. Rule change preview

### Endpoints
Admin auth plus SameOrigin, the same as the other admin APIs. All three return the same response shape.

- `POST /api/placeholders/{id}/preview-impact` with `{permission_config, requires_approval?, approval_ttl_minutes?}`.
  This is the draft the token editor already sends to `acl/test`.
- `POST /api/keys/{id}/preview-impact` with `{acl}`.
- `POST /api/services/{id}/preview-impact` with `{default_acl}`.

### Replay
- Read `request_log` rows from the last 7 days.
  - For a token, read that token's rows.
  - For a key or a service, read the rows of every token whose ACL stack includes it.
- Group rows by distinct `(placeholder_id, method, path)`, newest first, up to 5,000 groups.
- Each group's weight is the sum of its rows' `repeat_count`.
- Evaluate every group twice with the side-effect-free decision path: once with the current config, once with the draft.
  - Use `gatewayState.decide` with `dryACLCheck`.
  - Resolving `@default` uses the simulator's bounded default-branch lookup.
  - Never use the enforcing check. Never write logs, approvals or rate counters.
- Put each group in one bucket:

| Bucket | Meaning |
|---|---|
| `newly_refused` | Allowed or held before, refused now. |
| `newly_allowed` | Refused before, allowed now. |
| `newly_held` | Not held before, held now. |
| `no_longer_held` | Held before, not held now. |
| `unknown` | The log lacks what is needed (see below). |
| `unchanged` | Same verdict. |

- A group goes to `unknown` when it needs the query string or the body. That covers:
  - rules with field or body constraints, or query constraints;
  - receive-pack pushes with no refs in `deny_detail`, which is every allowed push;
  - JSON ref updates whose decision depends on the body.
- Every `unknown` group carries a `reason`.
- Refused receive-pack rows that have `deny_detail.refs` are evaluated per ref using those refs.

### Response
```
{window_days, total_requests, distinct, truncated,
 buckets: {<bucket>: count},
 items: [{bucket, placeholder_id, token_name, method, path, client, hits,
          before:{verdict, kind, rule}, after:{verdict, kind, rule},
          reason?, simulate_url}]}
```
- Items are capped at 200 per bucket.
- For a key or a service, items are also grouped per token.
- Must finish within about 5 s for 5,000 groups. Add a timeout, and return a partial result with `truncated` set.

### UI
- A "Preview impact" button on:
  - the token editor (branch rules, the ACL builder and the approval setting);
  - the credential ACL editor;
  - the service default ACL editor.
- The result has tabs per bucket, a row table, and a "Test in simulator" link for each row.
- When `newly_refused` is above 0, Save asks for confirmation once.
- Layout rules:
  - CSP-safe;
  - cards at 900 px and below, usable at 412 px;
  - dark and light themes;
  - `aria-live` for the result.

## 2. Approvals anywhere

### Approver tokens
- New table `approver_tokens`:

| Column | Notes |
|---|---|
| `id` | |
| `name` | Unique. |
| `token_hash` | SHA-256 of a 32-byte random value with the prefix `dwa_`. |
| `created_at` | |
| `last_used_at` | |
| `revoked_at` | |

- The token is shown once, at creation.
- Admin APIs:
  - `GET /api/approvers`
  - `POST /api/approvers` with `{name}`, which returns `{id, name, token}` once
  - `DELETE /api/approvers/{id}`, which revokes
- UI: a Settings → Approvers section.
- Approver API, under `/approver/`, authenticated with `Authorization: Bearer dwa_…`:
  - `GET /approver/approvals?status=pending`
  - `GET /approver/approvals/{id}`
  - `POST /approver/approvals/{id}/approve` with `{duration_minutes?}`
  - `POST /approver/approvals/{id}/reject`
  - `GET /approver/events`, an SSE stream of pending-approval changes; reuse the existing events watcher if it fits.
- The approver API returns only approval fields:
  - client name, service, method, path, held-by reason, the redacted request summary, and timestamps;
  - never credentials, ACL JSON or logs.
- It has a constant-time hash lookup and rate-limits failed authentication.
- Client tokens (`/client`, `/proxy`) never authenticate here, and approver tokens never authenticate anywhere else.

### Who decided
- Add `approvals.decided_by TEXT NOT NULL DEFAULT ''` and `approvals.decided_via TEXT NOT NULL DEFAULT ''`.
- Values of `decided_by`:
  - `admin:<username>` from the web console;
  - `approver:<name>` from an approver token;
  - `discord:<user id>` or `telegram:<user id>` from chat;
  - `system:sweeper` for expiry.
- Values of `decided_via`: `web`, `tui`, `api`, `discord`, `telegram`, `sweeper`. An approver token decides with `tui` only when the client sends `X-Duckway-Approver-Client: ducklord-tui` (ducklord does); any other holder of a token is `api`.
- Show both in the web inbox history.
- A concurrent decision returns 409 with the existing decider, so a UI can show "already approved by …".

### Chat channels (`discord_bot`, `telegram`)
- Add an approver list to the channel config: Discord user IDs or role IDs, and Telegram user IDs. Edit it on the notification channel page.
- If the list is empty, the channel never decides.
  - A press or command gets the reply "not an approver".
  - The console shows a banner on the Approvals page and on the channel.
- Discord:
  - Held-request messages get message-component buttons: Approve, "Approve for 1 h", and Reject.
  - Handle `INTERACTION_CREATE` on the existing gateway connection.
  - Acknowledge within 3 s.
  - A press from someone not on the list gets an ephemeral reply.
  - After a decision, edit the message: remove the buttons and add a result line with the decider and the time.
  - Do the same edit when the decision came from elsewhere (web or TUI), on a best-effort basis.
  - Reactions and `!approve` keep working, but only for listed users.
- Telegram: inline keyboard buttons with the same rules.
- Keep `GuardChannelAction`.

### ducklord TUI
This part comes after the imp1116 split lands.

- `ducklord approver login [--server URL]` reads the token from stdin or a prompt and stores it in ducklord config with mode 0600.
- `ducklord approver logout`.
- The token must never be copied to ducklion hosts, agent containers or session environments.
- The approvals panel:
  - opens with `a`, and from the existing approval-required notification;
  - the status bar shows a pending count;
  - list plus detail, with keys ↑/↓, `y` approve, `Y` approve for a chosen duration, `n` reject, `enter` body, `h` history, `esc` back;
  - updates live from `/approver/events`;
  - works under NO_COLOR and TERM=dumb, and at 80 columns, where the detail goes below the list.

## 3. Audit log

### Storage
- New table `admin_audit_log`:

| Column | Notes |
|---|---|
| `id` | INTEGER PK autoincrement. |
| `created_at` | |
| `actor` | `admin:<user>`, `approver:<name>`, `discord:<id>`, `telegram:<id>`, `system:<x>` or `anonymous`. |
| `via` | `web`, `api`, `tui`, `discord`, `telegram`, `sweeper` or `system`. Admin console actions are `web` whether they come from a browser page or the console's JSON API (both are signed-in admin sessions, so the client cannot be told apart). |
| `remote_addr` | |
| `action` | For example `placeholder.update`. |
| `resource_type` | |
| `resource_id` | |
| `resource_name` | |
| `summary` | |
| `before_json` | |
| `after_json` | |
| `status` | `ok` or `failed`. |

- Add an index on `(created_at)` and one on `(resource_type, resource_id, created_at)`.
- Portable SQL. Retention is set in Settings with a default of 365 days. A sweeper deletes older rows.

### What is recorded
- Every admin API mutation (any non-GET under `adminAPIMux`, plus the `/admin/approvals` form routes). A mux wrapper records method, route pattern, actor, status and resource ID.
- Before/after JSON for resources that carry rules or access:
  - service `default_acl`;
  - key `acl`, name, active state and proxy (redacted);
  - placeholder `permission_config` and approval settings;
  - groups and their members, clients, notification channels (the approver list), approver tokens, and settings.
- Approval decisions from every path.
- Sign-ins:
  - successes and failures, with the username tried for failures;
  - throttle repeated failures into one row per minute per IP.
- Secrets are never stored:
  - key values, refresh tokens, client tokens, approver tokens, passwords, bot tokens and webhook URLs become `"(changed)"` or `"(set)"`;
  - reuse the existing redaction helpers.
- A failure to write the audit row must not fail the user's action. Log it instead.

### API and UI
- `GET /api/audit` with filters `actor`, `kind` (rules | approvals | credentials | clients | signins | settings), `since`, `q`, `resource_type` and `resource_id`, using cursor paging.
- An `/admin/audit` page with filters, a list and a detail view showing a line diff of before and after.
- A History tab on the phantom token, credential and service pages, filtered to that resource.
- Read-only. There is no delete from the UI.

## 4. Tests

- **Preview:**
  - bucket assignment for each kind;
  - the `unknown` reasons;
  - no side effects (no logs, approvals or counters);
  - key and service fan-out to tokens;
  - the cap and timeout;
  - JS tests for the UI helpers.
- **Approvers:**
  - token auth isolation: a client token gets 401 on `/approver/`, and an approver token gets 401 on `/api/` and `/client/`;
  - revoke;
  - the hash-only store;
  - a concurrent decision gives 409 with the decider.
- **Chat:**
  - allowlist enforcement for buttons, reactions and commands;
  - the empty list denies;
  - interaction handling, with a fake gateway.
- **Audit:**
  - a row for each mutation family;
  - the redaction table;
  - an audit write failure does not break the action;
  - retention;
  - sign-in throttling.
- **E2E:** `scripts/e2e-test.sh` additions for preview, the approver API and the audit API.
- **TUI E2E:** the approvals panel.
