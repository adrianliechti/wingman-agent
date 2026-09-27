# codex

ACP bridge for the Codex CLI. `Spawn` launches `codex app-server` and translates
its JSON-RPC notifications into ACP session updates.

## Protocol reference

[Codex App Server](https://learn.chatgpt.com/docs/app-server)

The app-server surface is what this package speaks: `initialize`, `thread/start`,
`turn/start`, and the `item/*` and `turn/*` notification streams handled in
`events.go`.

## ACP versions

This adapter implements **ACP v1** using `acp-go-sdk v0.13.5`. A client requesting
v2 receives `protocolVersion: 1` and can continue with v1 or disconnect, as required
by [version negotiation](https://agentclientprotocol.com/protocol/v1/initialization).
Codex's App Server API also has types named `v2`; that is a separate protocol and
does not indicate ACP v2 support.

The [ACP v2 draft](https://agentclientprotocol.com/protocol/v2/migration) was
reviewed on 2026-09-07. It needs more than a version-number change:

| Surface | Implemented v1 | Draft v2 |
| --- | --- | --- |
| Prompt completion | Pending request returns `stopReason` | Immediate acknowledgement; `state_update` reports completion |
| Initialization | `agentInfo`, `agentCapabilities` | `info`, `capabilities` with a different structure |
| Tools and permissions | `tool_call`, `tool_call_update`, permission `toolCall` | Tool upserts, permission `title` and `subject` |
| History | `session/load` replays history | `session/resume` with `replayFrom` |

Keep one implementation while the Go SDK targets v1. Native v2 support should
migrate the connection surface together with these lifecycle changes when SDK
support is available. The wire tests exercise both a v1 initialization and a v2
initialization falling back to v1, including both cancellation methods.

## Connection reliability

Each turn has an ordered, bounded event queue. The app-server reader only routes
events, so a client update or permission dialog cannot block RPC replies. Events
and approval requests with a turn ID must match the active turn, including events
that arrive before the `turn/start` reply. Replaced sessions cannot remove the
replacement's handlers during cancellation cleanup.

Command output is buffered and sent once when the command completes. Each command
retains at most the last 1 MiB, with a visible truncation marker. The same limit
applies to command output replayed from history. This follows the scratch Codex
adapter's fixes for quadratic output growth (`4e5cffb`, `8aef91b`).

RPC acknowledgements have a 30-second deadline; interrupts have a 2-second
deadline. A timed-out start or interrupt retires the backend connection because
the adapter cannot establish which work is still running. Stalled client writes fail after
10 seconds and close the stdio output. These limits apply to transport operations;
model turns and user approval waits have no fixed overall deadline. Backend EOF,
framing errors, malformed completion payloads, exhausted retries, and failed turns become errors. Retrying error
events keep the turn open, and already-received completion events survive EOF.
Process exit is also monitored independently of stdout, so a tool that inherits
the pipe cannot keep a dead app-server session pending.

Codex answers `no active turn to interrupt` until a just-started turn becomes
interruptible, so a cancellation retries `turn/interrupt` with a short bounded
backoff instead of letting that turn run on.
Cancellation releases active and cancelled queued prompts. Late permission
responses cannot approve cancelled work, and steering leaves an open permission
request intact. These checks apply the lifecycle lessons from the reference
Codex cancellation fix (`f67ca5f`) and Claude's exact result attribution and
JetBrains approval/steering fixes (`a04d354`, `8710ce1`). The Codex app-server's
`turn/steer` API already queues input without Claude's interrupting `now` delivery.
The next prompt waits for cancellation cleanup, even when the previous start
reply arrived after cancellation. Resolving a backend approval cancels only that
request's dialog, using both its request ID and thread ID.

URL elicitation IDs are generated per interaction. Accepting a URL request means
consent to open it; `serverRequest/resolved` does not prove that the external
workflow finished. The adapter therefore omits the optional `elicitation/complete`
notification instead of completing unrelated URL interactions. This follows the
[elicitation lifecycle](https://agentclientprotocol.com/protocol/v1/elicitation).
The pinned Go SDK still lacks top-level elicitation scope fields; session scope
is currently carried in `_meta.sessionId`. Standard permission requests carry
their normal top-level `sessionId`.

`reliability_test.go`, `rpc_test.go`, `turn_stream_test.go`,
`approval_lifecycle_test.go`, and `session_lifecycle_test.go` cover these cases with local transports and a helper
process. `protocol_review_test.go` and `cancellation_order_test.go` verify protocol
envelopes and cancellation ordering. These tests do not make model requests or
replace an IntelliJ integration test.

## History and reference implementations

`session/load` reads all history before reporting success. Paginated Codex stores
use `thread/turns/list` with full items, preserve chronological replay order, and
reject repeated cursors. Legacy stores retain `thread/read` with `includeTurns`.
Read failures are reported instead of silently returning an empty conversation.
Resume and fork requests omit unnecessary history hydration. These changes follow
the current adapter's [history pagination fix](https://github.com/agentclientprotocol/codex-acp/commit/1a3c01e).
`history_review_test.go` covers both store formats and failure cases.

Codex writes a thread's rollout on its first user message. Resuming or loading a
session that was never prompted falls back to `thread/read` and replays no
history; deleting it, an unknown ID, or an ID Codex cannot parse succeeds.

The review compared the local scratch Codex adapter at `296069e`, the local
Claude adapter at `e6681d2`, and the current
[App Server adapter](https://github.com/agentclientprotocol/codex-acp) at `bf37821`.
The scratch Codex repository has moved development to that App Server adapter.
Upstream session notices and `compaction_update` need session update variants
and client capabilities that `acp-go-sdk v0.13.5` does not model, so Codex
advisories and compactions keep their text and tool-call presentation. Terminal
output metadata and AIR file change reports are not implemented here.

## Extensions

- Tool calls name Codex's model-facing tool in `_meta.codex.toolName`
  (`exec_command`, `write_stdin`, `view_image`, `request_permissions`, or a
  namespaced dynamic tool). ACP v1 has no tool name field.
- File diffs carry AIR line counts in `_meta.jetbrains.air.diffStats`. Counts
  come from the patch and are omitted when its hunks are inconsistent.
- Clients that negotiate AIR `recommendedValue` in
  `clientCapabilities._meta.jetbrains.air` receive the catalog default model and
  the current model's default effort as recommendations. GPT model names are
  shortened for pickers, for example `GPT-5.3-Codex` becomes `5.3 Codex`.
- `_meta.mcpStartupAwaitTimeoutMs` on `session/new`, `session/resume`, and
  `session/fork` waits up to that many milliseconds for the requested MCP
  servers in that session to report `ready`, `failed`, or `cancelled`.
- Codex `request_user_input` questions become one form elicitation for clients
  with form support; other clients return no answers. An "Other" question gets
  a "None of the above" choice and a note field whose answer is sent as
  `user_note: …`.
- Authentication failures (`unauthorized` or HTTP 401) end the prompt with ACP
  `auth_required` instead of a chat message.

## Plans

This bridge currently does not forward `turn/plan/updated` task lists as ACP v1
`plan` notifications. A completed Codex `plan` item is surfaced as agent text and
offered to the client via `requestPlanImplementation`. Approving implementation
starts a separate backend turn with its own event and approval lifecycle.

## Live regression tests

`live_regressions_test.go` drives the installed Codex app-server through ACP
without making model requests. It checks that an unprompted session retains
Plan mode on resume and load, and that an MCP startup wait only completes for
its own session. The MCP test starts two real stdio servers with independently
controlled handshakes and observes the CLI's startup notifications.

Run from the repository root:

```sh
CODEX_ACP_LIVE=1 go test ./pkg/acp/codex -run '^TestLiveCodex(UnpromptedPlanMode|MCPStartupSessionIsolation)$' -count=1 -v -timeout=2m
```
