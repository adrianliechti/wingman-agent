# Sessions and recovery

Wingman saves conversations automatically. A session belongs to the agent that
created it. Choose the same agent when reopening its conversation.

## Resume a conversation

In the terminal:

```bash
wingman --continue
wingman --resume <session-id>
wingman --agent codex --continue
```

In the browser, open the session list and select a saved conversation. Switching
the agent picker changes which sessions you browse and which agent a new chat
uses. Existing sessions keep their agent and continue running.

Closing a browser tab leaves its running work on the workspace server. Stopping
the workspace server ends that running work. Deleting a session cancels its
active turn and removes its saved history.

## Fork a conversation

Fork to try another direction without losing the original thread. The fork
copies the conversation, model, effort, and mode; background agents, schedules,
and queued follow-ups stay with the original. A session cannot be forked while
a turn is running; wait for it to finish or stop it first.

In the terminal UI, run `/fork [title]`. To fork when starting:

```bash
wingman --continue --fork
wingman --resume <session-id> --fork
```

In the browser, right-click a session in the session list and choose **Fork
session**, or search for **Fork Session** in the command palette.

## Steer work or queue a follow-up

While the agent is working, send a message to steer the current task, or queue
it to run afterward. Agents that cannot accept steering may queue the input.

In the terminal UI:

| Control | Action |
|---------|--------|
| `Enter` | Send a message or steer the active turn |
| `Alt+Enter` | Queue a follow-up |
| `Shift+Enter` or `Ctrl+J` | Insert a new line |
| `Ctrl+Q` or `/queue` | View, edit, remove, or resume queued inputs |
| `/queue resume` | Resume a paused queue |
| `Escape` during work | Stop the turn and pause follow-ups |
| `/discard` | Clear the current draft |

`Shift+Enter` requires terminal support; use `Ctrl+J` when your terminal cannot
distinguish it from Enter. On macOS, configure Option as Alt/Esc for `Alt+Enter`.

The browser composer also offers steering and queue controls. Editing a queued
message preserves your separate composer draft. Restored queues stay paused
until you resume them, so reopening a session does not run saved work immediately.

## Watch agents

With the built-in Wingman agent, run `/agents` in the terminal UI to list the
session's synchronous and background agents, including finished runs. Select an
agent and press Enter to watch its transcript; press Escape to return to chat.
The viewer is read-only and stays available while the main agent is working.
The `/agents` picker also lists scheduled tasks; select one to view its details.

`/agent` selects Agent mode, and `/plan` selects Plan mode. Tab toggles between
these modes.

## Recover a draft

Drafts and attachments are saved separately from sent messages. Switching
sessions preserves each draft. In the browser command palette, search for
**Recover draft** to reopen a closed draft.

If saving fails, keep the page open and use the retry action. Pending text stays
in memory while the page is open. Resolve storage errors before closing it.

## Reconnect after a network interruption

The agent runs on the workspace server while a browser is disconnected. On
reconnect, the browser restores the current transcript, including an unfinished
response, queued inputs, and pending approvals. Another browser connected to the
same session sees the same work.

If a send's acknowledgement is lost, retry the retained input through the UI.
Wingman recognizes that retry while the same workspace server is running.
After a server restart, inspect the saved conversation before resending an
uncertain request. Retry records do not survive a process restart.

If Wingman asks you to reload after the workspace changes or the server
restarts, preserve unsent drafts and editor buffers before reloading.

## Approvals and questions

In Agent mode, a reviewer model checks risky actions, such as destructive
shell commands, fetches from new sites, and scheduled scripts, before they run.
It uses the utility model and the conversation to judge whether you asked for
the action. A denied action returns its reason to the agent. To allow it, say so
in the chat; critical actions such as secret exfiltration stay denied. After
repeated denials in one turn, the turn stops. Unattended mode runs every action
without review.

