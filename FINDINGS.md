# Review: `feature/ahp` protocol changes

This reviews the switch from the custom session, terminal, LSP and debug routes to AHP (`/ahp`), LSP (`/lsp`) and DAP (`/dap`) WebSockets. Only open findings are listed; fixed ones have been removed. The LSP and DAP gateways were checked against LSP 3.18 and DAP 1.71.

Line numbers refer to commit `5737bcc` unless noted and may have drifted.

**Checks that pass:**
- `go test -race ./server ./pkg/lsp ./pkg/language ./pkg/code ./pkg/dap ./pkg/terminal ./pkg/debugadapter`
- `npm --prefix server/ui run test:unit` (116/116)
- `tsc -b --noEmit`
- `npm --prefix server/ui run build`

**How each finding was checked:**
- **probe**: reproduced with a throwaway Go test against the real host.
- **code**: confirmed by reading the code path end to end.
- **agent**: reported by a review agent and not independently re-checked.

## AHP 1.1 adoption

The pin is upstream `d7a5579b` (Go module, vendored TS SDK, `scripts/vendor-ahp.mjs`, `docs/ahp.md`).

**Adopted:**
- **Reasoning timing.** The host is the producer, so it measures each reasoning part.
  - **Start:** the part's first non-empty text.
  - **End:** the next markdown, tool or reasoning part, or the turn ending. The final duration is sent before the turn's terminal action.
  - **History:** timing lasts for the host process; history from earlier processes stays untimed, as the spec allows.
  - **Web UI:** shows "Thought for Ns".
- **`SessionModelInfo.vendor`** comes from the model's namespace, together with the 1.0 token limits.

**Already present:** mixed steering/queued pending messages, changeset file nonces and `isEditing`.

