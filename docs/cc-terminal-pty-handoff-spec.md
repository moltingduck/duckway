# CC ↔ Terminal PTY Handoff Specification

Status: V1 design converged; implementation pending

This document records only decisions confirmed with the user. Undecided
behavior is added after its corresponding design discussion.

## 1. Ownership scope

Status: Decided

PTY control ownership is scoped to one task-channel session, identified by its
stable Duckway channel handle.

### Requirements

- A task-channel session has at most one control and reply owner at a time.
- The owner is either `cc` or `terminal`.
- A terminal handoff affects only the selected task-channel session.
- Other task-channel sessions assigned to the same Duckway client continue to
  accept and process work independently.
- Ownership follows the stable channel handle; renaming a Discord channel does
  not change or release ownership.
- A management channel cannot be owned or attached by a terminal.
- Management channels remain available as administrative entry points while a
  task-channel session is terminal-owned.
- Management commands may inspect or manage a terminal-owned task session, but
  ordinary management-channel prompts must not be routed into that session as
  agent input.

### Invariant

For every task-channel handle `h`, Duckway must enforce:

```text
active_control_sources(h) <= 1
active_reply_destinations(h) <= 1
```

The ownership state of one handle must not block work on another handle.

## 2. Terminal interaction model

Status: Decided

A handoff-enabled task session uses one persistent native PTY. The CC and a
terminal attachment take turns controlling that same PTY; they must not launch
separate agent processes or represent a terminal turn as a brokered headless
request.

### Requirements

- Ducklion owns the persistent PTY and its agent process independently of any
  Discord connection or terminal attachment.
- A terminal owner attaches directly to the agent's native terminal UI,
  including normal output, input, resize, and terminal control sequences.
- When the CC owns the session, Duckway sends CC input to that same PTY and
  derives progress and the final reply from that PTY session.
- Detaching a terminal must not terminate the PTY, agent process, or agent
  conversation.
- Ownership transfer must not restart the agent or create a second agent
  conversation.
- Duckway must preserve the task-channel handle to PTY identity mapping across
  client restarts whenever the underlying Ducklion session is recoverable.
- Input from a non-owner must be rejected before it reaches PTY stdin.
- Output generated during a turn must be routed only to the reply destination
  selected by the ownership epoch under which that turn began.
- Handoff commands are control-plane operations. They must not be encoded as
  agent prompts or written to PTY stdin.

### Excluded model

The terminal is not a prompt broker that launches a new `codex exec`,
`codex exec resume`, or equivalent headless process for every terminal turn.
Handoff-enabled sessions require native PTY attachment to the persistent agent
process.

### Consequence

The existing CC per-turn PTY lifecycle is insufficient for handoff-enabled
sessions. Ducklion must expose a persistent-session lifecycle and Duckway must
be able to determine whether the native agent is idle before changing owners.

## 3. Ducklion role and ownership transfer

Status: Decided

Ducklion is Duckway's built-in PTY management subsystem and the authority for
PTY session lifecycle, task activity, attachment permissions, and ownership.

### Process model

- Ducklion ships as part of Duckway but runs as an independent local process.
- `duckway start`, `duckway restart`, and corresponding lifecycle operations
  start or supervise Ducklion.
- Ducklord requests the PTY session list from the host's Ducklion after it
  connects.
- A CC-owned PTY is visible and attachable read-only from Ducklord.
- Only the current owner may send PTY input or receive routed replies.

### Immediate yield

Ownership transfer is unilateral. It does not require approval or a pending
request from the current owner.

- From Ducklord, selecting a CC-owned PTY and invoking the yield shortcut asks
  the host Ducklion to transfer ownership to `terminal`.
- From Discord, `!yield` asks Ducklion to transfer ownership back to `cc`.
- Ducklion atomically verifies that the PTY has no active task before changing
  the owner and incrementing the ownership epoch.
- If a task is active, immediate yield fails without changing ownership.
- After a successful terminal takeover, Duckway posts an ownership-transfer
  notice to the original CC channel.
- After a successful CC takeover, the terminal becomes read-only or detached
  according to the terminal attachment policy defined later.

### Waiting yield

Both control surfaces may request a waiting transfer:

```text
yield -w
yield --wait
```

- If the PTY is already idle, waiting yield behaves like immediate yield.
- If a task is active, Ducklion records one pending transfer request.
- As soon as the active task reaches a terminal state, Ducklion atomically
  revalidates the session and attempts the ownership transfer.
- A pending request must be fenced by its ownership epoch so it cannot apply
  after another ownership change.
- Exclusivity and restart recovery for pending requests are specified
  separately.

## 4. Managed-agent eligibility

Status: Decided

Only a PTY backed by a reliable Ducklion agent adapter may participate in CC
ownership or yield.

### Requirements

- The adapter is responsible for determining semantic task states such as
  `idle`, `running`, `completed`, `failed`, and `cancelled`.
- Ducklion is the authority for task state. Controllers submit work through
  Ducklion and must not write directly to a CC-managed PTY.
- Ducklion must reject CC binding and every yield operation when the session's
  adapter is absent, unsupported, unhealthy, or unable to determine state.
- Screen contents, process existence, elapsed time, and terminal silence are
  not sufficient evidence that an agent is idle.
- A shell PTY without an adapter may be created and operated by Ducklord under
  its separate shared-writer rules.
- A shell session cannot be CC-owned, cannot accept CC tasks or Discord binding,
  and cannot transfer ownership with yield.
- Adapter capability and health must be visible in Ducklion's session metadata
  so Ducklord can disable unavailable actions instead of offering a yield that
  will fail later.

### Invariant

```text
cc_managed(session) or yieldable(session)
    implies adapter_supported(session) and adapter_healthy(session)
```

## 5. Pending yield lifecycle

Status: Decided

- `yield -w` and `yield --wait` create a pending ownership-transfer request
  when the session is running.
- Each session may have at most one pending yield.
- Every later yield request is rejected while a pending yield exists, including
  a repeat from the same requester.
- A pending yield has no cancellation operation and no time-based expiry.
- A pending request is bound to the ownership epoch in which it was created.
  It becomes invalid if that epoch changes for any other reason.
- When the task completes, times out, or is terminated by restart, Ducklion must
  revalidate adapter health, current owner, epoch, and absence of another active
  task in one atomic transition before transferring ownership.

## 6. Attachment behavior after yield to CC

Status: Decided

When ownership transfers from `terminal` to `cc`, existing terminal
attachments remain connected in read-only mode.

### Requirements

- Ducklion atomically changes the owner and revokes terminal write permission.
- Ducklion must reject all subsequent terminal input before it reaches PTY
  stdin, including input already buffered by an attachment but not yet applied.
- The attachment continues receiving PTY output generated under CC ownership.
- Ducklord displays an explicit `Control transferred to CC; read-only` status.
- The ownership change must not detach the viewer, stop the PTY, or restart the
  agent.
- Regaining terminal ownership may upgrade an eligible existing read-only
  attachment whose Ducklord-provided owner name matches the new owner.

## 7. Terminal owner identity and disconnect behavior

Status: Decided

Terminal ownership belongs to a logical owner name supplied by Ducklord, not
to an attachment connection.

### Requirements

- Ducklord supplies its owner name when connecting to the host Ducklion. The
  configured Ducklord name or hostname may be used as that value.
- Because Ducklord has already authenticated to the host over SSH, Ducklion
  trusts the owner name supplied through that connection.
- Ducklion persists the terminal owner name with the session ownership state
  and uses it for authorization, display, audit messages, and subsequent
  attachment decisions.
- A terminal attachment may write only when its supplied owner name equals the
  session's persisted terminal owner name and the session owner is `terminal`.
- Other attached terminals remain read-only.
- Disconnecting the current writer does not release, expire, or otherwise
  modify terminal ownership.
- Ducklion does not track an orphaned-writer state and does not automatically
  promote another attachment after disconnect.
- Control remains with the persisted terminal owner until a control source
  explicitly requests yield and Ducklion approves it while the adapter reports
  no active task.
- Ownership notifications identify the terminal owner by its persisted name,
  not by a transient connection or process identifier.

## 8. Same-name reconnection

Status: Decided

- Ducklord owner names are stable logical principals, not unique connection IDs.
- A Ducklord attachment whose supplied name matches the persisted terminal
  owner may regain write access immediately after connecting.
- Reconnection by the same owner name does not require another yield and does
  not change the ownership epoch.
- Ducklion permits only one live Ducklord bridge connection for a given owner
  name. A second concurrent connection using that name is rejected.
- After the existing bridge disconnects, a new bridge may reuse the name and
  regain the persisted owner's write access.
- Users are responsible for assigning and managing unique Ducklord owner names.

## 9. CC input while terminal-owned

Status: Decided

- Ordinary messages in a Discord task channel are admitted to the durable inbox
  without requiring the server to mirror Ducklion ownership.
- When connected, the normal durable-ingress path pushes or exposes the message
  to the Duckway client, which submits it to the bound Ducklion session.
- Ducklion is the authoritative writer gate and rejects the task before PTY
  input when the requester does not match the current owner.
- An ownership rejection is a successfully handled business outcome for the
  durable inbox. It is completed rather than retried or sent to dead-letter.
- Duckway replies in the Discord task channel with the persisted terminal owner
  name and tells the user to use `!yield` or `!yield -w` to request CC
  ownership.
- A rejected message must never execute later after ownership returns to CC.
- Ownership and management commands remain available and are not treated as
  agent input.
- The rejection decision records Ducklion's current ownership epoch and occurs
  before any PTY input.

## 10. Multiple attachments and input

Status: Decided

Ducklion supports shared reading for every session and shared writing for shell
sessions.

- Multiple attachments may view the same PTY concurrently.
- For an agent session under terminal ownership, only the attachment on the sole
  live Ducklord bridge whose name matches the persisted owner may write;
  attachments with other names are read-only.
- For a shell session, every authenticated Ducklord bridge attachment may write,
  regardless of its owner name. Ducklion serializes their input bytes in arrival
  order and does not prevent command or character interleaving.
- Ducklion does not automatically transfer agent-session writer ownership to a
  reader when the writer bridge disconnects.
- Ducklord owner-name assignment and uniqueness are the user's responsibility.

## 11. PTY size and reader viewports

Status: Decided

- For an agent session, only the current writer controls the PTY rows and
  columns.
- Under agent CC ownership, Ducklion uses the configured CC PTY size.
- Under agent terminal ownership, size updates are accepted only from attachments
  whose Ducklord owner name matches the persisted terminal owner name.
- Read-only attachments never send a terminal resize operation to the PTY.
- All attachments consume the same PTY framebuffer.
- A smaller reader crops or scrolls the shared framebuffer locally.
- A larger reader displays unused space locally.
- Writer disconnection preserves the last PTY size until an authorized writer
  reconnects or ownership changes.
- Reader resize operations must not trigger agent TUI redraws or otherwise
  mutate the shared PTY session.

## 12. Yield-safe idle state

Status: Decided

Ducklion may approve an ownership transfer only when all of the following are
true in one atomic state check:

```text
adapter_state == idle
pending_pty_input == 0
pending_cc_tasks == 0
reply_routing_in_progress == false
```

### Requirements

- An adapter completion event alone is insufficient while buffered input or
  reply delivery remains outstanding.
- `pending_pty_input` includes accepted input that has not been fully written to
  the PTY.
- `pending_cc_tasks` includes admitted or claimed CC work assigned to this
  session but not yet started or terminally resolved.
- `reply_routing_in_progress` remains true until the final reply has either
  succeeded or reached its defined terminal delivery-failure state.
- The immediate form of yield fails when any condition is false.
- A valid waiting yield remains pending and is re-evaluated whenever one of
  these conditions changes.
- Ownership transfer and ownership-epoch increment occur atomically with the
  final revalidation.

## 13. Output and reply routing

Status: Decided

- Every task records an immutable owner and ownership epoch when it starts.
- Native PTY output remains visible to all connected attachments, subject to
  their local viewport.
- For a CC-owned task, Duckway sends semantic progress and the final reply to
  the bound Discord task channel.
- For a terminal-owned task, Duckway does not mirror progress or the final
  answer to Discord; terminal attachments observe the native PTY output.
- Discord receives ownership-transfer audit notices regardless of the new
  owner.
- Late progress, completion, or delivery callbacks must carry the task's epoch.
  They may update only the reply destination recorded for that epoch.
- A callback from an older epoch must never be rerouted to the current owner.
- Yield remains unavailable until reply routing for the current task reaches a
  successful or terminally failed state, as defined by the yield-safe idle
  requirements.

## 14. Pending-yield drain barrier

Status: Decided

- Accepting `yield -w` or `yield --wait` immediately places the session in
  drain mode.
- The task active when the request is accepted continues to completion.
- Ducklion rejects every new agent task or turn from both CC and terminal
  control sources while drain mode is active.
- For a CC-owned session, Discord messages may still enter the durable inbox;
  Ducklion rejects them as drain-barrier business outcomes before PTY input and
  the client completes those inbox records without agent execution.
- For a terminal-owned session, Ducklion rejects new task-starting input before
  it reaches PTY stdin.
- Status and lifecycle control-plane operations remain available, but every
  additional yield request is rejected.
- Drain mode normally ends only when the pending yield succeeds and ownership
  transfers to its requester.
- Destroying the session removes the pending request together with the session;
  there is no standalone pending-yield cancellation path.

## 15. Ducklion restart recovery

Status: Decided

- Ducklion persists the session owner, terminal owner name, ownership epoch,
  adapter identity, task state, and any pending yield.
- After restart, Ducklion re-adopts surviving PTY supervisors and associates
  them with their persisted sessions.
- Every recovered session starts in a fail-closed read-only recovery state.
- No controller may write or yield until the agent adapter reconnects and
  provides a reliable task-state synchronization.
- After successful synchronization, Ducklion restores the persisted owner and
  corresponding write permissions without incrementing the ownership epoch.
- If synchronization fails or remains indeterminate, the session stays
  read-only and yield is unavailable.
- After synchronization, Ducklion re-evaluates every recovered pending yield
  using the normal atomic yield-safe idle checks.
- Missing or dead PTY processes are reported as terminated sessions and cannot
  regain ownership merely from persisted metadata.

## 16. Supervisor upgrade through agent resume

Status: Decided

Ducklion may replace a PTY supervisor at an idle boundary by resuming the
logical agent conversation in a new native PTY.

### Requirements

- The agent adapter must explicitly support reliable session resume and expose
  a validated conversation or session identifier.
- Ducklion must satisfy the full yield-safe idle check before replacement.
- Ducklion persists the resume identifier before stopping the old supervisor.
- The old PTY and supervisor are stopped, then a new supervisor and native PTY
  are started with the updated binary and agent resume operation.
- The stable Duckway task-channel handle and ownership state are preserved.
- Supervisor replacement does not increment the ownership epoch.
- Each replacement increments a separate `runtime_generation`; callbacks,
  input, and adapter events from an older runtime generation are rejected.
- Conversation context is expected to survive through the agent's resume
  mechanism. PTY screen contents and scrollback are not guaranteed to survive.
- Ducklord displays a lifecycle notice indicating that the agent resumed after
  a supervisor upgrade.
- A running task is never interrupted solely to upgrade its supervisor.
- An adapter without reliable resume support cannot use this upgrade path.
  Its existing supervisor remains pinned until the session is explicitly
  terminated or restarted by the user.

## 17. Restart drain semantics

Status: Decided

- `duckway restart` requests a graceful restart and waits for every affected
  managed-agent PTY to satisfy the yield-safe idle conditions.
- While graceful restart is waiting, affected sessions enter drain mode and
  reject new agent tasks or turns.
- Tasks already running continue to their normal terminal state and complete
  their reply routing before restart proceeds.
- After all affected sessions are idle, Ducklion replaces eligible supervisors
  through the adapter resume procedure and restarts the requested Duckway
  services.
- `duckway restart -f` or `duckway restart --force` skips the graceful wait.
- Forced restart cancels active tasks, fences their late events by incrementing
  `runtime_generation`, and immediately replaces or terminates affected
  supervisors.
- Forced cancellation and any incomplete reply delivery are reported to the
  original task owner.
- Force does not bypass adapter eligibility: a session without reliable resume
  support may lose its live agent process and must be reported as requiring a
  manual restart.

## 18. Restart wait duration and cancellation

Status: Decided

- A normal `duckway restart` has no automatic wait timeout.
- While waiting, the command continuously reports the number and identity of
  active tasks that prevent restart.
- Interrupting the command, including with `Ctrl-C`, cancels the restart
  request and removes its drain barrier.
- Cancelling a pending restart does not cancel, stop, or otherwise modify any
  active agent task or PTY session.
- After restart cancellation, the existing owners may submit new work again.
- Operators who do not want to wait must explicitly invoke `--force`/`-f`.