Wingman asks you only when the reviewer cannot decide, and for trust prompts
such as running hooks from a cloned repository. Answer the prompt shown for the
relevant session. In the browser, `Alt+A`
focuses a prompt; `Y` or `N` then allows or declines that action. An approval
does not disable later reviews. Each risky command, scheduled script execution,
or permission expansion is evaluated against the current authorization.

## Sandbox

The agent's shell commands and scheduled scripts use an OS sandbox by default:
Seatbelt on macOS, bubblewrap (`bwrap`) with seccomp on Linux, and a native
process security environment (PSEC) on Windows. Unavailable backends fail closed.
Set `WINGMAN_SANDBOX=off` to explicitly disable isolation.

- Commands can read most files, but not credentials such as `~/.ssh`,
  `~/.aws`, `~/.gnupg`, `~/.kube`, or Wingman's MCP tokens. Common authentication
  environment variables, including GitHub and cloud tokens, are removed.
- They write only to the workspace, project memory, and private temporary
  storage (`$TMPDIR`/`$TEMP`). The shared host temporary directory is read-only.
- Git metadata, including the index and worktree pointer files, editor settings,
  `.envrc`, and agent configuration are protected. The edit tool checks the same
  protected names, including files that do not exist yet.
- IP network access is off. A command can request `sandbox_permissions:
  "require_permissions"` with `network: true` and/or `writable_roots`, plus a
  justification. Reviewed grants apply to one execution and keep the sandbox.
  `WINGMAN_SANDBOX_NETWORK=on` enables IP networking globally instead.
- Host Unix sockets, including Docker and SSH agents, stay blocked on macOS
  and Linux, even with IP networking enabled. macOS allows its DNS service.
- `sandbox_permissions: "require_escalated"` requests execution outside the
  sandbox when scoped permissions cannot do the job, such as a Git commit.
  In Agent mode the reviewer decides; Unattended mode allows the request.

Routine commands run without review. The shell classifier still identifies
destructive intent and supplies Plan mode, read-only subagent, and parallel
execution decisions. The sandbox limits capabilities; it does not prevent all
data loss inside writable directories or make every network transfer safe.

Your own `!` commands and hooks run unsandboxed. On Linux and Windows, native
protected-path rules cover existing paths at each writable root; paths that
do not exist yet, or sit below those roots, are not covered. The classifier
remains a supplementary check, not a complete security boundary for those gaps.

On Windows the sandbox attaches filesystem and network policy to the processes.
Wingman does not change existing host ACLs, integrity labels, or ownership, and
does not provision sandbox users or require elevation.

- Windows must expose PSEC 1.0 and native denied paths. The initial supporting
  Windows 11 builds are 26100.9278 and 26200.9278; Wingman queries the actual API
  capabilities before launching. Unsupported hosts fail without an ACL fallback.
- Windows supplies private temporary storage for each environment and sets its
  `TEMP` and `TMP`. The host's shared temporary directory is not writable.
- Existing `.git` metadata is read-only, including a worktree's `.git` pointer
  file. This also blocks commands that update the index or create commits.
- Existing protected editor and agent paths are read-only. Paths that do not
  exist when the command starts are not covered.
- Existing files with multiple hardlinks beneath writable roots are denied
  conservatively, because another link may point outside the workspace or to a
  credential file. Wingman checks writable trees before each launch; large
  workspaces can take longer to start.
- Network access is blocked through native policy unless explicitly enabled.
- The command and all its descendants are stopped before policy is released.
  Killing the helper also kills the process tree through a kill-on-close job.
- Filesystem write grants include security changes inside writable roots.
  A command that explicitly changes the workspace ACL can persist that change;
  native policy continues to block protected-path and outside writes. The
  sandbox itself performs no ACL or label changes.
- The workspace cannot be a drive root, share root, or your home directory.

Switching to PSEC does not undo ACLs or labels applied by earlier Wingman
versions. Those versions did not retain original descriptors, so exact automatic
restoration is unavailable.

See [reviewing changes](reviewing-changes.md) to inspect or undo a turn, and
[remote web access](remote-control.md) to connect through a relay.
