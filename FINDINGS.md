# Review: `feature/ahp` protocol changes

This reviews the switch from the custom session, terminal, LSP and debug routes to AHP (`/ahp`), LSP (`/lsp`) and DAP (`/dap`) WebSockets. Only open findings are listed; fixed ones have been removed. The LSP and DAP gateways were checked against LSP 3.18 and DAP 1.71.

**Checks that pass:**
- `go test -race ./server ./pkg/...`, including the TypeScript client integration (17 tests)
- `go test -tags=e2e -run TestWebUIE2E ./server` (116 Playwright tests)
- `npm --prefix server/ui run test:unit` (132/132)
- `tsc -b --noEmit` and `oxlint src test e2e bench`
- `npm --prefix server/ui run build`

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

## Simplifications

**A. The data still makes a double round trip.** Agent frames become `TranscriptEntry`s, then AHP state built by diffing (`transcript`, `syncParts`, `syncTool`), then the client's reducers. Text, reasoning and tool updates in the active turn now take fast paths; other changes still rebuild the transcript under the host lock. Emitting AHP actions straight from `sessionController.apply` would remove the diffing.

**Workspace polling.** Feed the changeset from `checkWorkspace` instead of a separate 2 s `git diff` ticker. The language-server file scan already runs inside `checkWorkspace`, while each AHP resource watch walks its tree every second. Both use `filewatch.Scanner`, so one shared `filewatch.Source` could serve them.