## 19. Discord authorization source

Status: Decided

Discord channel permissions are the authority for human access to CC input and
commands.

- A human who can send a message in the bound Discord task channel may submit
  task-channel commands, including `!yield` and `!yield -w`.
- A human who can send a message in the management channel may use its
  management commands.
- Duckway continues to validate transport identity, guild and channel mapping,
  channel state, bot authorship, and event integrity before processing input.
- Duckway does not duplicate Discord membership policy with application-level
  user or role allowlists.
- The existing `allowed_user_ids` and `allowed_role_ids` CC configuration fields
  are deprecated and will be removed.
- Existing values in those fields must not remain a hidden authorization layer
  after migration to this model.
- Discord category and channel permission changes take effect according to
  Discord's own authorization behavior without requiring ACL synchronization
  into Duckway.

## 20. Mention policy

Status: Decided

- `require_mention` remains an optional CC activation policy and is not treated
  as authorization.
- Its default is `false` when omitted, preserving the current behavior where
  ordinary task-channel messages may start work without mentioning the bot.
- When enabled, ordinary agent input requires an actual Discord mention of the
  configured bot; matching text alone is insufficient.
- Explicit control-plane commands such as `!yield`, `!yield -w`, status, and
  cancellation do not require a bot mention.
- Discord channel permissions remain authoritative regardless of the mention
  setting.

## 21. Unified Ducklion yield operation

Status: Decided

Discord commands and Ducklord shortcuts are frontend translations of one
Ducklion control-plane operation; they do not implement separate handoff
semantics.

Conceptually, the operation is:

```text
yield(session_id, requester, wait)
```

where `requester` contains a controller kind and stable owner name.

- Discord `!yield` calls Ducklion with the bound CC identity as requester and
  `wait=false`.
- Discord `!yield -w`/`!yield --wait` uses the same requester and `wait=true`.
- A Ducklord yield shortcut calls Ducklion with `kind=terminal`, its supplied
  Ducklord owner name, and the selected PTY session.
- Ducklord's waiting variant sends the same operation with `wait=true`.
- The operation always means `transfer ownership to the requester`; callers do
  not specify the current owner or a separate direction.
- Ducklion alone performs adapter-health checks, drain handling, idle
  validation, pending-request management, epoch fencing, persistence, and the
  atomic ownership transition.
- A request from the current owner is idempotent and reports the existing
  ownership without changing the epoch.

## 22. Stable session identity

Status: Decided

- Every Ducklion PTY session has a persistent six-character random `session_id`
  that is unique within that Ducklion instance. Ducklion retries generation on
  collision before exposing the session.
- Ducklion control-plane operations, including yield, address the session by
  this ID together with the local Ducklion instance context.
- A CC task-channel handle is non-unique display metadata, not a lookup alias or
  PTY identity.
- Duckway resolves a bound Discord channel directly to its stored `session_id`
  before calling the unified Ducklion operation.
- Ducklord lists the human-readable session name, CC handle when present,
  ownership, and adapter metadata, but sends the instance UUID and session ID in
  control requests.
- Discord channel rename, terminal reconnect, agent resume, and supervisor
  replacement preserve `session_id`.
- Supervisor replacement changes `runtime_generation`, not `session_id` or the
  ownership epoch.

## 23. Immutable agent type

Status: Decided

- `agent_type` is required when a Ducklion session is created and is persisted
  in the authoritative session registry.
- The session record also exposes adapter type, adapter version, adapter health,
  adapter task state, and capabilities.
- Yield and other existing-session operations accept only `session_id`; callers
  must not supply or override `agent_type`.
- Ducklion selects and validates the adapter using the persisted agent type.
- Agent type is immutable for the lifetime of a session.
- Changing agent type requires terminating the old session and creating a new
  session with a new six-character session ID.
- Replacing a supervisor or resuming an agent does not change agent type.

## 24. Initial ownership and session creation

Status: Decided

- Duckway creates one managed Ducklion session for each eligible Discord task
  channel.
- A CC-created session starts with owner kind `cc`, stores its stable CC channel
  handle, and requires a supported healthy agent adapter.
- The management channel does not create, own, or bind to a PTY session.
- A Ducklord-created agent session starts with owner kind `terminal` and the
  creating Ducklord-provided owner name; it may later use the defined Discord
  binding workflow without changing ownership.
- A Ducklord-created shell session uses shared Ducklord writer access rather
  than a persisted exclusive owner.
- A shell session has no CC handle, cannot accept CC tasks or Discord binding,
  and cannot use yield.
- Initial ownership creation is persisted before any controller receives write
  permission.

## 25. Eager PTY startup for CC sessions

Status: Decided

- CC-managed task sessions are created explicitly through the management
  channel's `!new` workflow.
- Duckway does not create a dormant CC session record for later lazy startup.
- A successful `!new` creates the Discord task channel, persistent Ducklion
  session record, PTY supervisor, native PTY, agent process, adapter binding,
  and initial CC ownership as one provisioning workflow.
- The agent PTY is running and adapter health is known before `!new` reports the
  session ready for use.
- Ducklord may list or attach read-only to the session immediately after
  provisioning succeeds.

## 26. Owner-authorized management operations

Status: Decided

- Every Ducklion operation that can mutate a PTY session carries the caller's
  logical `owner_id`.
- Management-channel commands resolve the target CC session and send its CC
  owner identity with the Ducklion request.
- Ducklord operations send the Ducklord-provided terminal owner name.
- Ducklion compares the request owner identity with the persisted current
  writer identity before applying the operation.
- A mismatch fails without changing the PTY, task, session metadata, or Discord
  channel.
- Therefore, management-channel operations cannot manage a terminal-owned
  session until CC successfully requests ownership through yield.
- Session close, restart, input, cancellation, and configuration mutations use
  this common owner check rather than command-specific terminal-owned rules.
- Yield is the ownership-transfer operation and is intentionally evaluated
  under its separate requester and idle-state rules.

## 27. Canonical Ducklion session API and authorization

Status: Decided

Ducklion exposes one canonical session API. Discord commands, Ducklord actions,
MCP tools, and local Duckway commands translate into these operations:

```text
CreateSession
ListSessions
GetSession
Attach
Detach
SubmitTask
SendInput
Resize
Yield
CancelTask
RestartSession
CloseSession
DestroySession
UpdateSession
ReadOutput
```

### Read-only operations

- `ListSessions`, `GetSession`, `Attach` in read-only mode, and `ReadOutput` do
  not require the caller to match the current owner.
- An agent-session writable attach and resize require the persisted terminal
  owner name to match the Ducklord-provided owner name. Shell writable attach
  and resize use the shared-writer rules.

### Mutating operations

- `Yield` is the only mutation that a non-owner may request. Ducklion evaluates
  it using the unified yield, adapter, idle, and pending-request rules.
- Every other agent-session mutating operation requires an `owner_id` matching
  the persisted current writer identity. Shell input and lifecycle operations
  use the authenticated shared-user rules.
- Mutations that replace, reset, close, destroy, or reconfigure an agent also
  require the session to be yield-safe idle unless their separately defined
  force mode is used.
- Agent-originated Discord post, edit, delete, file, and approval operations
  must carry the active task's session ID, ownership epoch, and runtime
  generation rather than relying only on a channel handle.
- System recovery and supervisor upgrade operations preserve ownership and use
  runtime-generation fencing.

## 28. Pending-yield exclusivity

Status: Decided

- A session may contain only one pending yield.
- While it exists, Ducklion rejects every new immediate or waiting yield request,
  regardless of whether it comes from the same requester or another requester.
- Ducklion exposes no `CancelYield` operation and no controller can replace,
  shorten, or remove the pending request.
- The request remains pending until the current task completes, times out, or is
  terminated through restart and the atomic yield-safe checks then succeed.
- Destroying the session removes its pending yield as part of session deletion;
  an invalid ownership epoch or unrecoverable session state fails closed and
  requires operator lifecycle recovery rather than transferring blindly.

## 29. Session management command locations

Status: Decided

- A Discord task channel may issue management commands for its own bound
  Ducklion session without specifying a session ID or handle.
- The management channel may issue the same operations with an explicit session
  selector for centralized administration and recovery.
- Duckway resolves both forms to the same canonical Ducklion operation with the
  same CC `owner_id` and authorization checks.
- Terminal-owned sessions reject CC management mutations until CC successfully
  obtains ownership through yield.
- Read-only commands such as status and log remain available without ownership
  transfer.
- The management-channel form remains available when the original task channel
  is archived, deleted, or otherwise unavailable, provided the Ducklion session
  still exists.
- Discord channel permissions determine who may invoke either form.

## 30. Destroy confirmation

Status: Decided

- `DestroySession` and its Discord `!destroy` translation do not require a
  second confirmation token or confirmation command.
- The operation executes immediately after Ducklion validates the caller's
  owner identity and all required session-state preconditions.
- Absence of confirmation does not bypass ownership, epoch, adapter-health,
  idle-state, or force-mode rules.
- Success removes the Ducklion session and performs the configured Discord
  task-channel deletion workflow.
- Failure before the destructive transition leaves both the session and channel
  available and reports the reason to the caller.

## 31. Destroy behavior while busy

Status: Decided

- `DestroySession`/`!destroy` without a mode requires yield-safe idle and fails
  immediately when a task or reply delivery is active.
- `!destroy -w`/`!destroy --wait` establishes a drain barrier, waits for the
  current task and reply routing to finish, then revalidates ownership and
  destroys the session.
- `!destroy -f`/`!destroy --force` cancels the active task, fences late events,
  and proceeds with destruction without waiting for normal completion.
- All three modes require the caller's `owner_id` to match the current owner.
- A waiting destroy is bound to its original owner and ownership epoch and is
  invalidated if either changes.

## 32. End versus destroy

Status: Decided

### End

- `EndSession`/`!end` stops the agent process, PTY, and supervisor after the
  applicable owner and idle checks.
- The Discord task channel is archived rather than deleted.
- Ducklion retains the terminated session record, CC binding metadata,
  ownership history, adapter metadata, and agent conversation resume ID.
- An ended session is not writable or yieldable unless a separate restore
  operation is defined and succeeds.

### Destroy

- `DestroySession`/`!destroy` stops the agent process, PTY, and supervisor.
- The Discord task channel is permanently deleted.
- Ducklion removes the active session record, CC binding, adapter state, and
  stored agent conversation resume ID.
- Destroy does not erase immutable security and ownership audit records.

Both operations must report partial Discord or local cleanup failures and use
idempotent cleanup so retries cannot affect a different session.

## 33. No Duckway reset operation

Status: Decided

- The existing Discord `!reset` command is removed.
- Ducklion does not expose a generic `ResetSession` operation.
- Conversation reset or clearing is performed by the current owner through the
  native agent PTY using that agent's own supported command.
- Duckway and Ducklion do not translate, emulate, or normalize agent-specific
  reset commands.
- Normal ownership checks still apply to the input: CC may send the command
  only while CC-owned, and a terminal may send it only while its Ducklord owner
  name owns the session.
- Agent adapters observe the resulting native lifecycle and update conversation
  resume metadata when the agent reports that it changed.

## 34. RestartSession purpose

Status: Decided

- Ducklion retains `RestartSession` as a runtime-management operation.
- It replaces the PTY supervisor, native PTY, and agent process, then uses the
  adapter's resume capability to restore the existing agent conversation.
- It is intended for a wedged PTY, unhealthy adapter, crashed agent runtime, or
  supervisor upgrade.
- It does not clear or reset agent conversation state; users invoke the native
  agent command when they want that behavior.
- Restart preserves the stable Ducklion session ID, CC binding, current owner,
  and ownership epoch while incrementing `runtime_generation`.
- An adapter without reliable resume support cannot promise conversation
  continuity and must report that limitation before restart proceeds.

## 35. RestartSession modes

Status: Decided

- `RestartSession` without a mode succeeds only when the session is yield-safe
  idle and otherwise fails immediately.
- `RestartSession --wait`/`-w` establishes a drain barrier, waits without a
  default timeout, then revalidates owner and session state before restarting.
- `RestartSession --force`/`-f` cancels an active task, fences late events, and
  restarts immediately.
- Every mode requires the caller's `owner_id` to match the current owner.
- Interrupting a waiting restart cancels its drain request without cancelling
  the active task.
- Successful restart preserves session identity, CC binding, owner, and
  ownership epoch and increments `runtime_generation`.

## 36. EndSession modes

Status: Decided

- `EndSession`/`!end` without a mode succeeds only when the session is
  yield-safe idle and otherwise fails immediately.
- `!end -w`/`!end --wait` establishes a drain barrier, waits without a default
  timeout, then revalidates owner and state before ending and archiving.
- `!end -f`/`!end --force` cancels an active task, fences late events, and ends
  and archives the session immediately.
- Every mode requires the caller's `owner_id` to match the current owner.
- Interrupting a waiting end cancels the drain request without cancelling the
  active task.
- Successful end follows the retained-metadata and Discord archive semantics
  defined for `EndSession`.

## 37. Task cancellation and terminal Ctrl-C

Status: Decided

- A writable terminal attachment may send `Ctrl-C` as the native PTY byte
  sequence after the normal owner check.
- Ducklion forwards the control byte to the PTY and does not reinterpret it as
  a control-plane command.
- Receiving `Ctrl-C` is not proof of cancellation: the application may handle,
  ignore, or reinterpret it.
- Ducklion keeps the task active until the agent adapter reports a reliable
  `cancelled`, `failed`, or completed terminal state and reply routing settles.
- `CancelTask` remains available for non-interactive controllers such as
  Discord `!cancel`.
- The agent adapter implements `CancelTask` using the agent's supported cancel
  mechanism, which may itself send `Ctrl-C` to the PTY.
- `CancelTask` requires the caller's `owner_id` to match the current owner.
- A pending yield is re-evaluated only after adapter-confirmed cancellation and
  all other yield-safe idle conditions are satisfied.

## 38. Ducklord-to-Ducklion transport

Status: Decided

- Ducklord connects by running a non-interactive SSH remote command equivalent
  to `duckway ducklion bridge --stdio`.
- The bridge uses SSH stdin/stdout as a bidirectional transport and connects
  locally to the Ducklion daemon through a Unix domain socket.
- Ducklion does not expose a TCP listener for this protocol.
- The Unix socket is scoped to the local user with mode `0600`; Ducklion checks
  the bridge process UID using peer credentials where supported.
- The authenticated bridge passes the Ducklord-provided owner name.
- One SSH connection multiplexes session-list subscriptions, multiple PTY
  attachments, raw input/output, resize, yield, task state, ownership, and
  lifecycle events through independent stream IDs.
- Records are length-prefixed frames with a structured JSON header and optional
  binary payload. Raw PTY bytes remain binary and need not be valid UTF-8.
- Mutation frames carry session ID, ownership epoch, and runtime generation as
  applicable; Ducklion validates them before changing state or writing input.
- SSH disconnect closes attachments but does not change persisted ownership or
  terminate PTY sessions.
- After reconnect, Ducklord supplies the same owner name, refreshes the session
  list, and reattaches by stable session ID.

## 39. PTY output replay

Status: Decided

- Each PTY supervisor keeps a bounded ring buffer containing the most recent
  4 MiB of raw PTY output for its session.
- Output bytes have a monotonically increasing session-runtime offset.
- Live output frames include the offset needed to detect gaps and resume.
- Ducklord records the last contiguous offset received for each attachment and
  supplies it when reconnecting.
- If the requested offset remains in the ring, Ducklion replays the missing
  bytes and then continues with live output without duplication.
- If the offset is older than the retained ring, Ducklion reports the gap,
  sends a current terminal-screen snapshot, and continues from a new offset.
- The ring limit is per session; a slow reader must not prevent eviction or
  block PTY output collection.
- Full historical scrollback is not guaranteed by the PTY management protocol.
- Replacing the PTY increments `runtime_generation`; offsets are interpreted
  only within the generation that produced them.

## 40. PTY output ownership and persistence

Status: Decided

- The running agent/application process is the source of truth for its own
  conversation and terminal content.
- Ducklion's 4 MiB output ring and terminal framebuffer are transient transport
  and reconnect caches only; they are not a durable transcript.
- Raw PTY output is not written to Ducklion or Duckway persistent storage.
- Ducklion daemon restart preserves the transient cache only because the
  independent PTY supervisor remains alive.
- Supervisor restart, replacement, or upgrade may discard the ring and
  framebuffer; Ducklord then starts from the new runtime's screen snapshot.
- Agent conversation continuity relies on the adapter's native resume
  capability, not on replaying captured PTY bytes.
- Persistent audit records contain lifecycle, task-state, ownership, yield,
  error category, and version metadata only. They must not contain prompts,
  terminal bytes, command output, environment values, or agent responses.

## 41. Audit retention

Status: Decided

