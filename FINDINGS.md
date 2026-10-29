# Review: `feature/ahp` protocol changes

This reviews the switch from the custom session, terminal, LSP and debug routes to AHP (`/ahp`), LSP (`/lsp`) and DAP (`/dap`) WebSockets. It covers correctness and simplification.

Line numbers refer to commit `834072d`.

**Checks that pass at `834072d`:**
- `go test -race ./server ./pkg/dap ./pkg/terminal`
- `npm --prefix server/ui run test:unit` (117/117)
- `tsc -b --noEmit`

**How each finding was checked:**
- **probe**: reproduced with a throwaway Go test against the real host.
- **code**: confirmed by reading the code path end to end.
- **agent**: reported by a review agent and not independently re-checked.

## Bugs

### Critical

#### 1. Any website can run commands on the user's machine (probe)

- **Cause:** `/ahp` (`server/ahp.go:168`) and `/lsp` / `/dap` (`server/editor_transport.go:26`) use `InsecureSkipVerify`, so they accept any origin. On main, both browser sockets used coder/websocket's default same-origin check.
  - `LocalHostOnly` doesn't help: it validates `Host`, which stops DNS rebinding but not a cross-origin `new WebSocket("ws://localhost:PORT/ahp")`.
  - `CrossOriginProtection` lets GET upgrades through.
