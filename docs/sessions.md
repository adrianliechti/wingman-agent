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
focuses a prompt; `Y` or `N` then allows or declines an approval. **Allow for
session** grants the scope described by that prompt.

## Sandbox

Set `WINGMAN_SANDBOX=workspace` to run the agent's shell commands and scheduled
scripts in an OS sandbox: Seatbelt on macOS, bubblewrap (`bwrap`) on Linux,
and a Low integrity, write-restricted token on Windows.

- Commands can read most files, but not credentials such as `~/.ssh`,
  `~/.aws`, `~/.gnupg`, `~/.kube`, or Wingman's MCP tokens.
- They write only to the workspace, project memory, and temporary directories,
  and cannot change `.git/hooks`, `.git/config`, `.vscode`, `.envrc`, or agent
  configuration such as `.wingman`, `.agents`, `.claude`, and `mcp.json`.
- Network access is on; set `WINGMAN_SANDBOX_NETWORK=off` to block it. On macOS,
  local services such as the Docker socket or SSH agent stay unreachable.
- When a command needs more, the agent can ask to run it outside the sandbox.
  In Agent mode the reviewer decides; Unattended mode allows it.

Your own `!` commands and hooks run unsandboxed. On Linux, protected paths that
do not exist yet, or sit below the workspace root, are not covered.

On Windows the sandbox works by changing ACLs and labels, and leaves them in
place:

- The workspace and memory directory get a Low integrity label and an access
  entry for a per-workspace SID. The first command in a large workspace can
  therefore take a while.
- Existing protected paths get a deny entry for that SID. Paths that do not
  exist yet are not covered.
- Credential paths get a label that hides them from Low integrity processes.
  This does not affect your own programs.
- Commands use a private temp directory instead of `%TEMP%`.
- Files moved in from elsewhere keep their old ACLs and may be read-only to the
  sandbox.
- Windows runtime devices require the token to retain the Everyone and logon
  SIDs in its restricted access check. Existing directories outside the workspace
  that are already Low integrity and writable by those SIDs can also be written.
  This backend therefore does not provide strict isolation of filesystem writes.
- `WINGMAN_SANDBOX_NETWORK=off` is not supported and makes commands fail.
- The workspace cannot be a drive root or your home directory.

See [reviewing changes](reviewing-changes.md) to inspect or undo a turn, and
[remote web access](remote-control.md) to connect through a relay.