- Ducklion session audit events are retained for seven days by default.
- Retention duration is configurable through Duckway configuration and is
  propagated to the independently running Ducklion daemon.
- Ducklion periodically removes audit events and completed mutation-idempotency
  records older than the configured retention window.
- Retention cleanup must not delete or alter current session records, ownership
  state, pending or in-progress operations, adapter state, or supervisor
  recovery metadata.
- Changing retention applies to subsequent cleanup runs and does not require
  PTY supervisors or agent processes to restart.
- Raw PTY content remains excluded regardless of the configured retention.

## 42. Ducklion state database

Status: Decided

- Ducklion stores its authoritative local state in a dedicated SQLite database
  under the Duckway configuration/data directory, with a default location
  equivalent to `~/.duckway/ducklion/ducklion.db`.
- The SQLite driver is pure Go and compiled into the Duckway/Ducklion binary.
- Users do not install a SQLite CLI, shared library, CGO runtime, or separate
  database service.
- Ducklion creates the state directory with mode `0700` and the database file
  with mode `0600`.
- Ducklion automatically applies its own schema migrations before accepting
  bridge or controller connections.
- The database contains session registry, ownership and epochs, runtime
  generations, pending operations, supervisor recovery metadata, and bounded
  audit events.
- Raw PTY output, prompts, responses, and environment values are not stored in
  this database.
- Ducklion state is separate from the Duckway server database so local PTY
  management remains available across server and client-service restarts.

## 43. Shared Duckway configuration

Status: Decided

- Ducklion reads configuration from the same Duckway configuration source used
  by the local Duckway client services.
- Ducklion-specific settings live under a `ducklion` namespace; no separate
  user-managed Ducklion configuration file is introduced.
- Initial settings include audit retention, Unix socket path override, per-
  session output-ring size, and default CC PTY rows and columns.
- Empty path values select secure platform defaults rather than disabling the
  corresponding feature.
- `duckway config` validates Ducklion settings before writing them and reports
  that Ducklion restart is required.
- PTY supervisors receive only the validated settings relevant to their own
  session and do not read or rewrite the shared config directly.

## 44. Configuration activation

Status: Decided

- Ducklion configuration is loaded at daemon startup.
- No Ducklion configuration field supports hot reload in the initial design.
- Every Ducklion configuration change is marked as pending until Ducklion is
  restarted.
- Configuration activation uses the normal drain-aware restart behavior:
  existing tasks finish before restart unless the operator explicitly uses
  force mode.
- The running daemon and `duckway status` expose both the active configuration
  version and whether a newer saved configuration requires restart.
- PTY supervisors receive the newly validated configuration when they are
  created, resumed, or replaced after restart.

## 45. No automatic restart after config changes

Status: Decided

- Saving Ducklion-related configuration never automatically restarts Ducklion,
  PTY supervisors, agents, or other Duckway services.
- The config command reports that the values were saved but are not active and
  that a Ducklion restart is required.
- `duckway status` continues displaying a restart-required indicator until the
  active daemon confirms it loaded the saved configuration version.
- The operator explicitly invokes `duckway restart` or the scoped Ducklion
  restart command when ready.
- Explicit restart then follows the previously defined infinite-wait drain
  behavior unless `--force` is supplied.

## 46. Protocol version negotiation

Status: Decided

- Ducklord, the SSH bridge, Ducklion daemon, and PTY supervisors exchange
  protocol major/minor versions and capability sets during handshake.
- A major-version mismatch is incompatible and the connection or supervisor
  adoption is rejected fail-closed.
- Peers with the same major version negotiate the intersection of their minor-
  version capabilities.
- Unsupported optional features are disabled explicitly rather than guessed or
  silently emulated.
- A supervisor that lacks reliable ownership, adapter-state, epoch, or yield
  capabilities may be exposed read-only but cannot accept managed input or
  ownership transfer.
- Ducklord shows the peer versions, negotiated capabilities, and a clear reason
  when attach or yield is unavailable.
- New daemons retain only the explicitly tested backward-compatible protocol
  range; compatibility is not inferred solely from binary version strings.

## 47. Durable completion of business rejections

Status: Decided

- Ducklion returns ownership, drain, and other expected business rejections as
  structured terminal outcomes rather than transport errors.
- Duckway records that the inbox event was submitted and rejected so retries
  cannot submit the same event to the PTY or agent again.
- Duckway posts the human-readable rejection reason to the originating Discord
  channel before completing the durable inbox record.
- The inbox record transitions to `completed` only after the Discord rejection
  reply succeeds.
- If Discord delivery fails, Duckway retries only the reply-delivery step; it
  does not resubmit the task to Ducklion.
- Rejection replies use the inbox event key as an idempotency key so a retry
  cannot intentionally create duplicate responses.
- A terminal Discord delivery failure follows the configured delivery retry
  policy but is not converted into an agent dead-letter or delayed agent task.

## 48. Offline handling for commands and prompts

Status: Decided

Duckway distinguishes control commands from agent prompts before choosing a
delivery path.

### Control commands

- Session and Ducklion control commands are not queued for later execution when
  Ducklion is offline or unreachable.
- The originating surface immediately reports that the command failed because
  Ducklion is unavailable.
- This includes immediate and waiting yield requests; a pending yield exists
  only after Ducklion has accepted it.
- A failed offline command must not execute automatically after reconnect.

### Agent prompts

- Discord agent prompts continue using the durable inbox independently of
  Ducklion connectivity.
- A prompt remains eligible for delivery while CC owns the session, including
  during temporary client or Ducklion disconnection.
- The durable inbox still sends an admitted prompt to Ducklion after reconnect,
  even if ownership changed before delivery.
- Each prompt identifies the CC requester. Ducklion compares that requester to
  the current writer at actual submission time.
- If ownership has transferred to terminal, Ducklion rejects the prompt before
  PTY input. Duckway follows the structured business-rejection and durable-
  completion flow instead of executing it later.

## 49. Discord command and prompt classification

Status: Decided

- A recognized Duckway `!` control command is handled as an immediate control-
  plane request and is not admitted as an agent prompt.
- Agent-native slash input is escaped in Discord as `!/…`; Duckway removes only
  the leading `!` and admits the resulting `/…` input through the durable prompt
  path.
- An ordinary task-channel message is a durable agent prompt.
- An unknown `!…` command receives an immediate unknown-command response and is
  neither sent to Ducklion nor admitted as an agent prompt.
- Classification occurs before durable admission and uses one versioned command
  registry shared by the Discord gateway and Duckway client.
- The `!!…` direct-shell escape follows the separate management-channel
  subprocess specification below.

## 50. Management-channel direct shell commands

Status: Decided

- The `!!shell-command` feature is retained but accepted only in a CC management
  channel.
- Duckway translates it to a Ducklion non-PTY subprocess operation.
- Ducklion starts and supervises a direct shell subprocess without attaching it
  to any managed PTY session.
- The command does not write to a PTY, does not use a PTY adapter, and does not
  change PTY ownership, ownership epoch, task state, or yield eligibility.
- Task channels reject `!!…`; it is not treated as agent input.
- Because the management channel cannot be terminal-owned, the subprocess does
  not participate in CC/terminal ownership transfer.
- Ducklion still tracks subprocess identity, start time, exit state, cancellation,
  and bounded output delivery as a separate job type.
- Ducklion unavailability causes the command to fail immediately rather than
  queue for later execution.

## 51. Direct-shell working directory

Status: Decided

- `!!` accepts `-p` and `--project` as equivalent project selectors.
- A project selector accepts either the configured project name or its numeric
  index in the same project registry shown by Duckway project-list commands.
- `!!` accepts `-c` and `--cwd` as equivalent explicit working-directory
  selectors.
- Project and cwd selectors are mutually exclusive; specifying both is an
  immediate validation failure.
- Selector parsing ends at `--`, after which all tokens belong to the shell
  command.
- If neither selector is supplied, Ducklion uses the fixed directory
  `~/.duckway/ducklion`.
- Ducklion resolves and validates the working directory before starting the
  subprocess and never falls back to the daemon process's current directory.
- A missing project, invalid numeric index, or unavailable cwd fails without
  starting a subprocess.

## 52. Direct-shell output delivery

Status: Decided

- Each `!!` subprocess uses one Discord progress message rather than posting a
  message for every output event.
- Ducklion captures stdout and stderr into one ordered in-memory stream.
- Per-job captured output is capped at 1 MiB; Ducklion retains the newest bytes
  needed for the final tail when the cap is exceeded.
- On completion, Duckway edits the progress message to include the exit code,
  elapsed time, and at most the final 1,500 characters of combined output.
- Truncated output is clearly marked.
- Duckway does not automatically upload complete output as a Discord attachment.
- Raw subprocess output is not written to the Ducklion audit database or other
  persistent transcript storage.
- The in-memory output buffer is released after terminal delivery success or
  terminal delivery failure handling completes.
- Discord delivery retries must not execute the shell command again.

## 53. Direct-shell timeout and restart termination

Status: Decided

- A `!!` subprocess has a default maximum runtime of 30 minutes.
- The maximum runtime is configurable through the shared Duckway configuration
  and takes effect after the required Ducklion restart.
- On timeout, Ducklion sends `SIGTERM` to the entire
  subprocess process group, waits five seconds, then sends `SIGKILL` if any
  process remains.
- Ducklion assigns every job a stable job ID for status and result correlation.
- The first version does not expose a command or API for cancelling an
  individual direct-shell job.
- A normal Duckway/Ducklion restart drains and waits for active direct-shell
  jobs as well as managed PTY tasks.
- A forced restart cancels active direct-shell jobs using the same process-group
  termination sequence before proceeding.
- Timeout and forced-restart termination results are delivered through the existing single
  Discord progress message without rerunning the command.

## 54. Direct-shell concurrency

Status: Decided

- Ducklion does not impose a per-CC, per-management-channel, or host-wide
  concurrency limit on accepted `!!` subprocess jobs.
- Ducklion does not queue jobs behind an application-level concurrency gate;
  each valid request starts independently.
- Host operating-system resource limits and process-creation failures remain
  authoritative and are reported as job-start failures.
- Each concurrent job retains its own job ID, timeout, process group, output
  buffer, progress message, and termination state.
- Restart draining waits for all active jobs unless force mode is selected.

## 55. Direct-shell interpreter

Status: Decided

- `!!` jobs execute through the login shell of the operating-system user that
  runs Ducklion.
- Ducklion resolves and validates the shell executable at daemon startup and
  records the active path in daemon status.
- Commands are invoked using that shell's login-command form equivalent to
  `<login-shell> -lc <command>`.
- The shell choice is not taken from each job request and cannot be overridden
  through Discord command text.
- If the user's login shell cannot be determined or its executable is
  unavailable, Ducklion falls back to `/bin/sh` and exposes a warning in status
  and the affected job result.
- The resolved shell remains active until Ducklion restart, consistent with the
  no-hot-reload configuration policy.

## 56. Direct-shell environment

Status: Decided

- `!!` jobs use the ordinary login-shell environment of the operating-system
  user running Ducklion.
- Duckway and Ducklion do not inject agent or CC credentials into these jobs,
  including provider API keys, Duckway client tokens, or per-agent proxy
  secrets.
- Environment variables configured by the user's own login-shell startup files
  remain available; managing and protecting those values is the user's
  responsibility.
- The environment policy cannot be overridden through Discord command text.

## 57. Direct-shell standard input

Status: Decided

- `!!` jobs are always non-interactive.
- Ducklion closes the subprocess standard input immediately after launch.
- Discord cannot supply responses to password prompts, confirmation prompts,
  interactive menus, or other subprocess input requests.
- Commands requiring interaction must run in a managed PTY session instead.
- A command that waits despite closed standard input remains subject to its
  normal timeout or forced Ducklion restart.

## 58. Direct-shell audit data

Status: Decided

- Ducklion does not persist the `!!` command text in its audit log.
- Ducklion does not persist the command's standard output or standard error.
- The audit log records only the job ID, requester identity, management-channel
  identity, selected project or working-directory identity, timestamps, exit
  code, and completion, timeout, forced-termination, or start-failure status.
- The Discord message containing the original request remains governed by
  Discord's own retention and access controls; Ducklion does not copy it into
  its database.

## 59. Direct-shell working directory

Status: Decided

- `!! -c/--cwd` accepts either an absolute path or a path relative to
  `~/.duckway/ducklion`.
- Ducklion resolves the path before launching the job and requires it to exist
  and be a directory.
- Directory traversal and filesystem access are governed by the operating-system
  permissions of the user running Ducklion.
- Ducklion does not restrict the resolved directory to configured project roots.
- An invalid, missing, inaccessible, or non-directory path fails before the
  subprocess starts and is reported as a job-start failure.

## 60. Direct-shell project index

Status: Decided

- A numeric value passed to `!! -p/--project` is the display index from the
  current project list, not a persistent project identifier.
- Ducklion resolves the index immediately when accepting the request and binds
  the job to the resolved stable project ID and working directory.
- Later project-list reordering does not change an already accepted job.
- The initial progress response displays the resolved project name and working
  directory so the requester can detect an unintended selection.
- A missing, stale, ambiguous, or out-of-range index fails before subprocess
  launch; Ducklion never guesses the intended project.

## 61. Direct-shell attachments

Status: Decided

- `!!` accepts text commands only and does not accept Discord attachments as
  subprocess input or files.
- Duckway does not download, stage, extract, reference, or delete attachments
  for direct-shell jobs.
- An `!!` request containing one or more attachments is rejected before job
  creation, even when its message also contains a valid text command.
- The rejection explains that files must already exist on the host or be
  handled through a managed PTY workflow.

## 62. Direct-shell command text

Status: Decided

- `!!` accepts a command spanning multiple Discord message lines.
- After parsing Duckway-owned options, the remaining text is passed unchanged
  as one command string to the configured login shell.
- Duckway does not parse, strip, or unwrap Markdown code fences, backticks, or
  other Discord formatting characters.
- Duckway does not normalize line endings, expand variables, or otherwise
  rewrite shell syntax before launch, except for the minimum transport decoding
  required to recover the original message text.
- An empty command after option parsing is rejected before job creation.

## 63. Direct-shell option delimiter

Status: Decided

- The canonical syntax is `!! [-p PROJECT | -c DIR] -- COMMAND`.
- When `-p/--project` or `-c/--cwd` is present, the `--` delimiter is required
  between Duckway-owned options and the shell command.
- When no Duckway-owned option is present, all text following `!!` is treated as
  the command and the delimiter may be omitted.
- After the delimiter, option-like tokens such as `-p` and `-c` belong entirely
  to the shell command and are not interpreted by Duckway.
- Missing option values, a missing required delimiter, or an empty command is
  rejected before job creation with a usage hint.

## 64. Direct-shell source-message changes

Status: Decided

- A direct-shell job's command and parameters become immutable when Ducklion
  accepts and creates the job.
- Editing the source Discord message does not modify, restart, or create another
  job.
- Deleting the source Discord message does not cancel or otherwise affect the
  job.
- Deleting or editing the message cannot terminate an active job; timeout or a
  forced Ducklion restart may still terminate it under their normal rules.
- Discord message-update and message-delete events are not admitted as
  direct-shell work items.

## 65. Direct-shell event dispatch

Status: Decided

- Discord ingress dispatches a source message to the direct-shell command
  handler at most once.
- Direct-shell job management does not add a separate message-snowflake
  idempotency key or duplicate-job reconciliation path.
- More than one job created from the same Discord message is an ingress defect,
  not an expected reconnect or job-management condition.

## 66. Direct-shell feature scope

Status: Decided

- `!!` is a lightweight management-channel convenience for short server-status,
  filesystem-inspection, and similar diagnostic commands.
- It is not intended as a general remote shell or a heavy job-execution system.
- The first version does not expose `!!jobs`, `!!status`, per-job cancellation,
  queues, scheduling, interactive input, or persistent command output.
- Each request is observed only through its single Discord progress/result
  message; users needing richer control must use a managed PTY session or SSH.

## 67. Ducklord session visibility

Status: Decided

- Successful SSH login as the operating-system user running Ducklion is the
  host-side authorization boundary for Ducklord access.
- A Ducklord bridge connected as that user may list and inspect every managed
  session owned by that Ducklion instance.
- It may attach as a reader to any listed session without an additional
  per-session ACL.
- Session visibility and read attachment do not grant writer ownership; input
  remains rejected until Ducklord obtains ownership through the normal `yield`
  operation.
- Ducklion does not map Discord channel membership or Discord user identities
  onto Ducklord reader permissions.

## 68. Initial Ducklord attachment

Status: Decided

- A first-time Ducklord attachment receives Ducklion's current terminal-screen
  and available scrollback snapshot, followed by live output.
- Ducklion does not replay all raw PTY bytes from session creation into a newly
  attached terminal.
