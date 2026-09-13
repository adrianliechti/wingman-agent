# Review improvements

This change implements the combined September 2026 core, TUI, and GUI review,
including the independently checked findings from *Wingman Agent Review.html*.
The changes preserve user work and make control, recovery, and validation visible.

## Behavior

| Area | Result |
| --- | --- |
| File edits | Change notifications no longer acknowledge a new read. Background agents have independent read baselines. Multi-file edits validate every original before committing, check again during commit, serialize agent writes, and retain backups if an external change prevents rollback. |
| Tool output | File reads budget continuation notices inside the shared 48 KiB limit. Oversized results are persisted before harness truncation. UTF-8 boundaries are preserved. |
| Agent lifecycle | Stopping a stream consumer remains final even if journal recording fails. Post-tool failures preserve completed edit metadata. Real verification shell tools remain available under the execution policy. |
| Task limits and retries | Optional token and time allowances cover a built-in turn and its inline helpers. Retries show the reason, attempt, and delay; backoff honors Retry-After and remains cancellable. Terminal outcomes remain in the ledger. |
| Execution policy | Project/plugin MCP configurations require approval bound to their exact configuration and workspace before connection. Scheduled scripts use the shell environment and command approval policy, including legacy or edited schedules. Disabling the shell also disables scheduled scripts. |
| Local HTTP | Workspace and native launcher entry points check Host as well as browser origin. Authenticated relay transport has an explicit server-side exception. Forwarded headers cannot grant it. |
| TUI | Saved queues are visible and resumable. Drafts and attachments survive session switches/restarts. Backend settings and skill discovery run off the input loop. Status distinguishes waiting, retrying, paused queues, stopping, and unattended mode. Diff-pane preference persists. Shutdown restores terminal modes once, including on termination signals. Terminal control replies never become composer text. |
| GUI | Drafts and attachments persist in IndexedDB, with a synchronous recovery journal for pending writes during navigation. Closed drafts can be recovered. Open documents own Monaco models and undo/view state. Stop remains visible while typing, completing a turn or receiving a late send receipt preserves focus, and reconnects leave drafting and loaded editors usable. Approvals have deliberate keyboard focus and larger touch targets. |
| Review changes | Each built-in turn can show attributed file diffs, recorded validation commands/exit codes, and undo. Inline child reviews are included. Checks before the latest edits are marked outdated. Undo preserves the pre-turn contents, including uncommitted user work, and refuses changed files, discontinuous edit chains, symlinks, and uncertain outcomes. |
| Long sessions | Layout observes stable session summaries; mounted chats subscribe to their own views. Streaming render notifications are batched. Long transcripts and expanded tool output render bounded windows. Closed idle transcript caches are bounded. Monaco loads on demand. |

## Controls and recovery

- In the TUI, Enter steers an active turn, Alt+Enter queues the next task, and
  Shift+Enter inserts a newline. Ctrl+Q or `/queue` opens saved inputs, with Resume,
  edit, remove, and clear actions. `/queue resume` also works directly.
- Escape during work stops it while retaining paused follow-ups. Escape while
  idle preserves the composer; `/discard` clears it deliberately.
- Both interfaces preserve a separate composer draft while editing a queued
  message. Accepting or cancelling the queue edit restores that draft, including
  after a session switch or reload.
- In the GUI command palette, search for **Recover draft** to reopen a closed
  draft. Storage failures leave the draft in memory, show a retry action, and
  protect window closure if neither the regular save nor the navigation recovery
  journal can retain the pending data.
- **Alt+A** focuses an approval; **Y/N** then allow or decline it. Typing Y/N in
  the composer does not answer an approval. **Allow for session** states its scope.
- Use **Jump to latest** when reading earlier output. Tabs show pending input and
  newly completed work.
- Expand **Review changes** below a turn to inspect its diffs and validation.
  **Undo tracked edits** checks all affected files before changing any of them.

## Scope of guarantees

Review/undo covers built-in edit-tool records and synchronous child agents.
Commands, external tools, detached tasks, and native ACP backends can make changes
outside those records; the card discloses this scope. Historical sessions without
edit metadata do not acquire invented baselines. Undo only supports files inside
the workspace. Filesystem transactions are best effort across multiple files;
external programs do not participate in the agent's lock. If concurrent external
changes prevent rollback, the error identifies the retained backup.

Validation means that a command explicitly marked `validation: true` produced a
recorded exit code. Model prose, a detached command starting successfully, and a
check predating later edits do not prove current validation. This does not claim
that a passing check exhaustively verifies a task.

`WINGMAN_TASK_MAX_TOKENS` and `WINGMAN_TASK_TIMEOUT` configure built-in task
allowances. For example, `WINGMAN_TASK_MAX_TOKENS=200000` and
`WINGMAN_TASK_TIMEOUT=20m` limit a turn to 200,000 input-plus-output tokens or
20 minutes. Zero/unset means unlimited. Inline helpers share the parent allowance;
detached tasks get independent allowances. An in-flight response can overshoot the
token allowance. This is not a currency billing cap or a native ACP agent limit.

