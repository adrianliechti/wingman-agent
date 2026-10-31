# Review: `feature/ahp` protocol changes

This reviews the switch from the custom session, terminal, LSP and debug routes to AHP (`/ahp`), LSP (`/lsp`) and DAP (`/dap`) WebSockets. Only open findings are listed; fixed ones have been removed. The LSP and DAP gateways were checked against LSP 3.18 and DAP 1.71.

**Checks that pass:**
- `go test -race ./server ./pkg/...`, including the TypeScript client integration (17 tests), except the `TestReview…` bug tests listed below
- `go test -tags=e2e -run TestWebUIE2E ./server` (116 Playwright tests)
- `npm --prefix server/ui run test:unit` (182/187: the 5 failures are the `review*` bug tests listed below)
- `tsc -b --noEmit` and `oxlint src test e2e bench`
- `npm --prefix server/ui run build`

## Review of `8529846c`: bugs with failing tests

Each bug has a test that fails today and passes once the bug is fixed; the review also added 117 passing hard tests (concurrency under `-race`, hostile input, protocol edge cases). Go tests are named `TestReview<Area>…` in `review_*_test.go`; UI tests are in `server/ui/test/review*.test.ts`. Run them with `go test -race -count=1 -short -run TestReview ./server ./pkg/...` (drop `-short` for the 75 s search-deadline test).

### High
- **Discarding a change writes outside the workspace.** `pkg/changes/changes.go` `restoreWorktreeFromIndex` and `remove` use plain `os` calls on `workingDir/path`, so a tracked directory replaced by a symlink lets `discard-changes` delete and overwrite the link's target. `TestReviewWorkspaceChangesetDiscardStaysInsideWorkspace`, `TestReviewWorkspaceChangesetRevertDoesNotWriteThroughSymlinkedParent`.
- **A stop that loses the race to the turn's end cancels the next turn.** The active-turn check runs before the `cancel` command, which carries no turn id and runs `CancelAll` on whatever is active then (`server/ahp_actions.go`, `server/runtime.go`); the client is told the stop failed while the next turn is cancelled and the queue cleared. `TestReviewSessionsLostCancelDoesNotStopTheNextTurn`.
- **Connections editing the same file share one language-server document.** Closing it in one tab leaves that tab's discarded text for the other, and edits computed for one tab's text are labelled with the other tab's version (`server/lsp_transport.go` `releaseDocument`, `pkg/lsp/session.go` `SyncRequestDocument`). `TestReviewLSPClosingSharedDocumentKeepsTheRemainingBuffer`, `TestReviewLSPRenameUsesTheRequestingConnectionsBuffer`.

### Medium
- **Sessions and chats** (`server/review_sessions_test.go`):
  - Data race: `syncChats` reads the default chat's controller settings under `ahpHost.mu` only (`ahp_session.go`). `…SessionActionsDoNotRaceSettingsRefresh` (`-race`).
  - Every turn completion drops the turn's duration and reloads the whole history, because `transcript()` runs before `finishTurn`. `…TurnCompletionKeepsDurationWithoutReload`.
  - Parts arriving after an answered approval truncate and replay the active turn: prompt parts are appended after the turn's entries. `…PartsAfterAnApprovalDoNotReplayTheTurn`.
  - A background-task turn renames the previous turn into the active one. `…TaskTurnKeepsThePreviousTurn`.
  - `chat/turnStarted` queues behind a recovered queue that the send resumes, so its acknowledgement waits. `…TurnStartedIsNotQueuedBehindARecoveredQueue`.
  - Questions raised outside a turn are attached to turn "" and never reach clients. `…QuestionOutsideATurnIsSurfaced`.
  - Text attachments are appended to the visible message text. `…AttachmentContentStaysOutOfTheMessageText`.
- **Workspace, terminals and transport** (`server/review_workspace*_test.go`, `pkg/…/review_workspace*_test.go`):
  - A search aborted through the relay keeps its slot until yamux's 5-minute close timeout; eight such aborts lock remote search out with 429. `…RelayAbortedSearchReleasesSlot`, `…RelayAbortReachesWorkspaceHandler`.
  - `disposeTerminal` waits forever behind an input write the program doesn't read. `…TerminalDisposeNotBlockedByStalledInput`.
  - A terminal's final output is lost when the process exits while the pump is a full scrollback behind. `…TerminalExitWhileBehindKeepsFinalOutput`.
  - One unreadable subdirectory stops `filewatch` and every resource watch. `…ResourceScannerSurvivesUnreadableDirectory`, `…ResourceWatchSurvivesUnreadableDirectory`.
  - An unterminated OSC escape holds back, then drops, later terminal output (xterm also ends OSC at ESC, CAN, SUB). `…TerminalFilterEndsUnterminatedOSC`.
  - `resourceRead` on a FIFO blocks a request slot forever. `…ResourceReadDoesNotBlockOnFIFO`.