- This avoids re-executing historical terminal control sequences and matches
  the observable attachment behavior expected from tmux-like sessions.
- The snapshot is bounded by the supervisor's existing in-memory output and
  terminal-state limits; output already evicted from memory is unavailable.
- Offset-based raw replay remains a reconnect optimization only when the client
  supplies a valid offset within the retained ring; otherwise Ducklion sends a
  fresh snapshot.

## 69. PTY scrollback limit

Status: Decided

- Each managed PTY session retains up to 10,000 logical terminal lines of
  in-memory scrollback by default.
- The scrollback limit is configured through the shared Duckway `ducklion:`
  configuration namespace and applies after Ducklion restart; it is not hot
  reloaded.
- When the limit is exceeded, the supervisor evicts the oldest complete logical
  lines first.
- Scrollback storage is separate from the 4 MiB raw PTY output ring: scrollback
  serves terminal snapshots and local navigation, while the raw ring serves
  short reconnect replay by byte offset.
- Scrollback is transient and is not restored after supervisor replacement or
  host restart.

## 70. PTY resize and scrollback reflow

Status: Decided

- A writer-originated PTY width change reflows soft-wrapped terminal content to
  the new width.
- Hard line breaks emitted by the application remain line boundaries and are
  never removed by reflow.
- Reader viewport changes do not resize or reflow the shared PTY state; readers
  continue to crop, pad, and navigate the shared framebuffer locally.
- Reflow updates the current screen and retained scrollback as one terminal
  state and remains bounded by the configured logical-line limit.
- Height-only changes update the shared screen dimensions without changing hard
  line boundaries.

## 71. Initial PTY dimensions

Status: Decided

- A CC-created managed PTY starts at 120 columns by 40 rows when no terminal
  writer has supplied dimensions.
- The default width and height are configurable through the shared Duckway
  `ducklion:` configuration namespace and take effect after Ducklion restart.
- When a Ducklord attachment becomes writer, it immediately submits its current
  terminal dimensions and Ducklion resizes the PTY under the normal writer-only
  resize rule.
- Reader attachments never replace the configured initial dimensions or the
  current writer dimensions.

## 72. PTY dimensions after yield to CC

Status: Decided

- When ownership transfers from a terminal writer to CC, Ducklion resizes the
  PTY to the configured CC default dimensions, initially 120 columns by 40 rows.
- The resize is part of the ownership-transfer commit and occurs before Ducklion
  accepts new CC input.
- Existing soft-wrapped screen and scrollback content is reflowed under the
  normal writer-resize rules; hard line breaks remain unchanged.
- Terminal readers retain local viewport control but cannot preserve the former
  terminal writer's shared PTY dimensions.

## 73. Atomic resize during ownership transfer

Status: Decided

- Ducklion validates the destination writer's PTY dimensions before committing
  an ownership transfer.
- Any required `TIOCSWINSZ` operation is part of the transfer commit and must
  succeed before Ducklion accepts input from the destination writer.
- If resize fails because the PTY, supervisor, or session is no longer usable,
  the yield fails and the persisted writer ownership remains unchanged.
- Ducklion does not retry the resize automatically; the requester may retry
  yield after the underlying session state is healthy.
- Dimension bounds are validated before the system call so invalid client input
  cannot partially change ownership.

## 74. PTY dimension bounds

Status: Decided

- Managed PTY widths must be between 20 and 500 columns, inclusive.
- Managed PTY heights must be between 5 and 200 rows, inclusive.
- Ducklion rejects an out-of-range create or resize request and does not clamp,
  round, or otherwise substitute dimensions.
- A rejected resize leaves both the current PTY dimensions and writer ownership
  unchanged.
- The initial CC dimensions and every Ducklord-reported writer dimension are
  subject to the same bounds.

## 75. Ducklord owner-name connection uniqueness

Status: Decided

- One Ducklion instance accepts at most one live Ducklord bridge connection for
  each Ducklord-provided owner name.
- A concurrent bridge presenting an already-live owner name is rejected before
  it can attach, send input, request resize, or request yield.
- The sole live bridge for the persisted terminal owner is both the input writer
  and PTY-size authority for that owner.
- Ducklion releases the live-name registration when the bridge disconnects; a
  later bridge may reuse the name under the same-name reconnection rules.
- Ducklion does not generate, reserve, or centrally manage owner names; users
  remain responsible for their uniqueness.

## 76. Ducklord default owner name

Status: Decided

- Ducklord supplies its owner name during bridge connection setup.
- `ducklord --name <name>` explicitly selects the owner name.
- When `--name` is omitted, Ducklord uses the hostname of the machine on which
  Ducklord is running.
- The explicit or hostname-derived value must satisfy Ducklion's owner-name
  validation and live-name uniqueness rules.
- Ducklion trusts the supplied value and does not infer, rewrite, suffix, or
  centrally allocate owner names; users remain responsible for avoiding
  collisions.

## 77. Ducklord owner-name configuration precedence

Status: Decided

- Ducklord resolves its owner name in this order: the invocation's `--name`
  value, the local Ducklord configuration's `name` value, then the local
  machine hostname.
- The first present value is authoritative; Ducklord does not merge or suffix
  values from lower-priority sources.
- `--name` overrides configuration only for that Ducklord invocation and does
  not rewrite the local configuration file.
- The resolved value is validated before opening the remote Ducklion bridge.
- Ducklion receives only the resolved name and does not need to know which
  source supplied it.

## 78. Ducklord local configuration path

Status: Decided

- Ducklord reads its local YAML configuration from
  `~/.ducklord/config.yaml`.
- The optional top-level `name` field supplies the configured owner name under
  the established resolution precedence.
- A missing configuration file is not an error; Ducklord falls back to the
  local hostname.
- An unreadable file, invalid YAML, or invalid configured name is reported as a
  startup error rather than silently falling back.
- This file belongs to the Ducklord machine and is not synchronized or managed
  by Ducklion.

## 79. Ducklord owner-name changes

Status: Decided

- Changing Ducklord's resolved owner name does not rewrite ownership stored on
  existing Ducklion sessions.
- A connection using the new name sees a terminal-owned session held by the old
  name as read-only.
- The new name must request normal immediate or waiting `yield`; Ducklion grants
  it only after the adapter and session satisfy the standard safe-transfer
  checks.
- Ducklion does not infer that old and new names represent the same user or
  provide a privileged rename shortcut.
- Audit and ownership notifications record the old and new owner names as a
  normal ownership transfer.

## 80. Pending yield after Ducklord disconnect

Status: Decided

- A Ducklord-originated waiting yield belongs to the stable requester owner
  name, not to the transient bridge connection that submitted it.
- Disconnecting that Ducklord does not remove its pending yield.
- When the session becomes safely transferable, Ducklion transfers terminal
  ownership to the stored requester name
  even when no matching Ducklord is currently connected.
- The requester can later reconnect using the same name and regain write access
  without another yield.
- Normal pending-yield exclusivity, audit, and ownership-notification rules
  continue to apply.

## 81. Pending yield across Ducklion restart

Status: Decided

- Ducklion persists each pending waiting yield's session ID, requester identity,
  creation timestamp, source ownership epoch, and drain-barrier state in its
  SQLite database.
- A normal or forced Ducklion restart restores every pending yield.
- Recovery never treats restart or temporary adapter absence as proof that the
  session is idle.
- Ducklion first re-adopts the supervisor, completes adapter synchronization,
  and re-runs all normal safe-transfer checks before committing the yield.
- A recovered request does not transfer until the ownership epoch matches and
  the adapter validates a yield-safe idle state. A destroyed or unrecoverable
  session cannot transfer ownership.

## 82. Yield result routing

Status: Decided

- A yield request's immediate or deferred result is returned only through the
  control surface from which that request originated.
- A Discord-originated request replies in its originating Discord channel.
- A Ducklord-originated request replies over its originating Ducklord bridge
  connection.
- If that originating surface or connection is unavailable when a deferred
  result becomes final, Ducklion drops the reply and does not queue, replay, or
  redirect it on reconnection.
- The outcome remains in the normal audit log even when its requester-facing
  reply is dropped.
- Session ownership-change notifications required elsewhere in this spec are
  separate events and are not redirected yield-result replies.

## 83. Waiting-yield acknowledgements

Status: Decided

- When Ducklion accepts a `yield -w/--wait` request, the originating control
  surface immediately receives an acknowledgement that includes the session,
  requester, and current owner.
- The acknowledgement confirms only that the request is pending; it never
  implies that ownership has already changed.
- When the pending request succeeds or reaches an unrecoverable session failure,
  Ducklion attempts one final reply through the same originating control
  surface.
- If the originating surface is then unavailable, the final reply is dropped
  under the established yield-result routing rule and is not replayed.
- The acknowledgement and final reply do not replace the separately required
  audit entries or ownership-change notification.

## 84. Yield requests while one is pending

Status: Decided

- While a session has a pending yield, Ducklion rejects every additional
  immediate or waiting yield request.
- This includes an identical repeat from the original requester as well as a
  request from another CC or terminal owner.
- Rejection does not change the stored requester, request origin, drain barrier,
  ownership epoch, or current writer.
- The rejection response identifies that the session already has a waiting
  transfer without exposing any new control operation.

## 85. Ducklord owner-name format

Status: Decided

- A Ducklord owner name must contain between 1 and 64 ASCII characters.
- Allowed characters are ASCII letters, digits, `.`, `_`, and `-`.
- Names are case-sensitive and Ducklion compares and persists them exactly as
  supplied; it does not trim, lowercase, or otherwise normalize valid names.
- Whitespace, path separators, control characters, Discord markup, and all
  other characters are rejected during bridge connection setup.
- A rejected name cannot list, attach, request yield, or reserve a live-name
  registration.

## 86. Yield waiting and timeout semantics

Status: Decided

- Only `yield -w/--wait` may create a pending yield.
- Plain `yield` performs one immediate yield-safe check and returns success or
  failure without creating persistent waiting state.
- A pending yield has no timeout or expiry of its own.
- Accepting a waiting yield does not add, shorten, extend, or otherwise modify
  the active task's lifecycle or timeout policy.
- The pending yield remains until the task ends through its existing lifecycle,
  including normal completion, its independently configured task timeout, or
  restart termination, after which Ducklion performs the required atomic
  transfer checks.

## 87. Ducklord input path and read-only behavior

Status: Decided

- Ducklord never writes directly to a remote PTY or supervisor file descriptor.
- In a writable attachment, Ducklord encodes terminal input as bridge protocol
  input frames; Ducklion validates the session, live bridge, supplied owner name,
  persisted writer, ownership epoch, and session state before forwarding bytes
  to the PTY supervisor.
- Ducklion rejects every input frame that does not match the current writer,
  even if a modified or malicious Ducklord bypasses its local UI restrictions.
- In a read-only attachment, Ducklord does not send ordinary input frames.
- Ducklord consumes navigation keys locally for viewport and scrollback actions,
  including arrow and related viewing controls, without affecting the PTY.
- Yield and other permitted Ducklion control operations remain available from
  read-only mode and travel as control frames, not PTY input.

## 88. Ducklord writer view mode

Status: Decided

- In normal writable mode, navigation keys and all other ordinary terminal input
  are sent through Ducklion to the PTY after authorization.
- Ducklord provides an explicit local view mode for navigating the retained
  screen and scrollback without sending those keys to the PTY.
- A prefix-driven local action enters view mode; the exact key binding is
  specified separately.
- While view mode is active, navigation input is consumed locally and Ducklord
  sends no PTY input frames for those actions.
- Leaving view mode returns to the live bottom of the terminal and restores
  normal writable input forwarding when the attachment still owns the session.
- If ownership changes while view mode is active, leaving it returns to
  read-only mode rather than restoring write access locally.

## 89. Ducklord command prefix and default bindings

Status: Decided

- Ducklord uses `Ctrl-b` as its default local command prefix.
- The default attachment bindings are `Ctrl-b [` to enter view mode, `Ctrl-b y`
  to request immediate yield, `Ctrl-b Y` to request waiting yield, and
  `Ctrl-b q` to detach.
- Prefix actions are consumed by Ducklord and are sent as local UI changes or
  Ducklion control frames; their bytes are never forwarded as PTY input.
- The prefix and individual bindings are configurable in Ducklord's local YAML
  configuration and require restarting Ducklord to take effect.
- A binding configuration must reject duplicate or malformed sequences rather
  than selecting one action implicitly.

## 90. Ducklord prefix passthrough

Status: Decided

- With the default binding, `Ctrl-b Ctrl-b` escapes the prefix and sends one
  literal `Ctrl-b` byte to the PTY.
- Ducklord sends only the second `Ctrl-b`; the first is always consumed as the
  local prefix.
- Literal-prefix passthrough is available only in normal writable mode and still
  travels through Ducklion's standard owner and ownership-epoch authorization.
- In read-only or view mode, the same sequence sends no PTY input.
- When the command prefix is customized, pressing the configured prefix twice
  provides the equivalent single-prefix passthrough behavior.

## 91. Unknown Ducklord prefix commands

Status: Decided

- When a command prefix is followed by an unbound key, Ducklord consumes both
  the prefix and that key and sends neither to the PTY.
- Ducklord displays a short local `unknown command` status without creating a
  Ducklion request.
- The status clears automatically and does not modify attachment, ownership,
  view-mode, or PTY state.
- Ducklord does not fall back to forwarding the unknown sequence, including in
  writable mode.

## 92. Ducklord prefix timeout

Status: Decided

- After receiving the command prefix, Ducklord enters a local pending-prefix
  state and displays a short `PREFIX` indicator.
- The default pending-prefix timeout is two seconds and is configurable in the
  local Ducklord YAML configuration.
- If no following key arrives before the timeout, Ducklord clears the prefix
  state and consumes the prefix without sending any byte to the PTY.
- Changing the timeout requires restarting Ducklord; it is not hot reloaded.
- Prefix timeout affects only local input parsing and never changes session,
  ownership, or Ducklion state.

## 93. Ducklord view-mode controls

Status: Decided

- The first view-mode version supports arrow keys and Page Up/Page Down for
  viewport movement.
- Home and End move to the start and end of the current logical line.
- `g` moves to the oldest retained scrollback position and `G` returns to the
  newest live position.
- `q` and Escape leave view mode.
- Search, text selection, copy-mode marks, and mouse-driven selection are outside
  the first-version scope.
- Every view-mode key is processed locally and never sent as PTY input.

## 94. Ducklord mouse-event handling

Status: Decided

- In normal writable mode, Ducklord forwards terminal mouse input through the
  bridge as PTY input; Ducklion applies the same writer and ownership-epoch
  authorization used for keyboard input.
- In read-only and view modes, mouse-wheel events move the local viewport and
  are not forwarded to the PTY.
- Other mouse events in read-only and view modes are ignored.
- The first version does not implement mouse selection or clickable Ducklord
  attachment controls.
- Mouse handling does not bypass the local command-prefix parser or Ducklion's
  authoritative input gate.

## 95. Ducklord paste handling

Status: Decided

- Ducklord accepts pasted terminal input only in normal writable mode.
- Bracketed-paste begin and end sequences and the enclosed bytes are preserved
  and travel through the normal Ducklion-authorized PTY input path.
- In read-only or view mode, Ducklord discards the entire paste and sends no
  content or bracketed-paste control sequence to Ducklion.
- The first version treats paste as ordinary PTY input and adds no special
  large-paste chunking, backpressure, delivery acknowledgement, partial-write
  recovery, or PTY-write timeout mechanism.

## 96. Ducklord session-list synchronization

Status: Decided

- After bridge negotiation and owner-name registration, Ducklord requests and
  receives a complete snapshot of the Ducklion instance's visible sessions.
- Ducklion then pushes incremental session-created, metadata/status-changed,
  ownership-changed, and session-removed events over the bridge.
- Ducklord applies those events to its local session-list model without periodic
  polling.
- A disconnected Ducklord does not receive or persist missed incremental events.
- On every reconnection, Ducklord discards its prior authoritative list, obtains
  a new complete snapshot, and only then applies newly streamed events.
- Snapshot and incremental events carry a Ducklion instance generation so stale
  events from an older connection cannot overwrite the refreshed list.

## 97. Session-list snapshot consistency

Status: Decided

- Ducklion assigns a monotonically increasing list revision to every committed
  session-list-visible change within one Ducklion instance generation.
- A complete snapshot represents one revision and includes that revision in its
  response metadata.
- Ducklion buffers changes committed after the snapshot revision while the
  snapshot is being transmitted, then sends them in revision order after the
  snapshot completes.
- Ducklord applies only events from the negotiated instance generation whose
  revision is greater than its current applied revision.
- A revision gap, duplicate snapshot completion, or generation mismatch causes
  Ducklord to discard the incremental stream and request a fresh snapshot rather
  than guessing missing state.