**Not adopted:**
- **Not useful with one chat per session:** per-chat config (`chatMutable`), initial chat titles, `chat/moved`, mixed chat providers.
- **Experimental:** windowed delivery and TCP channels. Windowed delivery is the long-term fix for slow subscribers (#10).
- **No source data:** `ToolCallResult.resultMetadata` and MCP auth targets.

## Bugs

### High

#### 2. Long terminal output keeps resetting the browser terminal (code)

- **Cause:** once retained output passes 320 KiB, `server/ahp_terminal.go:228` sends `terminal/cleared` followed by a 256 KiB `terminal/data`. `TerminalView.tsx` handles `terminal/cleared` with `term.reset()`.
- **Effect:** a long-running dev server, build log or `htop` is wiped about every 64 KiB.
  - It loses alt-screen, application-cursor and mouse modes.
  - The replay starts at an arbitrary byte, possibly mid-escape-sequence.
  - Each reset sends 256 KiB to every subscriber.
- **Fix:** trim the host's snapshot without broadcasting `cleared`, and bound the client's reduced content separately.

#### 3. The host refuses all new clients after 1024 browser page loads (probe)

- **Cause:** `h.receipts` never removes an identity (`server/ahp.go:432`), and every page load creates a new `clientId` (`ahpConnection.ts`). Each ledger also keeps up to 256 envelopes, including base64 images.
- **Probe:** the 1025th sequential `initialize` fails with "too many AHP client identities; restart the server". The browser then retries every second forever.
- **Fix:** see simplification B, or evict ledgers of disconnected clients after a TTL.

### Medium

#### 6. A session that fails to load shows "loading" forever (code)

- **Cause:** `x-wingman/resolveSession` cannot fail, so every load error appears at `subscribe`.
  - `AHPConnection.attach` sends that error only to `read()` callers; `watch()` listeners never hear about it.
  - Affected errors: unknown or disposed session, backend load failure.
- **Effect:** the view stays `status: "loading"`, and after a reconnect it stays unsynchronized with no message.

#### 7. Deleting a session elsewhere never closes its tab (code)

- **Cause:** `server/ahp_session.go:121` only sends `root/sessionRemoved`. No update follows on the session channel, so the session never reaches status `deleted`.
- **Effect:** the App effect that closes deleted tabs never fires. A session deleted from VS Code or another window stays open with stale content.

#### 8. A retried send can be delivered twice (code)

- **Cause:** the send fingerprint includes `intent` (`workspaceClient.ts`). Main deliberately left it out and had a test for it, which was removed.
- **Scenario:**
  1. The user sends while a turn is running (intent `steer`).
  2. The connection drops after the host accepted the message.
  3. The turn finishes.
  4. The user retries; the intent is now `follow_up`, so the client uses a new id and the message runs a second time.

#### 9. A retry with its original `clientSeq` is rejected (code)

- **Cause:** the client reuses the old `clientSeq` when retrying an unconfirmed action. The host rejects any sequence at or below the last one it saw if it isn't in the ledger (`server/ahp_actions.go:45`).
- **Effect:** if the original frame never arrived and any newer action was accepted after reconnect, the retry fails with "clientSeq is older than the retained receipt window". `TerminalView` sends a resize on every snapshot, so a newer action is likely.
- **Also:** terminal keystrokes share the 256-entry window, so typing 256 characters ends de-duplication for an earlier send.

#### 10. One slow request stalls the whole connection (code)

- **Cause:** `handleAHP` runs each request and dispatch to completion before reading the next message.
- **Slow examples:** `createSession` or `resolveSessionConfig` starting an external agent CLI, `listSessions` calling every backend, `ensureSession` loading an external session, the first changeset `git diff`, a large resource read or watch scan.
- **Effect:** while one runs, that tab's terminal input and chat actions are stuck, with no timeout.
- **Fix:** handle slow requests concurrently; keep per-channel ordering only for dispatches.

#### 13. A shell `exit` no longer closes its terminal tab (code)

- **Cause:** `TerminalView` never calls its `onExit` prop, so `closeTabNow` and `handleDebugTerminalExit` (switching the debug view to output) never run.
- **Also:** `terminal.Manager.handleExit` keeps exited sessions, and creation is capped at 128 (`MaxSessions`). Exited debugger terminals are now removed, but exited user shells still count toward the cap.

### Low

- **False error on Stop (code):** the host only confirms a cancel when the turn ends as Cancelled (`server/ahp_actions.go:80`, `ahp_session.go:246`). If the turn completes or errors first, the client shows "Action delivery unconfirmed" after 30 s; a retry gets no reply either, and the pending entry is never freed.
- **Stop during compaction does nothing (agent):** there is no `activeTurn`, so `cancel` sends no action and reports "accepted". On main, it interrupted compaction.
- **Full transcript rebuild per tool-argument delta (probe):** every `entry.upsert` rebuilds the whole transcript and deep-compares it under the host's single lock. At 400 turns with 2,800 entries, that's about 6 ms per delta, blocking every session and terminal meanwhile.
- **Opening a session bumps its modified time (code):** `ensureSession` attaches with `time.Now()`, so opening an old session moves it to the top of the list. Turns loaded from history also get the attach time as `startedAt`.
- **Terminal ownership gaps (agent):** after a successful claim no resize is sent, so host-created terminals keep the default size; the host never announces that an owner disconnected, so a second window stays read-only; a tab whose terminal is owned by another open window can't be closed.
- **Prompts lose detail (agent):** `default`, `enum_previews` and `custom_answer_for` are not carried across AHP, but `PromptBar` uses them.
- **Unconfirmed actions pile up (agent):** they stay in `AHPConnection.actions` after timeouts and disconnects. After 256, every dispatch fails, and reconnecting doesn't clear them.
- **`reconnect` from a client this process hasn't seen gets the 1.0 projection (code):** `c.version` comes from the in-memory ledger (`ahp.go:442`), so after a restart steering messages disappear from a 1.1 client's snapshot. Reject `reconnect` for unknown client IDs so the client re-initializes.
- **Wrong AHP error codes (code):** a duplicate terminal returns -32003 `SessionAlreadyExists` instead of -32010 `AlreadyExists`; an unknown provider returns -32603 instead of -32002 `ProviderNotFound`; a failed git operation returns -32011 `Conflict` even when nothing conflicts.
- **Unknown session URIs leave controllers behind (code):** `ensureSession` calls `b.session(id)` before loading, so arbitrary `ahp-session:/wingman/…` subscriptions grow `b.sessions` without bound.
- **Language server messages wait during a command edit review (code):** the native `workspace/applyEdit` handler blocks the jsonrpc2 reader until the user decides. `jsonrpc2.Async` would release it but closes the stdio stream (probe), so diagnostics and other responses from that server queue until the review ends.
- **`workspace/textDocumentContent` is registered for the `file` scheme (agent):** the 3.18 provider is meant for virtual schemes; other clients may refuse it. Native virtual schemes (jdtls `jdt://`) are not proxied.
- **Debugging several child sessions (code):** `startDebugging` children are kept, but inspection reaches only the focused child and threads are not merged across children.

## Simplifications

### Architecture

**A. The data makes a double round trip.** Agent frames become `TranscriptEntry`s, then AHP state built by diffing (`transcript`, `syncParts`, `syncTool`), then the client's reducers, then a `ChatEntry`. Images are encoded into markdown on the server and regex-parsed back out on the client.
- Emitting AHP actions straight from `sessionController.apply` would remove about 400 lines of diffing and fix the rebuild cost.
- A cheaper step: handle a single-entry update in the active turn directly, and rebuild only on load or reset.

**B. Drop the host's per-client receipt ledger.**
- **Idea:** derive command IDs from the IDs that actions already carry: turn ID, pending-message ID, request ID and tool-call ID. The controller's receipts already de-duplicate those. Title, config and archive changes are naturally idempotent, and terminal input is never replayed.
- **What goes away:** `ahpClientReceipts`, `ahpActionReceipt`, the fingerprints, and the 1024 and 256 caps. This fixes bugs 3 and 9.
- **Tradeoff:** a duplicate start-turn would get a rejection instead of the original confirmation, unless the controller's receipt is checked first.

**C. Remove the remaining dead HTTP handlers.** The debug handlers (`handleDebug*` in `server/handler_debug.go`) and the file handlers (`server/handler_files.go`) are reachable only through the test shim `server/scoped_test.go`, so their tests exercise a path the UI no longer uses. Port the valuable cases to WebSocket tests, as was done for LSP.

### Protocol and client

- **Remove settings from `_meta.wingman`.** `ahpRuntimeInfo.Settings` duplicates the AHP session config (schema and values) and is resent, with the full model list, on every tool-progress update.
- **Remove redundant notifications and dead fields:** `EvtTerminalsChanged` in `debug_terminal.go` and `EvtSessionsChanged` in `createSession` are covered by AHP root notifications; `sessionController.revision` and the JSON tags on `SessionChange` are unused; the epoch in `x-wingman/*` parameters never changes within a server process.
- **Add an `onError` callback to `AHPConnection.watch`.** Route `attach` errors to it; this fixes bug 6 and makes the `channelErrors` plumbing unnecessary.
- **Return the already-parsed frame from the `recv` wrapper** in `ahpConnection.ts`. Every frame is currently parsed twice, and a throw inside the wrapper closes the socket where the SDK's own path only warns.
- **Replace the string-typed `command()` switch with typed methods** in `workspaceClient.ts`. Typed methods (`cancel`, `respond`, `updateQueued`, …) remove the switch and the guessing of the message kind.

### Host

- **Terminal content length:** keep a running count instead of re-joining all content parts on every output chunk (`ahp_terminal.go:222`).
- **Workspace polling:** feed the changeset from `checkWorkspace` instead of a separate 2 s `git diff` ticker. The language-server file scan already runs inside `checkWorkspace`, while each AHP resource watch walks its tree every second. Both use `filewatch.Scanner`, so one shared `filewatch.Source` could serve them.
- **Native file watching:** a 100,000-file tree takes about 200 ms per scan on macOS. A native `filewatch.Source` would avoid the walk without cgo: `ReadDirectoryChangesW` on Windows and inotify on Linux need only `x/sys`, but FSEvents on macOS needs a purego binding, because kqueue (what `sgtdi/fswatcher` and fsnotify use without cgo) holds one file descriptor per watched file.

## Suggested order

1. **Terminal trimming (#2).**
2. **Receipt redesign (B):** fixes #3 and #9.
3. **Client error and deletion handling (#6, #7, #8).**
4. **Concurrent slow requests (#10).**
5. **Remove the remaining dead handlers (C).**
6. **Projection rework (A).**