- **Probe:** a connection with `Origin: https://evil.example` and `Sec-Fetch-Site: cross-site` sent `initialize`, then `createTerminal`, then `terminal/input "touch …"`. The file was created on the host.
- **Instance check bypass:** the root snapshot exposes `instanceId` in `_meta.wingman` (`server/ahp.go:518`). The `?instance=` check on `/lsp` and `/dap` therefore doesn't stop a foreign page.
- **Debug plans:** `/dap` `launch` runs any client-supplied `_wingmanPlan.prelaunch`. `validateDebugPlan` (`server/handler_debug.go:984`) only checks that the command is an absolute, existing path. The server keeps no record of the plan it produced.
- **Fix:**
  - Accept only these origins:
    - the same origin,
    - no `Origin` header (non-browser clients such as VS Code's Node side),
    - `vscode-*` origins.
  - Reject every other http(s) origin.
  - Drop `InsecureSkipVerify` on `/lsp` and `/dap`; only the browser uses them.
  - Keep reviewed debug plans on the server and refer to them by id.
- **Test:** `TestAHPAcceptsAnyOrigin` currently asserts the vulnerable behaviour.

### High

#### 2. Long terminal output keeps resetting the browser terminal (code)

- **Cause:** once retained output passes 320 KiB, `server/ahp_terminal.go:228` sends `terminal/cleared` followed by a 256 KiB `terminal/data`. `TerminalView.tsx:85` handles `terminal/cleared` with `term.reset()`.
- **Effect:** a long-running dev server, build log or `htop` is wiped about every 64 KiB.
  - It loses alt-screen, application-cursor and mouse modes.
  - The replay starts at an arbitrary byte, possibly mid-escape-sequence.
  - Each reset sends 256 KiB to every subscriber.
- **Fix:** trim the host's snapshot without broadcasting `cleared`, and bound the client's reduced content separately.

#### 3. The host refuses all new clients after 1024 browser page loads (probe)

- **Cause:** `h.receipts` never removes an identity (`server/ahp.go:424`), and every page load creates a new `clientId` (`ahpConnection.ts:55`). Each ledger also keeps up to 256 envelopes, including base64 images.
- **Probe:** the 1025th sequential `initialize` fails with "too many AHP client identities; restart the server". The browser then retries every second forever.
- **Fix:** see simplification B, or evict ledgers of disconnected clients after a TTL.

### Medium

#### 4. The agent gets stale diagnostics for files open in the browser (code)

- **Cause:** `textDocument/didOpen` now syncs with `saved=false` (`server/lsp_transport.go:285`). Main passed `true` for open.
- **Mechanism:** `lsp.Session.OpenDocument` skips reading from disk for unsaved documents, and `PostEditDiagnostics` relies on that.
- **Effect:** after the agent edits a file that is open but unedited in the browser, its diagnostics come from the old browser buffer. Every reconnect re-opens that window.
- **Fix:** derive `saved` by comparing the `didOpen` text with the file on disk. Reconnect legitimately restores unsaved buffers through `didOpen`, so always passing `true` would be wrong.

#### 5. A 2-minute deadline on every editor request (code + agent)

- **Cause:** `editorSocket.begin` (`server/editor_transport.go:55`) applies the deadline to all `/lsp` and `/dap` requests.
- **Debug launch:** pre-launch builds may take 5 minutes (`pkg/dap/prelaunch.go:22`). A slow cold `cargo build` or `dotnet build` (over 2 minutes) now fails.
- **Edit review:** `workspace/executeCommand` waits inside `applyEdit` for the browser's confirmation dialog. After 2 minutes it errors, the remaining edits are dropped, and a later Accept still applies the edit.
- **Related:** declining the preview now surfaces "workspace edit was not applied" as an error toast (`monacoLsp.ts:231`). Previously it quietly returned `false`.

#### 6. A session that fails to load shows "loading" forever (code)

- **Cause:** `x-wingman/resolveSession` cannot fail, so every load error appears at `subscribe`.
  - `AHPConnection.attach` sends that error only to `read()` callers (`ahpConnection.ts:300`); `watch()` listeners never hear about it.
  - Affected errors: unknown or disposed session, backend load failure.
- **Effect:** the view stays `status: "loading"`, and after a reconnect it stays unsynchronized with no message. On main, `session.error` set `status: "error"`.

#### 7. Deleting a session elsewhere never closes its tab (code)

- **Cause:** `server/ahp_session.go:121` only sends `root/sessionRemoved`. No update follows on the session channel, so the session never reaches status `deleted`.
- **Effect:** the App effect that closes deleted tabs (`App.tsx:1942`) never fires. A session deleted from VS Code or another window stays open with stale content.

#### 8. A retried send can be delivered twice (code)

- **Cause:** the send fingerprint now includes `intent` (`workspaceClient.ts:380`). Main deliberately left it out and had a test for it, which was removed.
- **Scenario:**
  1. The user sends while a turn is running (intent `steer`).
  2. The connection drops after the host accepted the message.
  3. The turn finishes.
  4. The user retries; the intent is now `follow_up`, so the client uses a new id and the message runs a second time.

#### 9. A retry with its original `clientSeq` is rejected (code)

- **Cause:** the client reuses the old `clientSeq` when retrying an unconfirmed action (`ahpConnection.ts:434`). The host rejects any sequence at or below the last one it saw if it isn't in the ledger (`server/ahp_actions.go:45`).
- **Effect:** if the original frame never arrived and any newer action was accepted after reconnect, the retry fails with "clientSeq is older than the retained receipt window". `TerminalView` sends a resize on every snapshot, so a newer action is likely.
- **Also:** terminal keystrokes share the 256-entry window, so typing 256 characters ends de-duplication for an earlier send.

#### 10. One slow request stalls the whole connection (code)

- **Cause:** `handleAHP` runs each request and dispatch to completion before reading the next message.
- **Slow examples:**
  - `createSession` or `resolveSessionConfig` starting an external agent CLI,
  - `listSessions` calling every backend,
  - `ensureSession` loading an external session,
  - the first changeset `git diff`,
  - a large resource read or watch scan.
- **Effect:** while one runs, that tab's terminal input and chat actions are stuck, with no timeout.
- **Fix:** handle slow requests concurrently; keep per-channel ordering only for dispatches.

#### 11. LSP locks are held while a language server starts (agent)

- **Cause:** `c.mu` and the server-wide `s.lspDocumentsMu` are held across `SyncEditorDocument` (`server/lsp_transport.go:231-285, 644-665`). That call waits without a timeout for the language server to start.
- **Scenario:** opening a Java file during a cold jdtls start (10–30 s).
  - Document sync blocks on every connection.
  - Cancelled requests pile up until "too many outstanding requests".
  - After 128 queued messages, the connection is dropped.
- **Fix:** update the maps under the lock, and call the language service after unlocking.

#### 12. Debug output problems (agent)

- **Duplicated after reconnect:** on its first pass, `observe()` (`server/dap_transport.go:479`) sends the whole buffer with `_wingmanReplace: false`. The client keeps its old output, so old and full output are concatenated.
- **Resent in full past 64 KiB:** front-trimming means the previous text is never a prefix of the new text, so each new line resends about 64–128 KiB. Each `output` event also triggers a `wingman/status` refetch that returns the full output again. A chatty debuggee can fill the outbox and get the socket dropped.
- **Fix:** see "Send debug output by byte offset" under Protocol and client.

#### 13. Terminal lifecycle regressions (code)

- **Exit:** a shell `exit` no longer closes its tab. `TerminalView` drops `onExit`, so `closeTabNow` and `handleDebugTerminalExit` (switching the debug view to output) are never called.
- **Accumulation:** `terminal.Manager.handleExit` no longer removes exited sessions, and creation is capped at 128 (`MaxSessions`).
  - Each debug `runInTerminal` leaves a retained terminal.
  - Exited debug terminals pile up until terminal creation and debug launches fail with "terminal limit reached".

### Low

- **False error on Stop (code):** the host only confirms a cancel when the turn ends as Cancelled (`server/ahp_actions.go:80`, `ahp_session.go:246`).
  - If the turn completes or errors first, the client shows "Action delivery unconfirmed" after 30 s.
  - A retry gets no reply either, and the pending entry is never freed.
- **Stop during compaction does nothing (agent):** there is no `activeTurn`, so `cancel` sends no action and reports "accepted" (`workspaceClient.ts:294`). On main, it interrupted compaction.
- **Full transcript rebuild per tool-argument delta (probe):** every `entry.upsert` rebuilds the whole transcript and deep-compares it, under the host's single lock.
  - This includes every streamed tool-argument delta.
  - At 400 turns with 2,800 entries, that's about 6 ms per delta, and it blocks every session and terminal meanwhile.
- **Opening a session bumps its modified time (code):** `ensureSession` attaches with `time.Now()`, so opening an old session shows it as "just now" and moves it to the top of the list. Turns loaded from history also get the attach time as `startedAt`.
- **Terminal ownership gaps (agent):**
  - After a successful claim, no resize is sent, so host-created terminals keep the default size.
  - The host never announces that an owner disconnected, so a second window stays read-only.
  - A tab whose terminal is owned by another open window can't be closed.
- **Prompts lose detail (agent):** `default`, `enum_previews` and `custom_answer_for` are not carried across AHP, but `PromptBar` uses them.
- **Stack-frame path matching (agent):** the client matches with a plain `startsWith` (`dapClient.ts:61`). Main resolved symlinks and used `filepath.Rel`. This breaks for roots under `/tmp` or `/var` on macOS and for Windows drive-letter case.
- **Parent debug output not watched (agent):** with a child session attached, `Session.Changes()` watches only the child's output, while `Output()` combines parent and child.
- **Unconfirmed actions pile up (agent):** they stay in `AHPConnection.actions` after timeouts and disconnects. After 256, every dispatch fails, and reconnecting doesn't clear them.
- **Minor protocol details (agent):**
  - A cancelled DAP request replies with `message: "context canceled"`; the spec says `"cancelled"`.
  - LSP requests after `shutdown` return -32002 instead of -32600.

## Simplifications

### Architecture

**A. The data makes a double round trip.** The path today:

1. a frame from the agent,
2. a `TranscriptEntry`,
3. AHP state, built by diffing (`transcript`, `syncParts`, `syncTool`),
4. the client's reducers,
5. a `ChatEntry` (`AHPSessionProjection`).

`c.entries` is now only used to build the AHP state. Image handling shows the cost: images are encoded into markdown on the server and regex-parsed back out on the client.

- Emitting AHP actions straight from `sessionController.apply` would remove about 400 lines of diffing and fix the rebuild cost.
- A cheaper step: handle a single-entry update in the active turn directly, and rebuild only on load or reset.

**B. Drop the host's per-client receipt ledger.**

- **Idea:** derive command IDs from the IDs that actions already carry: turn ID, pending-message ID, request ID and tool-call ID. The controller's receipts already de-duplicate those. Title, config and archive changes are naturally idempotent, and terminal input is never replayed.
- **What goes away:** `ahpClientReceipts`, `ahpActionReceipt`, the fingerprints, and the 1024 and 256 caps. This fixes bugs 3 and 9.
- **Tradeoff:** a duplicate start-turn would get a rejection instead of the original confirmation, unless the controller's receipt is checked first.

**C. Remove about 1,100 lines of dead HTTP LSP and debug handlers.**

- Dead code:
  - `handler_lsp.go:187-598`,
  - `handler_lsp_edit.go:18-296`,
  - `handler_debug.go:326-793`.
- Only the test shim `server/scoped_test.go:18-91` reaches them, so `handler_lsp_test.go` and `handler_debug_test.go` test that dead path. That is how bugs 4 and the path-matching regression went unnoticed.
- Port the valuable cases to WebSocket tests.

### Protocol and client

- **Drop the `workspace/applyEdit` reverse request.** The host already answers the language server's own applyEdit (`pkg/lsp/session.go:163`), so the browser's reply never reaches it. Return the edits in the `executeCommand` result, as main did.
  - This removes `_wingmanRequestId`, the `replies` map and the client handler: about −60 lines.
  - It also removes the review deadline from bug 5.
- **Send debug output by byte offset.** Give the output buffer a running byte count and send `{offset, output}` events instead of full strings with `_wingmanReplace`.
  - `wingman/status` then returns output only on the first call.
  - `DebugOutputTab` can read `DAPClient.output` instead of refetching.
  - This fixes bug 12.
- **Remove settings from `_meta.wingman`.** `ahpRuntimeInfo.Settings` duplicates the AHP session config (schema and values). It is resent, with the full model list, on every tool-progress update.
- **Remove redundant notifications and dead fields:**
  - `EvtTerminalsChanged` in `debug_terminal.go` and `EvtSessionsChanged` in `createSession` are covered by AHP root notifications.
  - `sessionController.revision` and the JSON tags on `SessionChange` are unused.
  - The epoch in `x-wingman/*` parameters never changes within a server process.
- **Add an `onError` callback to `AHPConnection.watch`.** Route `attach` errors to it; this fixes bug 6 and makes the `channelErrors` plumbing unnecessary. `TerminalView`'s extra `read()` exists only to surface errors.
- **Return the already-parsed frame from the `recv` wrapper** (`ahpConnection.ts:158`). Every frame is currently parsed twice. A throw inside the wrapper also closes the socket, where the SDK's own path only warns.
- **Replace the string-typed `command()` switch with typed methods.** The switch in `workspaceClient.ts:250-372` translates the old HTTP commands, and the receipt it returns is made up. Typed methods (`cancel`, `respond`, `updateQueued`, …) remove the switch and the guessing of the message kind.
- **Stop duplicating server data on the client.** The semantic-token legend (`monacoLsp.ts:28-64`) copies `lsp_transport.go:22`; read it from the `initialize` result. The `languageId` table (`lspClient.ts:44`) is ignored by the server.
- **Use O(1) maps for the DAP reference table** (`dap_transport.go:145`). It does linear scans of up to 65k entries.
- **Replace the LSP method allowlist with a method→handler map.** The allowlist (`lsp_transport.go:319`) duplicates the dispatch switch, whose `default` already returns -32601.

### Host

- **Terminal content length:** keep a running count instead of re-joining all content parts on every output chunk (`ahp_terminal.go:222`).
- **Changeset polling:** feed the changeset from `checkWorkspace` instead of a separate 2 s `git diff` ticker. Today there are three workspace pollers: `checkWorkspace`, the changeset, and a 1 s full walk per resource watch.

## Decision needed

The web UI uses the HTTP file and diff routes, while VS Code uses AHP resources, resource watches and changesets. These are two implementations of the same functionality. They are worth keeping both only if VS Code integration is a goal.

## Suggested order

1. **Origin restriction (#1):** small and critical.
2. **Terminal trimming (#2).**
3. **Receipt redesign (B):** fixes #3 and #9.
4. **`didOpen` saved state (#4).**
5. **Client error and deletion handling (#6, #7, #8).**
6. **Remove dead handlers (C) and port their tests.**
7. **Projection rework (A).**