## 98. Session-list synchronization buffer limit

Status: Decided

- Ducklion buffers at most 1,024 session-list change events per bridge while a
  complete snapshot is in flight.
- The limit is fixed in the first version and is not exposed as configuration.
- If another event would exceed the limit, Ducklion aborts that bridge's current
  synchronization cycle and signals that a new snapshot is required.
- Ducklord discards the incomplete snapshot and all buffered incremental state,
  then requests a fresh snapshot on the same healthy bridge.
- Buffer overflow affects only that Ducklord synchronization stream and does not
  block session mutations or disconnect other bridge clients.

## 99. Ducklord session-list views

Status: Decided

- Ducklord provides a simple session-list view by default and a user-selectable
  detailed view over the same synchronized session model.
- The simple view displays session handle, project, agent type, task status,
  current owner, a waiting-yield indicator, and last-activity time.
- The detailed view additionally displays the six-character session ID, Discord
  channel name and ID, adapter health and state, ownership epoch, runtime
  generation, waiting requester, creation time, and full last-activity time.
- A field that does not apply, such as Discord channel data for a manual PTY, is
  displayed as absent rather than synthesized.
- Switching views is entirely local and does not request a second snapshot or
  mutate Ducklion session state.

## 100. Ducklord session-list detail toggle

Status: Decided

- Pressing `d` while the session list has input focus toggles between the simple
  and detailed views.
- The key is a list-view-local action and does not require the attachment command
  prefix or send a Ducklion control request.
- Ducklord remembers the selected view only for the lifetime of the current
  process.
- The selection is not written to local configuration or restored on the next
  launch; every new Ducklord process starts in the simple view.

## 101. Ducklord manual session ordering

Status: Decided

- Ducklion does not prescribe or transmit a display order for the session list;
  ordering is a Ducklord-local concern.
- Ducklord lets the user move the selected session upward or downward and saves
  the resulting order by instance UUID plus six-character session ID in local
  Ducklord state.
- A newly discovered session whose complete identity is not in the saved order
  is appended to the bottom of the list.
- Incremental status and metadata updates do not move an existing session.
- When Ducklion reports a session removed, Ducklord asks the user whether to
  remove its saved local entry; it does not remove ordering or membership
  without confirmation.
- A later session with a different instance/session identity is always treated
  as new even when its handle, project, channel, or agent type matches a removed
  session.

## 102. Ducklord local state directory

Status: Decided

- Ducklord stores all of its user-local files under `~/.ducklord` and does not
  use XDG configuration or state directories.
- User-managed settings, including owner name and key bindings, are stored in
  `~/.ducklord/config.yaml`.
- Mutable client state, including the saved manual session ID order, is stored
  separately in `~/.ducklord/state.json`.
- Ducklord creates `~/.ducklord` with user-only permissions when needed and
  writes both files so they are readable and writable only by that user.
- Ducklion never reads, writes, or synchronizes the Ducklord-local directory.

## 103. Ducklord manual-order controls

Status: Decided

- In the session-list view, `Ctrl-k` moves the selected session one position
  upward and `Ctrl-j` moves it one position downward.
- Ordinary Up/Down arrow keys move only the selection cursor and do not change
  saved ordering.
- Moving beyond the first or last position is a no-op with a short local status
  indication.
- Each successful move updates `~/.ducklord/state.json` and keeps the moved
  session selected by UUID.
- These are list-view-local actions and do not send control requests to
  Ducklion.

## 104. Ducklion instance identity and local ordering scope

Status: Decided

- Ducklion generates a random UUID instance ID when initializing its database
  for the first time and persists that ID in the database.
- The instance ID remains stable across daemon, supervisor, host, and software
  restarts as long as the Ducklion database is preserved.
- Every bridge handshake and session-list snapshot includes the instance ID.
- Ducklord keys saved manual session order in `~/.ducklord/state.json` by
  Ducklion instance ID, then by six-character session ID.
- SSH hostname, address, port, user, or local host alias changes do not create a
  new ordering scope when the reported instance ID remains the same.
- Recreating or replacing the Ducklion database produces a new instance ID and
  therefore a clean local ordering scope; Ducklord does not guess that it is the
  former instance.

## 105. Ducklord stale instance state retention

Status: Decided

- Ducklord does not automatically delete a Ducklion instance's local state based
  on age, last connection time, unreachable host status, or instance count.
- Saved manual session ordering remains in `~/.ducklord/state.json` indefinitely
  unless its individual sessions are explicitly reported removed by that same
  Ducklion instance or the user removes the state.
- Ducklord does not infer that an unseen instance has been decommissioned.
- An explicit user-facing state-cleanup command may be added later but is outside
  the first-version scope.

## 106. Ducklord local-state durability

Status: Decided

- Ducklord updates `~/.ducklord/state.json` by writing a user-only temporary file
  in the same directory, flushing it, and atomically renaming it over the prior
  state file.
- Ducklord never edits the active JSON file in place.
- On startup, invalid or unreadable JSON is renamed to
  `state.json.corrupt-<timestamp>` when possible, and Ducklord continues with an
  empty local state.
- Ducklord displays a local warning naming the preserved corrupt file but does
  not treat recoverable state corruption as a bridge or Ducklion failure.
- Failure to persist a later state update leaves the in-memory order active for
  the current process and reports that it will not survive restart.

## 107. Single local Ducklord instance

Status: Decided

- One operating-system user is expected to run at most one Ducklord process at
  a time.
- Ducklord obtains an exclusive advisory lock on
  `~/.ducklord/ducklord.lock` during startup and holds it for its entire process
  lifetime.
- If another live process already holds the lock, the new Ducklord exits with a
  clear already-running error before reading mutable state or opening a bridge.
- The operating system releases the advisory lock automatically when Ducklord
  exits or crashes; the lock file itself may remain and is not evidence that a
  process is still running.
- With the process lock held, Ducklord remains the sole writer of
  `~/.ducklord/state.json` and needs no multi-writer merge protocol.

## 108. Multi-host session-list modes

Status: Decided

- One Ducklord process may maintain concurrent SSH bridge connections to
  multiple Ducklion hosts; this is a core use case rather than a later extension.
- Each host connection negotiates, synchronizes, reconnects, and reports health
  independently, so one unavailable host does not block sessions from others.
- Ducklord offers three user-selectable session-list organization modes:
  `custom`, `host`, and `type`.
- `custom` uses user-defined groups and manual session ordering.
- `host` groups sessions by their configured Ducklord host and orders them
  within those host groups.
- `type` groups sessions by session kind. The first-version session kinds are
  `shell` and `agent`; an agent's implementation type remains separate metadata.
- Every session identity used by Ducklord is scoped by Ducklion instance UUID
  plus session ID, so sessions from different hosts cannot collide.

## 109. Custom-group membership

Status: Decided

- In `custom` mode, each session belongs to at most one user-defined group.
- A session without an explicit group belongs to the built-in `Ungrouped`
  section.
- Ducklord lets the user create, rename, and manually order custom groups and
  move sessions between a custom group and `Ungrouped`.
- Group membership, group order, and the shared manual session order are stored
  only in `~/.ducklord/state.json`.
- A session is displayed once in the custom list; first-version custom groups do
  not behave as multi-value tags.
- Removing a custom group moves its remaining sessions to `Ungrouped` rather
  than deleting or changing the remote sessions.

## 110. Ducklord custom-group menu

Status: Decided

- Pressing `g` in the session-list view opens a Ducklord-local group menu.
- The menu supports creating a group, renaming a group, deleting a group, moving
  the selected session to a group or `Ungrouped`, and changing custom-group
  order.
- Opening or dismissing the menu does not modify state; Ducklord persists only
  a confirmed operation.
- Every group operation changes only `~/.ducklord/state.json` and never sends a
  mutation to Ducklion or a Discord channel.
- Deleting a group requires explicit selection of the delete action but no
  second confirmation; its sessions move to `Ungrouped`.

## 111. Custom-group identity and names

Status: Decided

- Ducklord assigns each custom group a random persistent UUID and uses that UUID
  for membership, ordering, rename, and deletion references.
- A display name may contain 1 to 64 Unicode code points, including Chinese
  characters; Ducklord trims leading and trailing whitespace and rejects empty
  names, line breaks, and control characters.
- Display names do not need to be unique and are compared only for presentation,
  never as group identity.
- When two groups with the same display name are visible in one list, Ducklord
  appends a short UUID suffix such as `正式環境 · a13f` to disambiguate them.
- Renaming a group changes only its display name and preserves its UUID, session
  membership, and manual position.

## 112. Cross-host custom groups

Status: Decided

- A custom group may contain sessions from any number of concurrently configured
  Ducklion instances.
- Each membership entry identifies its session by the pair of Ducklion instance
  UUID and session ID, never by hostname, handle, or display name.
- Custom grouping is independent from the `host` organization mode and never
  changes remote session ownership or metadata.

## 113. Offline-host session presentation

Status: Decided

- When a Ducklion host bridge disconnects, Ducklord keeps that instance's last
  known sessions visible in their existing custom groups and manual positions.
- Those sessions are marked `OFFLINE`; Ducklord disables attach, yield, and all
  other remote operations for them while the bridge is unavailable.
- Disconnect does not move sessions to `Ungrouped`, remove saved membership, or
  change manual order.
- After reconnection and a fresh snapshot, matching instance and session IDs
  are updated in place and return to live status.
- Sessions absent from the authoritative post-reconnect snapshot are processed
  under the separately defined remote-removal rule rather than retained merely
  because they existed in the stale view.

## 114. Shared manual order across list modes

Status: Decided

- `custom`, `host`, and `type` modes use one shared manual relative order for
  sessions rather than maintaining three independent orders.
- `host` projects that order into automatically derived host groups, and `type`
  projects it into the `shell` and `agent` groups.
- Within each derived group, sessions retain their relative positions from the
  shared manual order.
- Moving a session with `Ctrl-k` or `Ctrl-j` updates that shared order and is
  therefore visible after switching to another organization mode.
- Changing organization mode never rewrites custom-group membership.

## 115. Per-mode group ordering

Status: Decided

- Ducklord persists a separate manual group order for each organization mode.
- `custom` order contains custom-group UUIDs plus the built-in `Ungrouped`
  section.
- `host` order contains Ducklord host identities, and newly configured or first
  discovered hosts are appended.
- `type` order contains the `shell` and `agent` session-kind groups.
- Reordering a group affects only the active mode's group order and never changes
  the single shared relative order of sessions.
- All per-mode group orders are stored in `~/.ducklord/state.json`.

## 116. Persisted session-list organization mode

Status: Decided

- Ducklord stores the currently selected `custom`, `host`, or `type`
  organization mode in `~/.ducklord/state.json`.
- A later Ducklord launch restores the last successfully persisted mode.
- When no valid saved value exists, Ducklord starts in `custom` mode.
- Changing mode is a local UI operation and does not contact Ducklion or alter
  session metadata, custom membership, or saved ordering.
- An unknown mode value from a newer or corrupt state file produces a warning
  and falls back to `custom` without discarding the remainder of valid state.

## 117. Session-list organization shortcut

Status: Decided

- Pressing `o` in the session-list view cycles the organization mode in this
  order: `custom`, `host`, `type`, then back to `custom`.
- Ducklord displays the active organization mode in the session-list header.
- Shortcut bindings in this document are agreed first-version defaults and may
  be adjusted together after implementation usability testing without changing
  their underlying operations or ownership semantics.
- Mode switching remains local and persists under the established mode-state
  rules.

## 118. Ducklord multi-host SSH configuration

Status: Decided

- `~/.ducklord/config.yaml` contains an ordered `hosts` list of OpenSSH host
  aliases, for example `prod` and `lab`.
- Ducklord does not duplicate SSH hostname, user, port, identity-file,
  ProxyJump, or equivalent transport settings.
- Those connection details and credentials remain in the user's standard
  `~/.ssh/config` and OpenSSH facilities.
- For each configured alias, Ducklord independently launches the equivalent of
  `ssh <alias> duckway ducklion bridge --stdio` and maintains that bridge
  concurrently with the others.
- A failure for one alias is reported on that host's status and does not stop
  bridge setup or operation for other aliases.

## 119. Duplicate Ducklion host aliases

Status: Decided

- Configuring multiple SSH aliases that resolve to the same Ducklion instance is
  a user configuration error.
- Ducklord does not merge aliases, silently choose one, rewrite configuration,
  or automatically deduplicate connections.
- Ducklion's one-live-bridge-per-owner-name rule rejects the later connection;
  after handshake information is available, Ducklord reports that the alias
  resolves to an already connected instance UUID.
- The failed alias remains visible with its configuration error while other host
  bridges continue normally.
- The user resolves the condition by removing or changing the duplicate alias in
  `~/.ducklord/config.yaml` and restarting Ducklord.

## 120. Ducklord host-configuration reload

Status: Decided

- Ducklord reads the configured `hosts` list only during process startup.
- It does not watch `~/.ducklord/config.yaml`, hot reload host additions or
  removals, or mutate active bridges in response to file changes.
- A changed hosts configuration takes effect only after the user restarts
  Ducklord.
- Ducklord does not restart itself automatically.

## 121. Ducklord per-host reconnect policy

Status: Decided

- Ducklord automatically reconnects a configured host after an unexpected SSH
  bridge exit, transport loss, or liveness failure.
- Each host maintains an independent retry schedule of 1, 2, 5, 10, and 30
  seconds, then continues retrying every 30 seconds.
- Failure and backoff for one host do not delay synchronization, attachment, or
  input on any other host.
- A successful bridge negotiation and complete session-list snapshot reset that
  host's backoff to the initial one-second delay for any later failure.
- Reconnection always performs a new protocol handshake and authoritative
  snapshot before enabling operations for that host.
- Authentication failures that require user interaction follow the separate
  interactive-authentication policy and do not continue background retries.

## 122. Interactive SSH authentication

Status: Decided

- Ducklord may support SSH password, passphrase, keyboard-interactive, and MFA
  authentication, but authentication prompts never share the bridge protocol's
  stdin or stdout stream.
- When authentication is required, Ducklord marks that host as requiring
  authentication and stops automatic background retries for it.
- A user-triggered action temporarily suspends the Ducklord TUI and runs an
  interactive SSH authentication phase on a separate controlling terminal.
- Successful authentication establishes an OpenSSH ControlMaster connection;
  Ducklord then starts the stdio bridge through that authenticated master.
- Interactive authentication requests are serialized, so multiple hosts cannot
  display overlapping terminal prompts.
- Cancellation or authentication failure returns to the TUI with that host
  offline and requires another explicit user action; Ducklord never loops on
  password or MFA prompts in the background.

## 123. Ducklord SSH ControlMaster lifecycle

Status: Decided

- Ducklord stores its private OpenSSH control sockets under
  `~/.ducklord/ssh/`, using a filesystem-safe hash of the configured host alias
  as the socket filename.
- ControlMaster connections created by Ducklord are scoped to the current
  Ducklord process and are not advertised as shared user-wide SSH masters.
- Every bridge for that host uses the Ducklord-owned control socket after
  interactive authentication succeeds.
- On orderly shutdown, Ducklord asks OpenSSH to close each ControlMaster it
  created.
- At startup, Ducklord checks socket entries left in its directory, removes only
  entries that fail an OpenSSH master liveness check, and never deletes an
  unverified live socket.
- Files and directories under `~/.ducklord/ssh/` use user-only permissions.

## 124. Interactive authentication entry point

Status: Decided

- Interactive SSH authentication may be initiated only while the session-list
  or host-status surface has focus.
- Ducklord does not open an authentication terminal while a PTY attachment has
  input focus.
- A user operating an attachment must return focus to the main surface before
  initiating authentication for any host.
- Suspending the TUI for authentication does not detach other host bridges or
  change any PTY ownership; their output resumes through the normal
  snapshot or offset path when the TUI returns.

## 125. Ducklord focus and input routing

Status: Decided

- Ducklord maintains exactly one focused interactive surface at a time.
- The focused surface determines whether ordinary user input controls the
  session list or is handled by the active PTY attachment under its current
  writable, read-only, or view-mode rules.
- Ducklord renders a clearly highlighted border around the focused surface and a
  visually distinct inactive border around other visible surfaces.
- Focus state is local to Ducklord and never changes Ducklion ownership.
- Local global shortcuts are processed before focus-specific routing; all other
  input is delivered only to the focused surface.

## 126. First-version Ducklord pane layout

Status: Decided

- The first Ducklord version uses a fixed two-pane layout: a session list on the
  left and one active PTY pane on the right.
- At most one PTY attachment is rendered as active by a Ducklord process at a
  time.
- Selecting another session replaces the right-hand PTY pane's attachment;
  detaching the prior view does not change that session's persisted ownership or
  stop its process.
