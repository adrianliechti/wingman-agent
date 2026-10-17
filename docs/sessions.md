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
and queued follow-ups stay with the original. Forking waits for the current turn
to finish.

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

See [reviewing changes](reviewing-changes.md) to inspect or undo a turn, and
[remote web access](remote-control.md) to connect through a relay.
