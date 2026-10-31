# Review: `feature/ahp` protocol changes

This reviews the switch from the custom session, terminal, LSP and debug routes to AHP (`/ahp`), LSP (`/lsp`) and DAP (`/dap`) WebSockets. Only open findings are listed; fixed ones have been removed. The LSP and DAP gateways were checked against LSP 3.18 and DAP 1.71.

**Checks that pass:**
- `go test -race ./server ./pkg/...`, including the TypeScript client integration (17 tests)
- `go test -tags=e2e -run TestWebUIE2E ./server` (116 Playwright tests)
- `npm --prefix server/ui run test:unit` (188/188)
- `tsc -b --noEmit` and `oxlint src test e2e bench`
- `npm --prefix server/ui run build`

## Review of `8529846c`

A review in five areas (sessions and chats, workspace and transport, LSP, DAP, web UI) added 137 Go tests (`TestReview<Area>…` in `review_*_test.go`) and 45 UI tests (`server/ui/test/review*.test.ts`): concurrency under `-race`, hostile input and protocol edge cases. It found 52 bugs, three of them High: discarding a change wrote through a symlinked parent directory, a stop that lost the race to the turn's end cancelled the next turn, and two connections editing one file shared a language-server document. All 52 are fixed and their tests now guard against regressions. `TestReviewWorkspaceSearchDeadlineBoundsStalledClient` takes a minute; `-short` skips it.

**Still open:**
- **Sessions:** forking through a compaction turn that isn't the chat's latest forks through the turn before it, without the compaction, because the journal records no input for it. Clients subscribed to a chat channel keep its old title until they resubscribe, since AHP has no chat rename action. Background-task turns stay separate only for the host process; after a restart they merge into the previous turn.
- **LSP:** a read-only request (hover, completion) can still be answered against another connection's text if that connection changes the file mid-request; edit results are refused with -32801 in that case.
- **DAP:** with `terminateDebuggee: false`, the process-group kill after the adapter exits can still end a launched debuggee. `_wingmanPlan.configuration` can carry adapter options that run commands (`runtimeExecutable`, `initCommands`); not an escalation, since the same page has terminals.
- **Terminals:** debugger terminals in `pkg/dap` can't yet tell a cut-off output stream from a normal end; `terminal.Session.SubscribeAt` and `Written` now make that possible.
- **Resources:** a regular file swapped for a FIFO between the check and the open can still block `resourceRead`.
- **Unverified:** `ahpHost.fileURI` likely produces unusable `file:` URIs on Windows (`file://C:/…`), and a background job holding a PTY open may hang its close on Linux.

## AHP 1.1 adoption

The pin is upstream `d7a5579b` (Go module, vendored TS SDK, `scripts/vendor-ahp.mjs`, `docs/ahp.md`).

**Adopted:**
- **Reasoning timing.** The host measures each reasoning part from its first text to the next part or the turn's end. Timing lasts for the host process; history from earlier processes stays untimed.
- **Models:** `vendor`, the token limits, and a per-model `effort` schema.
- **Chats:** per-chat config (`chatMutable`, `chat/configChanged`), forked chats with `origin`, `disposeChat`, and chat `activity`.
- **Already present:** mixed steering/queued pending messages, changeset file nonces and `isEditing`.

**Not adopted:**
- **Side chats and chats from other providers:** no backend support.
- **Experimental:** windowed delivery and TCP channels.
- **No source data:** `ToolCallResult.resultMetadata` and MCP auth targets.

## Known limitations

- **Forking from an earlier turn needs the builtin agent.** It cuts the native journal at the next input; other agents can only fork their whole idle history. A turn that a later steering message joined is cut at that message and loads as interrupted.
- **Compaction turns last for the host process.** The agent's history has no record of them, so after a restart the `/compact` turn and its notice are gone (the compacted context remains).
- **Approvals are answered once.** The confirm callback has no session-wide scope, so the host offers only Allow and Deny.
- **Chat URLs are long.** Web UI routes carry the encoded chat URI, e.g. `/wingman/ahp-chat%3A%2Fwingman%2F…`.
- **No refresh on window focus.** `x-wingman/focus` is gone; file changes appear on the next workspace check (at most 2 s after activity, otherwise 30 s).
- **Branch-only changes can show late.** Git status is refreshed from changeset updates and the UI's own Git actions, so an external checkout that changes no files shows when Git status is next fetched.
- **Agent pickers start empty.** Providers come from the AHP root, so they appear once the connection is up.

## Simplifications

**A. The data still makes a double round trip.** Agent frames become `TranscriptEntry`s, then AHP state built by diffing (`transcript`, `syncParts`, `syncTool`), then the client's reducers. Text, reasoning and tool updates in the active turn now take fast paths; other changes still rebuild the transcript under the host lock. Emitting AHP actions straight from `sessionController.apply` would remove the diffing.

**Workspace polling.** Feed the changeset from `checkWorkspace` instead of a separate 2 s `git diff` ticker. The language-server file scan already runs inside `checkWorkspace`, while each AHP resource watch walks its tree every second. Both use `filewatch.Scanner`, so one shared `filewatch.Source` could serve them.