- Session-list metadata for all connected hosts continues to synchronize while
  one PTY is active, but non-active sessions do not require a live PTY output
  subscription in the first version.
- Horizontal and vertical PTY pane splitting and multiple simultaneously
  rendered PTY attachments are reserved for a subsequent version.

## 127. Ducklord session-list pane width and visibility

Status: Decided

- The session-list pane is 36 columns wide by default and the active PTY pane
  receives the remaining terminal width.
- The default list width and whether automatic hiding is enabled are configurable
  in `~/.ducklord/config.yaml` and take effect on Ducklord restart.
- The user may resize the list pane locally during execution; Ducklord stores the
  selected width in `~/.ducklord/state.json` for later launches.
- When the outer terminal is too narrow for both panes under the configured
  minimums, the session list becomes an overlay over the left side of the PTY
  rather than shrinking or hiding the PTY.
- Automatic hiding does not overwrite the saved list width, remove list state,
  or change which surface regains focus when the list becomes visible again.
- Horizontal and vertical split-pane support remains outside this first-version
  behavior.

## 128. Ducklord automatic list-hide threshold

Status: Decided

- The session-list pane requires at least 20 content columns when visible.
- The active PTY pane requires at least 40 content columns.
- Ducklord includes the actual rendered borders and separator width when testing
  whether both pane minimums fit in the outer terminal.
- If both minimums cannot fit, Ducklord enters overlay layout rather than
  compressing either pane below its minimum.
- The threshold is derived from pane constraints and current border geometry; it
  is not stored as a separate fixed outer-terminal width.

## 129. Narrow-terminal overlay focus behavior

Status: Decided

- In narrow overlay layout, focusing the session list displays it over the left
  side of the active PTY; the PTY remains rendered and attached behind it.
- When focus moves to the PTY and automatic hiding is enabled, Ducklord hides the
  list overlay and exposes the complete PTY pane.
- Returning focus to the list restores the overlay without detaching or resizing
  the PTY.
- When automatic hiding is disabled, the list overlay remains visible after PTY
  focus and may cover the left part of the rendered PTY.
- Showing or hiding the overlay is local presentation state and never sends a
  PTY resize or Ducklion operation.

## 130. Opening a session from the Ducklord list

Status: Decided

- Pressing Enter on a selected session requests an attachment and prepares that
  session as the right-hand active PTY.
- Ducklord does not replace the existing active PTY until the new attachment and
  initial snapshot succeed.
- After success, Ducklord replaces the right-hand pane, detaches the prior view
  without changing its ownership or process, and moves focus to the new PTY.
- If attachment or snapshot fails, the prior active PTY remains unchanged, the
  session list keeps focus and selection, and Ducklord displays the error.
- Opening an offline or otherwise unavailable session fails locally without
  disturbing the current active PTY.

## 131. Initial active session selection

Status: Decided

- During startup, Ducklord waits until every configured host reaches an initial
  connected-snapshot, offline, or authentication-required result.
- It then selects the first session in the currently persisted organization mode
  and shared manual order.
- If that session is online and attachable, Ducklord automatically attaches it
  as the active PTY using the normal snapshot path.
- If it is offline or unavailable, Ducklord keeps it selected and displays its
  status in the right-hand pane rather than skipping to a later session.
- Ducklord displays an empty-session placeholder only when the combined list has
  no session from any configured host.

## 132. Initial Ducklord focus

Status: Decided

- When startup auto-attachment of the first ordered session succeeds, Ducklord
  places initial input focus on the active PTY pane.
- If the first session is offline, unavailable, or cannot be attached, Ducklord
  places focus on the session list while showing that session's status on the
  right.
- If no session exists, the session list retains focus alongside the empty-session
  placeholder.
- Initial focus is local UI state and never requests or implies PTY writer
  ownership.

## 133. Active session removed remotely

Status: Decided

- When an authoritative Ducklion event or post-reconnect snapshot shows that the
  active session was removed, Ducklord closes its active PTY view and sends no
  further input for that session.
- Ducklord selects the next session at the removed session's list position, or
  the preceding session when no next item exists.
- It does not automatically attach the newly selected session.
- Focus returns to the session list so subsequent typing cannot be delivered to
  a different PTY unintentionally.
- Ducklord displays a local removal notice and prompts separately about cleaning
  that session's saved Ducklord settings.

## 134. Local settings for a remotely missing session

Status: Decided

- When Ducklion reports that a saved session ID does not exist, Ducklord asks
  whether to remove that session's Ducklord-local settings.
- Confirming cleanup removes only its local manual-order, custom-group membership,
  last-known metadata, and detached PTY snapshot file; it never invokes
  `DestroySession`, deletes a remote PTY, or changes Ducklion data.
- Declining cleanup preserves the local entry exactly in its prior group and
  order with its last-known metadata, while keeping remote input and control
  operations disabled until existence is confirmed again.
- A later attach attempt first asks the corresponding Ducklion instance whether
  the session ID currently exists. If it still does not, Ducklord presents the
  cleanup choice again.
- If the session exists again with the same instance and session IDs, Ducklord
  refreshes it in place and allows normal attach; it never substitutes a similar
  handle or newly created UUID.

## 135. Ducklord detached PTY snapshots

Status: Decided

- Before detaching or replacing an active PTY view, Ducklord saves the last
  terminal screen and retained scrollback snapshot it received for that session.
- Each session has a separate snapshot file at
  `~/.ducklord/sessions/<instance-uuid>/<session-id>.snapshot`.
- Snapshot content is terminal state for user recall, not a replayable raw PTY
  byte stream and not part of `~/.ducklord/state.json`.
- Ducklord writes the snapshot through a same-directory temporary file and atomic
  rename so one session cannot partially overwrite another session's snapshot.
- Before a fresh attach snapshot arrives, Ducklord may display the saved content
  in the PTY pane with an unambiguous `STALE` indicator.
- A successful fresh snapshot replaces the displayed stale content and becomes
  the source for the next detach save.
- Snapshot files are local Ducklord data; Ducklion neither reads nor synchronizes
  them.

## 136. Ducklord snapshot file security

Status: Decided

- Ducklord creates `~/.ducklord/sessions` and its instance subdirectories with
  mode `0700`.
- Detached PTY snapshot files and their temporary replacement files use mode
  `0600` regardless of the process's less restrictive default umask.
- Ducklord does not add application-level encryption to snapshot files in the
  first version.
- Snapshot confidentiality therefore relies on the local operating-system user
  boundary and any filesystem or full-disk encryption configured by the user.
- Permission or ownership validation failure prevents Ducklord from reading or
  writing the affected snapshot and produces a local warning without weakening
  permissions automatically.

## 137. Ducklord snapshot retention

Status: Decided

- Detached PTY snapshots have no time-based expiry or automatic age-based
  deletion.
- Each instance/session ID pair retains only its newest snapshot file; a later
  detach atomically replaces the prior snapshot rather than creating history
  versions.
- Host disconnect, Ducklord restart, session offline state, and remote
  session-removal notification do not delete the saved snapshot.
- Ducklord deletes the snapshot only when the user confirms cleanup of that
  session's Ducklord-local settings or removes the file manually.

## 138. Ducklord snapshot size limit

Status: Decided

- Each detached PTY snapshot file has a maximum payload size of 4 MiB, matching
  the configured first-version capacity of the per-session raw PTY output ring.
- The equal limits do not imply equal content: the raw ring contains replayable
  PTY bytes, while the Ducklord snapshot contains parsed terminal screen and
  scrollback state.
- If serialized snapshot state exceeds 4 MiB, Ducklord always preserves the
  current screen, then retains the newest scrollback that fits and evicts the
  oldest complete logical lines.
- A truncated snapshot records and displays a `TRUNCATED` marker when used as a
  stale pre-attach view.
- Ducklord never splits one session snapshot across multiple history files.

## 139. Ducklord snapshot file format

Status: Decided

- A detached snapshot file wraps the same terminal-snapshot payload encoding
  used by the negotiated Ducklion bridge protocol, avoiding a second terminal
  state serializer.
- The file envelope contains a fixed magic header, snapshot format version,
  Ducklion instance UUID, session ID, saved timestamp, payload length, and a
  CRC32 checksum of the payload.
- Ducklord validates the envelope identities, declared length, supported version,
  and checksum before rendering any saved content.
- An invalid, corrupt, mismatched, or unsupported snapshot is ignored with a
  local warning and never blocks a fresh remote attach.
- Bridge protocol evolution and on-disk format evolution are versioned
  explicitly; Ducklord does not assume that every future bridge payload can be
  decoded by an older snapshot reader.

## 140. Stale snapshot interaction

Status: Decided

- A detached snapshot displayed before fresh attachment is a local read-only
  recall surface and does not prove that the remote session still exists.
- Ducklord permits local viewport and scrollback navigation over the stale
  snapshot.
- It sends no PTY input, resize, yield, or other session mutation while only
  stale content is available.
- Remote controls become available only after the corresponding Ducklion bridge
  confirms the instance and session ID, supplies current metadata, and the
  requested attachment succeeds.
- A failed or missing remote session leaves the stale snapshot visible with its
  `STALE` and unavailable status until the user selects another session or
  confirms local cleanup.

## 141. Ducklord owner identity across hosts

Status: Decided

- One Ducklord process resolves one owner name and presents that same name to
  every configured Ducklion host bridge.
- `~/.ducklord/config.yaml` does not support per-host owner-name overrides in the
  first version.
- Writer ownership remains scoped to each Ducklion instance and session, so the
  shared name does not combine session state across hosts.
- A name collision or live-connection conflict is evaluated independently by
  each Ducklion instance and affects only that host bridge.

## 142. Focus-derived Ducklord yield target

Status: Decided

- When the session list has focus, a Ducklord yield action targets the currently
  selected list session.
- When the PTY pane has focus, the action targets the currently active attached
  session.
- If selected and active sessions differ, Ducklord follows focus and does not
  silently substitute the other session or open an additional confirmation.
- The focused border and local yield status display include the target session's
  host alias and handle so the user can identify the operation target.
- Ducklion still resolves the immutable instance and session IDs and applies
  all normal pending-yield, owner, adapter, and idle checks.

## 143. Selected and active session indicators

Status: Decided

- The session-list selection cursor is rendered as a highlighted row.
- The session currently displayed in the active PTY pane has a persistent `●`
  marker on its list row.
- A row may simultaneously carry the selection highlight and active marker.
- Moving list selection never moves the active marker until another attachment
  succeeds.
- The row continues to show its host alias and ownership/status indicators so
  selection and attachment do not obscure operational state.

## 144. Non-active session unread activity

Status: Decided

- Ducklion session metadata changes include a last-activity update without
  requiring every Ducklord view to consume that session's raw PTY stream.
- Ducklord displays a `•` unread marker when a non-active session reports
  activity newer than the last fresh snapshot Ducklord viewed for it.
- Successfully attaching the session and receiving its fresh terminal snapshot
  clears the unread marker.
- Selecting a row, displaying a stale local snapshot, or receiving metadata
  alone does not clear unread state.
- Unread state is presentation metadata and never affects task, ownership,
  subscription, or yield behavior.

## 145. Ducklord session-event subscription

Status: Decided

- Each live host bridge maintains one session-event subscription covering every
  session visible from that Ducklion instance.
- The event stream carries lightweight session-created, removed, status,
  last-activity, task-completion/failure, owner, pending-yield, and adapter-state
  changes used by the session list and unread indicators.
- It never carries raw PTY output and does not require Ducklord to attach each
  session.
- Session-event subscription is established after the authoritative list
  snapshot and rebuilt after every bridge reconnect.
- This subscription remains active regardless of which PTY is displayed.

## 146. Ducklord raw-output subscription pool

Status: Decided

- Ducklord maintains a process-wide raw PTY output subscription pool with a
  default capacity of ten sessions across all host bridges.
- The capacity is configurable in `~/.ducklord/config.yaml` and takes effect
  after Ducklord restart.
- The active PTY session must be subscribed and is pinned against eviction.
- Non-active sessions may remain subscribed within capacity. Ducklord continues
  parsing their raw output into per-session in-memory terminal state.
- Ducklion pushes raw output only for explicitly subscribed sessions; the
  independent all-session event stream remains active for every session.
- Raw subscriptions are connection-scoped and are rebuilt from Ducklord's
  desired pool after host reconnect and authoritative synchronization.
- Ducklion validates that each requested session belongs to its own instance and
  rejects unknown session IDs.

## 147. Raw-output subscription LRU policy

Status: Decided

- Successfully switching the right-hand PTY pane to a session moves that session
  to the most-recently-used position and pins it while active.
- List selection, metadata events, unread events, and background raw output do
  not update LRU order.
- Switching away unpins the former active session but leaves its raw subscription
  and in-memory terminal state alive while pool capacity permits.
- Before subscribing a session beyond capacity, Ducklord chooses the
  least-recently-used non-active session, atomically saves its newest terminal
  state to its detached snapshot file, then unsubscribes it and releases its
  in-memory framebuffer.
- Switching to a session already in the pool displays its current in-memory
  framebuffer immediately without rebuilding the subscription.
- Switching to an evicted session displays its stale file snapshot while
  Ducklord subscribes and obtains an authoritative fresh snapshot plus offset
  continuation.


## 148. Persistent unread activity cursors

Status: Decided

- Ducklion maintains a monotonically increasing activity sequence per session
  and notification category and includes the current category-cursor map in
  session snapshots and the affected cursor in live events.
- Ducklord stores the last-seen sequence per category for each instance
  UUID/session ID pair in `~/.ducklord/state.json`.
- After Ducklord restart, host reconnect, or an offline interval, an enabled
  category whose current sequence exceeds its saved last-seen value displays the
  unread marker.
- Successfully attaching and receiving a fresh terminal snapshot advances every
  saved category cursor to the snapshot's values and clears unread.
- Ducklord compares sequences rather than local and remote wall-clock timestamps,
  so clock skew cannot hide or invent unread activity.
- Recreating a session under a new UUID starts independent unread state and never
  inherits the old session's cursor.

## 149. Agent-driven terminal attention events

Status: Decided

- An agent running inside a managed PTY decides when user attention is needed by
  emitting its existing terminal notification sequence, including BEL or
  supported notification OSC sequences such as OSC 9, OSC 99, and OSC 777.
- The PTY supervisor's terminal parser recognizes complete notification
  sequences and reports them to Ducklion independently from raw-output
  subscription state.
- Ducklion increments the session's terminal-attention activity sequence and
  publishes a
  `TerminalAttention` item on the all-session event stream, carrying only the
  session identity and that category sequence.
- Ducklion forwards the notification but does not infer that BEL or OSC means a
  task completed. Authoritative running, completed, failed, cancelled, and
  timeout states continue to come from the agent adapter.
- Ducklord treats `TerminalAttention` as a payload-free unread signal. A
  non-active session receives the unread marker; an active session does not mark
  itself unread.
- The original BEL or OSC bytes remain in the active session's raw PTY stream so
  its terminal behavior is preserved. Ducklord must avoid producing a second
  user notification from the parallel attention event for that active session.

## 150. Terminal attention sequence allowlist

Status: Decided

- Ducklion recognizes bare BEL, general OSC 9 notifications, OSC 99, and OSC 777
  as terminal attention sequence families.
- OSC 0, OSC 1, and OSC 2 title changes and all other terminal control sequences
  remain ordinary terminal state/output and do not increment the attention
  activity sequence.
- Ducklion parses only enough framing to recognize a complete allowlisted
  sequence. It does not decode, retain, or forward its message, progress value,
  OSC subtype, or other payload content.
- Ducklion distinguishes the OSC 9;4 prefix only far enough to exclude taskbar
  progress updates from `TerminalAttention`; it does not parse their state or
  percentage.
- Unsupported or malformed OSC input never becomes a `TerminalAttention` event,
  although the terminal parser continues handling raw display output under its
  normal safety rules.
- Because the event contains no terminal-provided payload, Ducklord never renders
  notification text and exposes no notification-payload injection surface.

## 151. Terminal attention versus agent completion

Status: Decided

- Codex turn completion is the structured `agent-turn-complete` event, which its
  TUI may render as a general OSC 9 notification or BEL according to user
  configuration; it has no completion-specific OSC number.
- Ducklion treats recognized BEL and notification OSC sequences only as generic
  attention signals and does not derive completed, failed, approval, or input
  state from them.
- OSC 9;4 progress sequences do not produce unread attention events.
- Authoritative task terminal state always comes from the session's agent
  adapter, even when no terminal notification was emitted or notifications were
  disabled.

## 152. Agent terminal-notification configuration ownership

Status: Decided

- Ducklion and its adapters do not rewrite the user's Codex or Claude Code
  notification settings.
- They do not force notifications on, change the selected BEL/OSC method, or
  override focused/unfocused notification conditions.
- When an agent emits an allowlisted terminal notification naturally, Ducklion
  forwards the corresponding payload-free attention event.