- **LSP gateway** (`server/review_lsp_test.go`):
  - A language server that stops reading stalls every connection and the workspace poller: `lspDocumentsMu` is held while `CloseEditorDocument` waits on a blocked stdin write. `…WedgedServerDoesNotStallOtherConnections`.
  - About 130 keystrokes during a server's cold start overflow the queue and drop the connection. `…TypingDuringColdStartKeepsTheConnection`.
  - Completion items and code actions that carry a `command` object can't be resolved (-32602). `…ResolvesItemsThatCarryACommand`.
  - A document closed while its server was down is restored when the server restarts. `…DocumentClosedDuringRestartIsNotRestored`.
- **DAP gateway** (`server/review_dap_test.go`, `pkg/dap/review_dap_test.go`):
  - A child session ending reports the whole session terminated, and the real end later sends no `terminated`. `…ChildEndDoesNotTerminateBrowserSession`, `…EndedChildDoesNotTerminateRunningParent`.
  - A stale thread id of an ended child is routed to another child's thread. `…StaleChildThreadIsNotRoutedToAnotherChild`.
  - Sibling activity invalidates the references of a child that is still stopped. `…SiblingExitKeepsStoppedChildReferences`.
  - The parent's status takes the focused child's `request` and `terminal_id`. `…ParentStatusKeepsOwnRequestAndTerminal`.
  - Breakpoints changed while a child configures never reach it. `…BreakpointsChangedDuringChildStartReachChild`.
- **Web UI** (`server/ui/test/review*.test.ts`):
  - Watching a migrated legacy chat key again marks it deleted, which closes its tab. "a legacy key watched again after its redirect is redirected, not deleted".
  - A chat disposed while the client was disconnected shows a permanent error instead of closing. "a chat disposed while disconnected is reported deleted after reconnecting".

### Low
- **Sessions:** normalized config values diverge between host and clients (`…NormalizedConfigReachesSubscribers`); `createSession` can claim another session's canonical URI (`…CreateSessionCannotClaimACanonicalURI`); a fork's canonical URI redirects to the parent instead of the fork chat (`…CanonicalForkURIRedirectsToTheForkChat`); disposed sessions keep their alias (`…DisposeSessionForgetsItsAlias`); forking through a compaction turn fails (`…ForkThroughACompactionTurn`); a fork through an earlier turn carries a later `/compact` checkpoint (`…ForkThroughStopsBeforeALaterCompaction`); the default chat keeps "New session" (`…DefaultChatTakesTheSessionTitle`); command receipts are never evicted (`…CommandReceiptsAreBounded`).
- **Workspace:** idle comparisons can push the workspace changesets into error (`…ChangesetIdleComparisonsDoNotStarveWorkspaceLayers`); a stalled search client keeps its slot past the deadline (`…SearchDeadlineBoundsStalledClient`, 75 s); unknown fields in a changeset target, such as a misspelled `fileNonce`, are ignored (`…ChangesetRejectsUnknownTargetFields`); PTY resize/status race with close (`…TerminalResizeAndStatusRaceWithClose`, `-race`); `resourceMove` doesn't overwrite an existing directory (`…ResourceMoveOverwritesExistingDirectory`); case-only renames fail on case-insensitive file systems (`…ResourceCaseOnlyRename`).
- **LSP:** a command waiting behind another command's edit review can't be cancelled (`…CommandWaitingForAnotherReviewCanBeCancelled`); queued requests that are cancelled or time out answer -32603 instead of -32800/-32803 (`…QueuedRequestCancellationAnswersRequestCancelled`); the gateway answers a client's null-id response (`…DoesNotAnswerResponses`).
- **DAP:** exception filters changed during start are lost (`…ManagerAppliesExceptionFiltersChangedDuringStart`); closing the socket after `configurationDone` aborts the launch (`…SocketCloseAfterConfigurationKeepsLaunch`); breakpoints of a deleted file can't be cleared (`…ClearBreakpointsOfDeletedFile`); an explicit `terminateDebuggee: false` is dropped by `omitempty` (`…DisconnectKeepsLaunchedDebuggeeWhenAsked`); output offsets disagree with the text when a UTF-8 character is split across reads (`…OutputOffsetsMatchSplitRune`).
- **Web UI:** a dispatch racing `stop()` rejects with a TypeError ("a dispatch racing stop rejects with a disconnection error"); queue positions are 0-based and out of display order ("queued messages are numbered from one…"); `changeset/contentChanged` and `changeset/cleared` don't invalidate diff queries ("whole-list changeset actions invalidate their layer").

### Observed without a test
- The web UI's fork retry tolerates only -32003, but `createChat` answers a repeated chat URI with -32010, so a retried fork whose reply was lost shows an error although it was created.
- `_wingmanPlan.configuration` can carry adapter options that run commands (`runtimeExecutable`, `initCommands`); only `prelaunch` keeps commands host-owned. Not an escalation, since the same page has terminals.
- `ahpHost.fileURI` likely produces unusable `file:` URIs on Windows (`file://C:/…`); unverified, no Windows run.

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