## Follow-up correctness review

- Cancelled file edits and undo requests check cancellation after waiting for
  transaction locks and between staging and commit steps. A stopped request
  cannot simply resume writing when another transaction releases the lock.
- Validation rejects missing/null exit codes, marks each stale check explicitly,
  and uses the latest attempt consistently across parent and inline child agents.
  Commands in different working directories remain separate checks. Polling
  retains the command's launch order, so a check started before edits stays stale
  and a late exit cannot overwrite a newer attempt. Results within the same
  ledger message retain their execution order. An exit without a recorded launch
  in the same agent turn remains unconfirmed.
- GUI recovery reads IndexedDB and navigation journals independently, skips
  malformed records, and gives saved revisions precedence over stale journals.
  Retiring a recovered tab identity advances its revision so old drafts cannot
  reappear after sending. Cleanup preserves journals changed by another window.
- TUI prompt receipts preserve the actual turn phase, including idle settings
  changes and stopping turns. Delayed prompt cleanup closes only its own popup.

Shared draft validation/revision helpers and one validation-check update path
replace duplicated logic. Regression tests cover the failure cases above,
including browser reloads with unavailable IndexedDB and damaged journals.

## Further correctness and simplification

- Each open editor document now has an identity independent of its filename.
  Renaming a file and reusing its old path keeps both buffers and undo stacks
  separate. Editor cleanup holds the model directly, removing path aliases and
  preventing stale cleanup after consecutive renames or a close/reopen.
- TUI questions share one setup/cleanup lifecycle. Switching sessions cancels
  the old question, restores its original draft, and leaves the new composer
  alone. Queued prompts and delayed approval receipts cannot cross session
  activations. Free-text questions suspend queue-edit controls and preserve the
  actual turn phase when answered.
- Large-file reads check cancellation while skipping lines and after reading
  the final requested line. Cancelled scans return no content. The scan accepts
  a reader separately from opening the file, so regression tests can cancel
  during I/O without timing assumptions or very large fixtures.

## Verification and measurements

The latest round passed the full Go suite, race detection in the changed
file-tool and TUI packages, the TUI integration suite, 99 frontend unit tests,
all 105 browser tests (including 16 recovery regressions), lint, and the
production build. Earlier review rounds also passed broader race checks across
the agent and server. The original terminal fuzz run exercised
4,143,990 inputs in its 15-second run without a failure.

The checked-in regressions include stale-write notification interactions,
background isolation, concurrent transaction preconditions, multibyte read
continuations, recorder failures after consumer closure, schedule/MCP approval
boundaries, token/time cancellation, interrupted prompts, queue recovery, draft
save failures and late receipts, journal reload and safe undo, session stream
faults, and terminal parser fuzzing.

The browser suite includes desktop/mobile Stop, editor focus and undo, reload
with image attachments, recovering closed drafts, deferred Monaco loading,
1,000-turn history, conflicting turn undo, disconnected file editing, deliberate
approval shortcuts, touch targets, queue-edit recovery, and large tool output.
It also retains the existing editor, debugger, terminal, relay, and session tests.

On an Apple M4, the reproducible session-store benchmark (50 sessions × 4,000
entries, 1,000 deltas) measured 53.3 ms for the previous store and 49.5 ms for the
new one. Render notifications fell from 1,000 to 1. This measures store work and
notifications, not end-user latency. Run `node bench/sessionStore.ts` from
`server/ui`; its optional argument accepts an older store for comparison.

The task activity microbenchmark (`BenchmarkTaskActivity`, 1,000 iterations)
measured approximately 4.79 ms per update previously versus 5 ns for the new
in-memory update. Transient activity no longer rewrites and fsyncs the registry;
the next durable task transition still includes the current activity.

The rebuilt entry chunk is about 1.06 MB, down from about 1.28 MB; the roughly
3.91 MB editor chunk is now deferred. A browser network regression verifies that
an empty chat does not fetch it and opening a file does.

Run checks from the repository root:

```sh
go test -timeout=10m ./...
go test -race -timeout=10m ./pkg/agent/... ./pkg/code/... ./pkg/tui/... ./pkg/remote ./server
go test -tags=e2e -run TestTUIE2E -count=1 ./pkg/tui/code
go test -tags=e2e -run TestWebUIE2ECodingAgentWorkflows -count=1 -timeout=15m ./server
go test ./pkg/tui/inline -run '^$' -fuzz FuzzTerminalControlReply -fuzztime=15s
```

From `server/ui`, run `npm run test:unit`, `npm run lint`, and `npm run build`.
Build before browser tests because Go embeds `server/static`. The browser fixture
uses a local fake model, Chromium (`npx playwright install chromium`), and a local
`gopls`. `E2E_GREP='recovery:'` selects the focused regressions; `E2E_STATIC_DIR`
can point at a separate build directory. No real model account is required.

These are local checks; this checkout does not contain a CI workflow. Native
interactive behavior across terminal emulators and Windows desktop execution
still require platform checks; Windows CLI and terminal test binaries are
cross-compiled locally.