- When terminal notifications are disabled or suppressed, structured adapter
  events still drive authoritative task completion, failure, approval, and input
  state and the related Ducklord session-list indicators.
- Terminal attention is therefore an optional agent-controlled signal, not a
  prerequisite for correct Ducklion lifecycle management.

## 153. Per-session Ducklord notification filters

Status: Decided

- Ducklord supports per-session notification settings keyed by Ducklion instance
  UUID plus session ID.
- First-version filter categories are terminal attention, task completed, task
  failed, task cancelled, task timeout, approval required, agent needs input,
  and unexpected process exit.
- Every category is enabled by default for a newly discovered session.
- An enabled event for a non-active session advances its unread presentation and
  displays the session-list marker; the same event for the active session updates
  status without marking it unread.
- Disabling a category affects only Ducklord notification and unread behavior.
  Ducklord still consumes the event and updates authoritative task, adapter,
  process, and ownership metadata.
- Per-session filters are stored in `~/.ducklord/state.json` and survive Ducklord
  restart without changing Codex, Claude Code, Ducklion, or Discord settings.

## 154. Ducklord per-session notification menu

Status: Decided

- With the session list focused, pressing `n` opens a local notification menu
  for the selected session and displays its host alias, handle, and owner.
- The menu lists terminal attention, task completed, task failed, task cancelled,
  task timeout, approval required, agent needs input, and unexpected process exit
  as independent checkboxes.
- Space toggles the highlighted category, `a` enables all, `x` disables all, and
  `r` restores the all-enabled default.
- Changes remain staged until Enter atomically saves them to
  `~/.ducklord/state.json`; Escape discards the staged changes.
- The menu cannot be opened directly while the PTY pane has focus; the user must
  first return focus to the session list.
- Filter changes affect only future events. Disabling a category does not clear
  an existing unread marker; a successful fresh attachment still clears it.
- No menu operation calls Ducklion, changes the all-session event subscription,
  modifies agent configuration, or sends a Discord message.

## 155. First-version Ducklord notification presentation

Status: Decided

- For a non-active session, the first Ducklord version presents enabled
  notification events only through the session-list unread marker and updated
  status metadata.
- Ducklord does not emit an additional desktop notification, BEL, notification
  OSC sequence, sound, or operating-system toast for background sessions.
- An active PTY's original raw BEL or OSC sequence remains available to the
  user's terminal under the normal raw-output path.
- Desktop, audible, and richer notification presentation are reserved for a
  later feature and must remain optional if implemented.
- Reserving that future capability does not change the first-version event
  protocol or per-session filter categories.

## 156. Group-level unread aggregation

Status: Decided

- In every organization mode, a group header displays an unread marker when at
  least one session projected into that group is unread.
- The first version exposes only the boolean marker and does not display an event
  or unread-session count.
- The group marker is derived from child session unread state and disappears
  automatically only after no child session remains unread.
- Opening, focusing, expanding, collapsing, or reordering a group does not clear
  any session unread state.
- A future collapsible group-list UI must retain this aggregation so unread
  sessions remain discoverable while their rows are hidden.

## 157. First-version group expansion

Status: Decided

- Every group is permanently expanded in the first Ducklord version.
- The first version does not expose collapse/expand controls or persist collapsed
  group state.
- Group headers still display the aggregated unread marker defined by the stable
  list contract.
- Collapsible groups are reserved for a later UI feature and may add local
  persisted expansion state without changing Ducklion or the session-event
  protocol.

## 158. Notification activity-sequence sources

Status: Decided

- Ducklion advances the corresponding per-category notification activity
  sequence for each recognized terminal attention, task completion, task
  failure, task cancellation, task timeout, approval-required,
  agent-needs-input, and unexpected-process-exit event.
- Ducklion advances and persists the category sequence before publishing the
  corresponding session event so reconnect snapshots cannot report an older
  cursor.
- Ducklion does not apply Ducklord per-session filters; every bridge receives the
  authoritative sequence and event metadata.
- Ducklord applies its local filter before deciding whether a non-active session
  becomes unread, while always updating authoritative session status.
- Ordinary raw PTY output and OSC 9;4 progress do not advance the notification
  activity sequence.

## 159. Notification cursors for the active session

Status: Decided

- When an enabled notification event belongs to the session currently displayed
  in the active PTY pane, Ducklord does not mark that session unread and
  immediately advances the affected local last-seen category cursor.
- Persisting that cursor prevents the already visible event from becoming unread
  merely because the user later displays another session or restarts Ducklord.
- For a non-active session, an enabled newer category cursor leaves the saved
  last-seen value unchanged and displays unread until a successful fresh attach.
- A successful fresh snapshot advances all category cursors to the snapshot's
  current values and clears unread for that session.
- A stale local snapshot is not proof of seeing current remote events and does
  not advance any cursor.

## 160. Notification cursors for disabled categories

Status: Decided


- When a notification category is disabled for a session, Ducklord still
  consumes its events and advances that category's local last-seen cursor.
- Such events do not set the session or group unread marker, regardless of
  whether the session is active.
- Re-enabling the category starts notification comparison from the latest cursor
  already observed and does not surface events that occurred while disabled.
- Offline reconciliation follows the same rule: disabled category cursors from
  the authoritative snapshot are accepted as seen without producing unread.
- Changing a filter therefore affects future notification presentation only and
  never asks Ducklion to replay or suppress event history.

## 161. Terminal-attention event coalescing

Status: Decided

- Ducklion publishes at most one `terminal_attention` event per session in each
  one-second window.
- Additional recognized BEL or notification OSC sequences in the same window are
  coalesced into that event and do not advance the category sequence again.
- Coalescing affects only the payload-free terminal-attention category; raw PTY
  output for the active subscription remains unchanged.
- Structured adapter events such as task completion, failure, approval request,
  input request, timeout, cancellation, and unexpected exit are not rate-limited
  or merged by this rule.
- Coalescing state is transient and restart may begin a new window without
  violating unread semantics.

## 162. Notification categories by session kind

Status: Decided

- Agent sessions expose every first-version notification category in the
  per-session notification menu.
- Shell sessions expose terminal attention and unexpected process exit.
- Agent-only task completion, failure, cancellation, timeout, approval-required,
  and agent-needs-input categories remain visible but disabled as unavailable
  when editing a shell session, so the common menu layout remains understandable.
- `a`, `x`, and `r` operate only on categories available to the selected session
  kind.
- A later session-kind or adapter capability change causes Ducklord to recompute
  availability from authoritative metadata without inventing events for formerly
  unavailable categories.

## 163. Ducklord terminal-state snapshot source

Status: Decided

- For every raw-output subscription, Ducklord parses PTY bytes into an in-memory
  terminal render state.
- The state contains the current screen cells and text, retained scrollback,
  styles and colors needed to redraw content, cursor state, and soft-wrap versus
  hard-line-boundary information.
- A detached snapshot serializes this parsed terminal state. It stores neither
  the replayable raw PTY byte ring nor rasterized screen pixels.
- Before LRU eviction and during orderly Ducklord shutdown, Ducklord atomically
  saves the latest render state for every subscribed session to that session's
  snapshot file.
- A Ducklord crash may lose render-state changes since the last detach, eviction,
  or successful orderly-shutdown save; the first version does not periodically
  checkpoint active subscriptions.

## 164. Raw-output LRU process lifetime

Status: Decided

- Ducklord keeps raw-output subscription LRU membership and order only in process
  memory.
- It does not write the LRU pool to `~/.ducklord/state.json` or infer it from
  detached snapshot files on startup.
- A new Ducklord process begins with an empty raw-output pool, then subscribes the
  session chosen as its initial active PTY; later displayed sessions populate the
  pool normally.
- Detached snapshot files remain available for stale recall but do not cause
  background subscriptions by themselves.
- Within one running Ducklord process, a temporary host-bridge reconnect may
  rebuild that host's still-desired in-memory subscriptions after authoritative
  synchronization without making the LRU persistent across process restart.

## 165. Raw-output subscription limit validation

Status: Decided

- The Ducklord raw-output subscription limit must be an integer from 1 through
  100, inclusive.
- The minimum reserves capacity for the active PTY subscription.
- The default remains 10 across the entire Ducklord process and all host bridges.
- Ducklord rejects an absent-type, non-integer, zero, negative, or above-maximum
  configured value as a startup configuration error rather than clamping it.
- Changing the value requires Ducklord restart under the established no-hot-reload
  policy.

## 166. Ducklord in-memory terminal-state limit

Status: Decided

- Each subscribed session's Ducklord in-memory terminal render state is bounded
  to the same 4 MiB serialized payload limit used by detached snapshots.
- Ducklord preserves the complete current screen first, then the newest
  scrollback that fits, evicting the oldest complete logical lines.
- Once eviction occurs, the in-memory state records truncation so a later stale
  snapshot and view can display the `TRUNCATED` indicator.
- The limit applies independently to each raw-output subscription and does not
  replace the process-wide subscription-count limit.
- Ducklord does not drop current-screen cells merely to preserve older
  scrollback; a valid screen that alone cannot fit is reported as an unsupported
  terminal-state error rather than serialized partially.

## 167. Raw-subscription handoff slot

Status: Decided

- When the configured raw-output pool is full and Ducklord switches to an
  unsubscribed session, it may temporarily hold one additional handoff
  subscription beyond the configured limit.
- Ducklord subscribes and obtains a valid snapshot for the destination before
  replacing the active pane or unpinning the source session.
- If destination setup fails, Ducklord closes only the temporary subscription
  and leaves the former active PTY, subscription pool, focus, and LRU order
  unchanged.
- After destination setup succeeds, Ducklord commits the pane switch, pins the
  destination, unpins the source, then saves and unsubscribes the least-recently
  used non-active session until the configured limit is restored.
- At most one temporary handoff slot may exist per Ducklord process, so concurrent
  UI actions cannot grow the pool repeatedly.

## 168. Ducklord local session search

Status: Decided

- Ducklord provides an incremental local search/filter over its synchronized
  combined session model.
- Search matches session handle, project name, configured host alias, custom-group
  display name, session kind, and agent implementation type.
- Matching is case-insensitive for scripts with case and accepts Unicode query
  text, including Chinese.
- Filtering preserves the current organization mode, group projection, and saved
  manual relative order among visible results.
- Search sends no Ducklion request and never changes group membership, ordering,
  selection identity, active attachment, or unread state.
- Search text is process-local and is not stored in `state.json`.

## 169. Ducklord session-search matching

Status: Decided

- The first version performs Unicode-aware case-insensitive substring matching
  and does not apply fuzzy ranking.
- Ducklord splits the trimmed query on Unicode whitespace into non-empty terms.
- Every term must occur in at least one of the selected session's combined
  searchable fields; terms may match different fields.
- Results retain their organization-mode grouping and shared manual order rather
  than being reordered by match quality.
- An empty query shows the unfiltered list.

## 170. Search-result group visibility

Status: Decided

- While a search is active, Ducklord hides organization-mode groups containing
  no matching visible session.
- Groups and sessions reappear in their prior positions immediately when the
  query is cleared or changed to match them.
- Search is a temporary in-memory presentation projection only. It never removes
  a custom group, session entry, membership, manual order, unread cursor, or
  snapshot file and never writes search results to `state.json`.
- An empty result displays a local no-matching-sessions message without replacing
  the active PTY pane.

## 171. Search-result activation flow

Status: Decided

- While search is active, the first Enter on a selected result attempts to make
  that session the active right-hand PTY through the normal safe attachment and
  subscription handoff path.
- After a successful switch, Ducklord keeps the search query and result list
  visible with list focus and enters an activated-result state.
- Pressing Enter again without changing the query or selection clears the search,
  restores the complete session list, and moves focus to the active PTY input.
- Editing the query or moving selection after activation clears the
  activated-result state; the next Enter activates the newly selected result
  rather than closing search.
- If the first attachment attempt fails, search and list focus remain active and
  the second-Enter transition is not armed.

## 172. Ducklord session action menu

Status: Decided

- From the session list, Ducklord provides a local action menu for the selected
  session with attach, immediate yield, waiting yield, restart, waiting restart,
  forced restart, end, waiting end, forced end, destroy, waiting destroy, and
  forced destroy actions.
- Agent sessions expose the yield and immediate/wait/force lifecycle variants.
  Shell sessions expose only their separate immediate forceful restart, end, and
  destroy actions.
- Ducklord derives action availability from synchronized owner, session, task,
  adapter, and bridge state and disables actions known to be unavailable.
- Attach remains read-only when the Ducklord owner name does not match the
  persisted writer; yield remains available under its separate non-owner rules.
- Every mutating action sends the selected instance UUID, session ID, Ducklord
  owner name, and required generation or ownership fencing to Ducklion.
- Ducklion is the final authorization and state authority and revalidates every
  request even when Ducklord displayed it as enabled.
- An action rejected due to a concurrent state change reports the current state
  and does not cause Ducklord to retry or substitute another mode automatically.

## 173. Ducklord-created agent sessions and Discord binding

Status: Decided

- Ducklord may create both `shell` and `agent` managed sessions through
  Ducklion.
- A Ducklord-created agent session immediately starts its PTY, selected agent
  runtime, and required adapter, with terminal ownership assigned to the
  creating Ducklord owner name.
- It initially has no Discord task-channel binding.
- An authorized Discord management channel binds such an existing session with
  `!bind <session-id>`; Duckway creates a new task channel and persists the
  channel/session association.
- Binding does not transfer PTY writer ownership or submit agent input. The new
  CC task channel remains unable to send prompts until it obtains ownership with
  normal `!yield` or `!yield -w` semantics.
- `!new` remains the direct CC workflow that creates a new agent session and its
  task channel together with initial CC ownership.

## 174. Discord bind routing

Status: Decided

- A Duckway CC creates and owns its Discord management channel as part of its
  existing channel setup.
- When that management channel receives `!bind <session-id>`, the owning Duckway
  CC queries the Ducklion instance on the same host where that CC runs.
- The server does not broadcast the session ID to other Duckway clients or Ducklion
  hosts and does not infer a target from Ducklord's multi-host view.
- If the local Ducklion does not contain the supplied session ID, the command
  fails immediately in the management channel.
- The permanent session ID remains sufficient for local lookup; agent type and
  other metadata are returned by Ducklion rather than supplied as routing input.

## 175. Read-only Discord binding

Status: Decided

- `!bind` creates and persists a Discord task channel associated with an existing
  local agent session, but grants that channel no PTY writer or mutation rights.
- The bound channel may display synchronized session identity, status, owner,
  and other read-only metadata while the terminal owner remains unchanged.
- Prompts and owner-gated management operations from that channel remain rejected
  by Ducklion until CC requests yield and Ducklion accepts the transfer under
  the normal immediate or waiting rules.
- Binding is not evidence of idle state, does not alter the ownership epoch, does
  not send PTY input, and does not interrupt the current terminal task.
- The bind operation is authorized by the Discord management channel and scoped
  to an unbound local agent session; it is not treated as a writer-authorized PTY
  mutation.

## 176. One-to-one Discord session binding

Status: Decided

- An agent session may be bound to at most one Discord task channel, and a task
  channel may be bound to at most one agent session.
- Duckway enforces both directions with durable uniqueness constraints rather
  than relying only on command preflight.
- Repeating `!bind` for a session already bound through the same Duckway CC does
  not create another channel and replies with the existing channel identity.
- An attempt to bind a session or channel already bound to a different counterpart
  fails without modifying either binding.
- Prompt routing, progress, final replies, ownership notifications, and channel
  lifecycle operations use this single stable association.

## 177. Durable Discord bind workflow

Status: Decided

- Duckway persists `!bind` as a recoverable workflow with `reserved`,
  `channel_created`, and `active` states.
- Reservation atomically claims the local instance/session ID and source
  management-channel request before any Discord channel is created.
- After Discord channel creation, Duckway persists its channel ID before
  activating the one-to-one association.
- Process restart resumes a non-terminal workflow from its persisted state and
  never creates a second channel for the same reservation.
- If activation reaches an unrecoverable failure after channel creation, Duckway
  archives the orphan task channel, marks the workflow failed, and leaves the
  agent session unbound and ownership unchanged.
- Repeated handling of the same management-channel command returns the existing
  workflow or active binding result rather than duplicating external effects.

## 178. Bound task-channel naming and placement

Status: Decided

- `!bind` uses the same Discord task-channel naming, sanitization, collision,
  category-placement, topic, and permission policy already used by `!new`.
- It supplies the existing Ducklion session's handle, project, session kind,
  agent type, and stable identifiers to that shared policy instead of creating a
  second bind-specific naming scheme.
- A channel created by `!bind` is operationally indistinguishable from a
  `!new`-created task channel after binding, except that its initial association
  is read-only until CC obtains writer ownership through yield.
- Changes to the shared channel policy apply consistently to future `!new` and
  `!bind` operations after the required Duckway restart.

## 179. Ducklord agent-session creation wizard

Status: Decided

- Ducklord creates an agent session through a sequential local wizard backed by
  live Ducklion queries rather than accepting all fields in one free-form input.
- The user first selects one connected Ducklion host.
- Ducklord asks that host's Ducklion for its available configured project
  locations and presents the returned list for selection.
- After project selection, Ducklord asks the same Ducklion which agent types are
  available for that project and presents only those returned capabilities.
- The final step asks for a session handle. Submitting an empty value with Enter
  uses the selected project's final directory name as the default handle.
- Ducklord then submits the resolved host, stable project identity, agent type,
  and handle to Ducklion. Successful creation starts the PTY, agent runtime, and
  adapter immediately with terminal ownership assigned to the current Ducklord
  owner name and no Discord binding.
- An offline host, stale project, unavailable agent type, or failed final
  revalidation returns to the relevant wizard step without creating a partial
  session.

## 180. Session ID encoding and display names

Status: Decided

- A Ducklion `session_id` is six characters generated from the Crockford Base32
  alphabet `0123456789ABCDEFGHJKMNPQRSTVWXYZ` using a cryptographically secure
  random source.
- Ducklion enforces uniqueness within its instance database and retries random
  generation on collision before committing session creation.
- Duckway, Ducklion, and Ducklord display IDs in uppercase and accept lowercase
  user input using case-insensitive comparison.
- Session handles are non-unique Unicode display names and are never used alone
  for lookup, routing, ownership, binding, or mutation.
- When equal handles are visible together, Ducklord and Discord management
  responses include the six-character session ID to disambiguate them.
- Cross-host identity remains the Ducklion instance UUID plus six-character
  session ID pair.

## 181. Session handle validation

Status: Decided

- A session handle is an independent display-name type and does not inherit
  Ducklord custom-group naming limits merely because both support Unicode.
- A handle contains 1 to 128 Unicode code points after Ducklion trims leading
  and trailing whitespace.
- Handles may contain Chinese and other Unicode text and do not need to be unique.
- Ducklion rejects an empty result, NUL, line breaks, and Unicode control
  characters, but otherwise does not restrict punctuation or path-like text.
- The original validated handle is preserved in session metadata. Discord applies
  its existing task-channel name sanitization separately and does not rewrite the
  Ducklion handle.

## 182. Ducklord shell-session creation wizard

Status: Decided

- Ducklord creates a shell session through a sequential wizard.
- The user first selects a connected Ducklion host, then selects one of that
  host's configured projects or the explicit `Ducklion default` location.
- `Ducklion default` resolves to `~/.duckway/ducklion` on the selected host.
- The final step accepts a session handle; an empty value uses the selected
  directory's final path component as the default.
- Ducklion revalidates the directory and handle, resolves the operating-system
  user's configured shell, then executes it without injected arguments in a
  managed PTY at that working directory. The PTY causes the shell to enter its
  normal interactive non-login mode.
- The new session kind is `shell`, has no agent adapter or Discord binding, and
  permits shared writable attachment by authenticated Ducklord bridges.
- Host, project, directory, shell, or creation failure returns to the relevant
  wizard step without leaving a partial session.

## 183. Shell-session shared writer model

Status: Decided

- Shell sessions are available only through authenticated Ducklord bridges and
  are never available to Discord CC for binding, task submission, input, or
  management.
- Unlike agent sessions, a shell session has no exclusive persisted writer owner
  and exposes no immediate or waiting yield operation.
- Multiple Ducklord owner names may attach to the same shell session with write
  access concurrently.
- Every PTY input frame still passes through Ducklion. Ducklion validates the
  bridge and shell session, then serializes accepted input into the shared PTY in
  arrival order.
- Concurrent users may interleave bytes and commands. Ducklion does not provide
  input focus, locking, coordination, or implicit single-writer protection for a
  shell session.
- Creator and attached Ducklord names remain available as metadata and audit
  identities but do not grant exclusive shell input rights.

## 184. Shell-session PTY size

Status: Decided

- When a shell session has multiple writable Ducklord attachments, Ducklion sets
  the shared PTY size to the smallest reported column count and smallest reported
  row count across those attachments.
- Attaching, resizing, or detaching a writable shell client causes Ducklion to
  recompute the effective size and apply it under the normal validated resize
  bounds.
- Each client still crops, pads, and navigates locally when its viewport differs
  from the effective shared size.
- A read-only observer, if one is introduced by a future access mode, does not
  participate in shell size calculation.
- When no Ducklord is attached, the shell retains its last effective PTY size
  until another attachment arrives.

## 185. Shell-session lifecycle authorization

Status: Decided

- Any authenticated Ducklord bridge connected as the operating-system user
  running the Ducklion instance may request the immediate `restart`, `end`, or
  `destroy` operation for a shell session.
- Ducklion does not require the requester name to match the shell creator or any
  exclusive owner because shell sessions use the shared-writer model.
- Ducklion records the requester owner name and bridge identity in audit for
  every lifecycle request.
- The same host-user authorization does not bypass exclusive ownership checks on
  agent sessions.
- Concurrent shell attachments receive the resulting session status and removal
  events through their normal bridge streams.

## 186. Shell-session lifecycle semantics

Status: Decided

- Shell session restart, end, and destroy are explicit immediate operations and
  do not inspect or infer whether a foreground shell command is running.
- Restart terminates the current shell process and starts the persisted shell
  executable without injected arguments in a new PTY at the session's
  configured working directory, preserving the Ducklion session identity and
  metadata.
- End terminates the current shell process and retains the stopped Ducklion
  session and its metadata so it may be inspected or restarted later.
- Destroy terminates the current shell process and then removes the Ducklion
  session and its persisted metadata under the normal removal-event rules.
- Ducklord does not offer `-w/--wait` or a separate `-f/--force` variant for these
  shell actions; selecting the action itself is the user's forceful decision.
- Ducklion uses its standard process-group termination sequence and reports the
  result to every affected attachment. The user is responsible for deciding
  whether terminating visible shell work is appropriate.

## 187. Shell-session termination sequence

Status: Decided

- Shell restart, end, and destroy first close the PTY master and send `SIGHUP`
  to the shell process group.
- Ducklion waits up to five seconds for the entire process group to exit.
- If processes remain, Ducklion sends `SIGTERM` to the process group and waits
  another five seconds.
- If processes still remain, Ducklion sends `SIGKILL` to the process group and
  waits for process reaping before completing the lifecycle operation.
- Restart launches the replacement shell only after the prior process group is
  fully reaped; end and destroy likewise do not report completion while known
  children remain.

## 188. Shell lifecycle attachment behavior

Status: Decided

- Restart preserves every Ducklord attachment to the logical shell session.
  Each attached client displays a restarting state and automatically receives
  the replacement shell runtime and its fresh terminal state when ready.
- End preserves each attachment and its final terminal view in a stopped,
  read-only state. Ducklion rejects further PTY input until the session is
  restarted.
- Destroy emits a session-removal event and closes the destroyed session view
  on every attached Ducklord.
- If the destroyed session was active, Ducklord selects the first session under
  the user's current ordering. If no session remains, it shows the empty state.

## 189. Shell restart launch configuration

Status: Decided

- In the first version, Ducklion persists the resolved user shell executable and
  working directory. It executes that shell without injected arguments inside
  the PTY, allowing the shell to select its normal interactive non-login mode.
- Restart launches the replacement shell from that persisted configuration.
- Ducklion does not capture or persist the creating process's environment for
  later shell restarts. The newly launched shell establishes its environment
  through the operating-system user environment and its normal interactive
  startup files, such as `bashrc` where applicable.
- Runtime state changed inside the terminated shell, including its current
  directory, exported variables, aliases, functions, and shell options, is not
  carried into the replacement shell.
- Restart therefore produces the same defined launch environment regardless of
  which Ducklord requested it or what commands previously ran in the old shell,
  subject to the user's current shell startup configuration.
- User-defined launch commands, arguments, login-shell mode, and explicit
  environment-variable overrides are reserved for a later launch-config
  feature and are outside the first-version scope.

## 190. Shell restart launch failure

Status: Decided

- If the prior shell has terminated but Ducklion cannot launch its replacement,
  the logical session remains present and enters the `stopped` state.
- Existing attachments retain the prior terminal view and receive the launch
  error for display.
- Ducklion does not automatically retry, create a replacement session, or roll
  back to the terminated process.
- After correcting the relevant configuration or host condition, the user may
  explicitly request restart again on the same session identity.

## 191. Natural shell-process exit

Status: Decided

- When a shell process exits by `exit`, failure, or an external signal without
  a Ducklion lifecycle request, its logical session enters the `stopped` state.
- Ducklion retains the session metadata, final terminal state, and process exit
  result, including the exit code or terminating signal.
- Ducklion emits the stopped status to attached Ducklords but does not
  automatically restart or destroy the session.
- The user may later restart or destroy the same logical session explicitly.

## 192. Shell lifecycle-operation serialization

Status: Decided

- Ducklion permits at most one restart, end, or destroy operation to execute for
  a shell session at a time.
- While one lifecycle operation is in progress, Ducklion immediately rejects
  every additional lifecycle request for that session with a `busy` result.
- Rejected requests are not queued or retried automatically, regardless of
  which Ducklord bridge submitted them.
- The active operation alone determines the resulting session state and emits
  its normal status or removal events.

## 193. PTY supervisor recovery authentication

Status: Decided

- Ducklion generates an Ed25519 recovery key pair whenever it creates a PTY
  supervisor and persists only the public key with the session's supervisor
  recovery metadata.
- The supervisor retains its private key only in process memory and does not expose it
  through command-line arguments, environment variables, status output, or
  bridge messages.
- Ducklion delivers the initial private key through a private inherited pipe or
  socketpair during supervisor spawn and closes its parent copy after bootstrap;
  it is never written to a filesystem path.
- After Ducklion restarts, it issues a fresh random nonce. The surviving
  supervisor signs a domain-separated message containing the Ducklion instance
  ID, session ID, runtime generation, negotiated protocol version, and nonce.
- Ducklion accepts re-adoption only when the session and runtime generation
  match and the signature verifies with the persisted public key. A nonce is
  single-use, so a captured proof cannot be replayed. A mismatch is rejected
  and the logical session remains in its fail-closed recovery state.
- Successful re-adoption still requires the existing protocol negotiation,
  adapter synchronization, and health validation before any writer input or
  ownership transfer is enabled.

## 194. Bridge mutation-request idempotency

Status: Decided

- Every bridge request that may change Ducklion state carries a
  cryptographically random `request_id` generated by the requesting client.
- Idempotency identity is scoped to the authenticated principal and binds the
  operation, target session, and canonical payload fingerprint. Reusing an ID
  with different bound data fails with `idempotency_conflict`.
- For database-only mutations, Ducklion atomically commits the request
  reservation, state transition, audit entry, and replayable result in one
  SQLite transaction before acknowledging the request.
- Receiving the same principal and `request_id` again returns the recorded
  result and never executes the mutation a second time.
- This applies to session creation and lifecycle operations, yield requests,
  Discord binding mutations, and other state-changing bridge operations; PTY
  input and read-only queries are not mutation requests under this rule.
- Completed idempotency records are retained for one week and cleaned up under
  Ducklion's configurable retention process.
- A request still in progress returns its existing in-progress identity or
  status rather than starting a parallel operation. Operations with external
  process or Discord side effects use durable phases that startup recovery must
  reconcile before accepting a retry; they are never blindly executed again.

## 195. Ducklion database migration safety

Status: Decided

- Ducklion automatically applies forward schema migrations during startup
  before accepting controller, bridge, or supervisor registrations.
- Migrations run under exclusive database migration ownership and use database
  transactions wherever SQLite permits atomic schema conversion.
- Before the first schema-changing step, Ducklion creates a timestamped backup
  of the database and its required SQLite sidecar state using SQLite's safe
  backup mechanism rather than copying a live database file directly.
- If backup or migration fails, Ducklion refuses to finish startup and accepts
  no control requests. Independently running PTY supervisors and their child
  processes remain alive and retry recovery registration later.
- Ducklion never performs an automatic schema downgrade. Running older software
  against a newer schema requires an explicit operator rollback using a
  compatible pre-migration backup.
- A successful migration records the new schema version before Ducklion begins
  normal recovery and connection handling.

## 196. Core end-to-end release gate

Status: Decided

- A release implementing this specification must pass automated end-to-end
  coverage for CC agent-session creation, Ducklord attachment, immediate yield,
  and waiting yield in both ownership directions.
- Tests must prove that PTY input from a non-owner is rejected before reaching
  the agent process.
- Recovery coverage must restart Ducklion while preserving the supervisor and
  verify authenticated re-adoption, adapter synchronization, and restored
  control eligibility.
- Bridge coverage must exercise disconnect and reconnect around a mutation and
  prove that retrying its `request_id` cannot execute it twice.
- Discord durable-ingress coverage must verify that ownership rejection is
  completed as a business outcome and that the rejection reason is posted to
  the originating channel.
- Shell-session coverage must exercise concurrent writable Ducklord
  attachments and the immediate restart, end, and destroy semantics.
- Service lifecycle coverage must verify both graceful restart draining and
  forced restart cancellation with stale-runtime event fencing.
- Failure of any required scenario blocks release of the feature.

## 197. Shell executable failure policy

Status: Decided

- Shell-session creation fails if the operating-system user's configured shell
  cannot be resolved, does not exist, is not executable, or cannot be launched.
- Restart uses the shell executable persisted for that session and enters the
  existing stopped-with-error outcome if it can no longer be launched.
- Ducklion does not silently fall back to `/bin/sh`, another shell, or an
  implementation-selected command for managed shell sessions.
- The launch error identifies the unusable executable without including the
  session environment or other secret values.

## 198. Discord end-to-end test tiers

Status: Decided

- Every normal CI run executes the credential-free Discord REST and Gateway
  fixture suite, covering durable ingress, routing, channel binding, yield, and
  reply behavior without contacting Discord.
- A separately controlled real-Discord `cc-smoke` environment supplies a test
  bot token, guild ID, and category ID and validates live provisioning and
  outbound Discord behavior.
- Real-Discord credentials are opt-in secrets and are never required, printed,
  persisted as repository fixtures, or exposed to untrusted pull-request jobs.
- The live smoke creates isolated temporary resources and performs best-effort
  cleanup after success or failure.

## 199. Live Discord smoke-test boundary

Status: Decided

- The first-version live `cc-smoke` uses only the dedicated Duckway test-bot
  token and validates Discord channel provisioning, permission preflight,
  outbound message creation and editing, Gateway connectivity, and cleanup.
- It does not attempt to impersonate a human Discord user or use a user token,
  self-bot, or other Discord-prohibited automation to generate inbound commands.
- Inbound `!new`, prompt, `!yield`, and related command flows remain fully
  covered by the deterministic Discord fixture E2E suite.
- Live inbound smoke may be added later using a separately authorized test actor
  bot and an explicit multi-bot test design; it is not required for the first
  version.

## 200. First-version implementation phases

Status: Planned

1. Implement Ducklion's protocol types, authoritative session state machine,
   SQLite schema and migrations, ownership fencing, and mutation idempotency.
2. Implement the independent PTY supervisor, agent adapters, recovery
   authentication, output/state streams, input validation, and lifecycle
   operations.
3. Integrate Duckway CC with Ducklion for `!new`, prompt delivery, progress,
   binding, yield, lifecycle commands, durable ingress outcomes, and restart
   draining.
4. Implement the SSH stdio bridge and the first-version Ducklord multi-host TUI,
   including attach, focus, session organization, subscriptions, snapshots,
   notifications, and shell sessions.
5. Add migration and upgrade compatibility coverage, deterministic fixture E2E,
   restart/recovery and race tests, plus the opt-in real Discord `cc-smoke`.
6. Treat the core E2E release gate as the completion criterion; later launch
   configuration, split panes, live inbound Discord actors, and other explicitly
   deferred features remain outside V1.

## 201. Legacy Duckway client compatibility

Status: Decided

- V1 is a deliberate breaking replacement of the existing prototype Duckway
  client, Ducklion name-based control API, and Ducklord attach protocol.
- Ducklion does not import or concurrently honor legacy
  `~/.ducklion/sessions.json` state, legacy session sockets, or name-addressed
  write and lifecycle requests.
- Operators must stop disposable legacy client sessions before upgrading. The
  new Ducklion initializes its independent state under
  `~/.duckway/ducklion` and accepts only the new versioned protocol.
- This cutover does not delete or migrate the Duckway server database, Discord
  control-channel configuration, API keys, or other server-owned data.
- No read/write compatibility shim is provided; an incompatible old client is
  rejected clearly and fail-closed.
