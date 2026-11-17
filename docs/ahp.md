# Agent, language, and debug protocols

`wingman server` exposes an [Agent Host Protocol (AHP)](https://microsoft.github.io/agent-host-protocol/)
WebSocket endpoint at `/ahp`, on the same port as the browser UI. VS Code
connects to this host and uses Wingman's existing agents and sessions.

## Start the host

Build from this repository:

```bash
go build -o /tmp/wingman-ahp ./cmd/wingman
```

Run the binary from the project you want the agent to work on:

```bash
cd /path/to/your/project
/tmp/wingman-ahp server --no-browser --port 9000
```

The server prints its AHP address, normally `ws://localhost:9000/ahp`.
Without an explicit `--port`, it can select another available port; use the
address printed at startup. Keep the server running while using VS Code.

Configure the built-in agent's provider as described in the
[README](../README.md#configure-your-provider). Installed external agents
use their own CLI login and configuration. Agent discovery lists them without
launching every CLI; selecting a provider initializes it.

## Connect VS Code

Use [VS Code 1.144 or later](https://code.visualstudio.com/updates/v1_144#_agent-host-protocol-v10).

1. Open the Command Palette and run **Chat: Open Agents Window**.
2. In the Agents window, run **Agents: Add Remote Agent Host...**.
3. Enter `ws://localhost:9000/ahp` (or the address printed by Wingman), and
   name the host **Wingman** if prompted.
4. Select the Wingman host, choose an agent and model, and start a session.
   If asked for a working directory, use the directory where you started
   `wingman server`.

Send a prompt, then try a follow-up, cancellation, and a task requiring tool
approval. Open `http://localhost:9000` to see the same sessions in Wingman's
browser UI. An approval answered in one client is resolved for all clients.
Disconnecting VS Code leaves an active turn running in the host; reconnecting
restores the current state. If the host has restarted or has not yet seen a
client, `reconnect` returns AHP `NotFound`, allowing VS Code to initialize again
on the same connection and renegotiate its protocol version automatically.

The local AHP endpoint needs no token or cookie. The server binds to loopback
on both `127.0.0.1` and `::1`, so `localhost` works whichever address a client
resolves it to, and validates the request Host. It accepts clients that send no `Origin`
(native clients), the same-origin Web UI, and desktop web views with their own
URL schemes such as `vscode-file://`. Other websites, including sandboxed
`Origin: null` frames, are rejected. `/lsp` and `/dap` accept only the
same-origin Web UI.

## One host for every project

`wingman host` serves any project: a session names its working directory and
the host opens that project when the session is created or reopened.
`wingman server` is the same host, rooted at the directory it was started in,
with the web UI for that directory's project.

```bash
wingman host --root ~/Projects --token "$(openssl rand -hex 16)"
```

The host prints its address, normally `ws://localhost:9100/ahp`, and has no
web UI. Add it in VS Code like any other host and choose a folder per session;
the folder picker starts in the host's root.

- `--root PATH` limits the directories the host serves; the default is the
  home directory. Sessions and file access outside it are refused with AHP
  `PermissionDenied`.
- `--token TOKEN` (or `WINGMAN_HOST_TOKEN`) is required from every client as
  the `tkn` query parameter, which VS Code sends from a host entry's
  `connectionToken`. Without a token the endpoint is open to local clients,
  like `wingman server`.
- Before a session exists, directories can be listed and files read so that a
  client can choose a folder; files become writable once a session uses their
  directory. A session created without a working directory uses the host's
  root.
- A project closes after 15 minutes without a subscribed client, a request
  that reaches it, or work: a running turn, queued input its agent does not
  save, an open question, a background task, a schedule or a live terminal
  keep it open. The builtin agent saves its queue, which waits for the session
  to open again.
- Sessions the host created are listed after a restart without opening their
  projects, with their title, read and archived flags and last change;
  subscribing to one opens its project. A session or chat opens again even if
  it never ran a turn. A creation that fails leaves no session behind, nor
  does a chat whose initial message fails, and a disposed session's or chat's
  URI can be used again.

A host owns the connections, the order of their actions and the channel
namespace. A project owns its agents, sessions, terminals, file watches and
changesets, and is the directory its sessions work in. Sessions carry it as
the protocol's `project` (`uri` and `displayName`), with links in the
directory resolved; a directory reached through a link opens the project it
leads to. On `wingman server` the web UI, `/lsp`, `/dap` and the REST routes
describe the project of its directory, which stays open; projects below it
open on demand as on any host, and the REST routes refuse their chats.

In the server package, `Host` owns connections, ordered publication, saved
session identities, and the project registry. `Project` owns the workspace,
agents, controllers, terminals, watches, and changesets. `Server` is the web
adapter: HTTP routing, browser preferences, preview hosting, and the embedded
UI. Its editor state owns browser LSP document ownership, diagnostic subscribers,
external-source permissions, and DAP reference IDs. Language servers and debug
adapters remain workspace resources. Projects notify the optional editor adapter
of diagnostic invalidation through a callback invoked outside the host lock.
The standalone host opens projects without constructing a web server.

Both `Server` and each backend hold an explicit `Project` reference. HTTP
handlers belong to the web adapter; native controllers and backends expose
operations without HTTP request or response types. Command receipts retain
the operation's error directly, with protocol error mapping at the boundary.

`host.go` and `projects.go` handle host lifetime and project routing;
`project.go` owns project startup, teardown, and channel attachment.
`connection.go` dispatches protocol requests. The `server/ahp` package contains
JSON-RPC framing, errors, origin checks, and channel URI rules, with no
project or web dependencies. `aliases.go` persists protocol-to-native session
identities, `sessions.go` creates and loads sessions, and `session.go` and
`chat.go` maintain their protocol state. `controller.go` and `runtime.go` own
native execution, with turn processing in `turns.go`, prompts in `prompts.go`,
and background task delivery in `tasks.go`. `routes.go` resolves HTTP chat routes
directly to their controllers. `handler_*.go` files adapt project services to
HTTP; file operations, search, Git, skills, and model discovery keep their service
code in separate files. Retained messages convert directly to transcript entries,
preserving their existing identities without an intermediate conversation model.

The host publication mutex orders snapshots and actions. Controller state
locks precede it and also protect command receipts; backend I/O runs outside
them. The connection read loop owns its channel queues. Each backend uses one
mutex for its session registry and task pumps. The project admits backend
operations under its backend registry lock; cancellation closes admission,
and teardown joins all admitted operations before closing agents. Project
startup and teardown remain serialized so a replacement cannot open files
while the previous instance is closing.

Alias persistence has one writer per project. Updates and deletions are staged
under the host lock, then written outside it. A successful write clears only the
change it wrote; a newer change remains pending. Failed changes retry on the next
update and at project teardown. Session creation waits for its own alias to be
saved, independently of failures for other sessions. This also orders deletion
against reuse of the same URI without a separate per-session dirty flag.

Context cancellation determines host and project lifetime. A controller's
status is its session lifecycle, including the retained deletion tombstone.
Host registries determine which sessions, chats and terminals are live;
stale handles cannot publish to a replacement using the same URI.

Closing a project removes its live channels but keeps its saved aliases.
Reopening waits for the previous instance to finish closing. Host shutdown
rejects new project lookups and joins any startup already in progress.

Channels a project names itself carry the project:
`ahp-changeset:/wingman/<project>/uncommitted`,
`ahp-terminal:/wingman/<project>/<id>`, and a third path segment with the
project directory on unaliased session URIs. Clients read them from session
and root state; the web UI takes its project's namespace (`wingman/<project>`)
from `/api/workspace`. The `wingman.workspace` root metadata and the
`x-wingman/workspaceChanged` notification exist only on `wingman server`, for
the web UI's project.

## Shared host and browser architecture

The web UI and VS Code use the same AHP host for sessions, chat, approvals,
queues, and terminals. Session controllers and the turn manager own execution,
backend persistence, pending questions, and duplicate-command receipts. A client
subscribes to a channel snapshot and applies ordered actions using Microsoft's
TypeScript reducers. Reconnection obtains fresh snapshots; it does not restart
an agent or automatically replay uncertain terminal input or debugger commands.

| Endpoint | Browser responsibilities | Host responsibilities |
| --- | --- | --- |
| `/ahp` | Render sessions/chat/terminals and changes; submit actions | Own agents, queues, approvals, PTYs, watches, and changesets |
| `/lsp?instance=…` | Synchronize open buffers; request language features; review/apply edits | Route LSP requests to managed language servers and structural fallbacks |
| `/dap?instance=…` | Show debugger state; request breakpoints, stepping, frames, and variables | Own adapters/debuggees, validate launch plans, retain output, and invalidate references |

Each WebSocket text frame contains one complete UTF-8 JSON message. AHP and LSP
use JSON-RPC 2.0. DAP uses its own `seq`, `type`, `command`, `request_seq`, and
`event` envelopes. Native language servers and debug adapters retain their
Content-Length framing on stdio/TCP. The `instance` value comes from
`/api/workspace`; it prevents an old browser from sending operations to a
replacement workspace. It is an identity check, not an authentication token.

The former session-command/event, terminal, LSP-operation, debug-operation,
file, diff, and AI editor HTTP routes are removed. HTTP still serves the UI,
workspace discovery, settings, skills, reviews, tasks/schedules, debugger
setup, Git history/commands, downloads, and isolated website previews.
`/content?path=…` retains browser range/streaming support; `/preview?path=…` redirects to the existing
isolated preview origin. Both URLs retain the workspace instance check.

`GET /api/workspace` replaces `/api/v2/bootstrap`, `/api/capabilities`, and
`/api/terminals/shells`. It supplies `workspaceId`, `instanceId`, `protocol`,
`name`, the native host `path`, `platform`, `shells`, `lsp`,
`debug`, the two `window.*.position` preferences, and optional `managed_tools`
installation progress. The first request needs no instance; subsequent requests
with an instance reject a replacement host. The browser seeds its workspace query
cache from that startup response and refreshes on `workspace_changed` events.
Task and terminal support are always present on supported builds, so their
flags are removed. Git status is fetched separately from `/api/workspace/git`,
which returns `null` when repository initialization is available; `git_init` is
therefore also removed. `/api/workspace/insights` returns Git activity statistics.

Agents and their models come from the AHP root state, and a new chat's default
settings from `resolveSessionConfig`; `/api/v2/backends` and its settings route
are removed. `POST /api/v2/backends/{provider}/models/refresh` retries model
discovery. Turn reviews, tasks and schedules live under `/api/v2/chats/{chat}`,
where `{chat}` is the URL-encoded chat URI.

Workspace tools that are not protocol state use REST:

| Route | Purpose |
| --- | --- |
| `GET /api/workspace/files?query=…` | File picker: up to 50 visible paths containing the query |
| `POST /api/workspace/search` | Text search; streams NDJSON `file` events, then `done` (or `error`); aborting the request cancels it. At most 8 at once, one minute each |
| `POST /api/workspace/reveal` | Shows a workspace path in the host's file manager |
| `GET /api/workspace/terminals/{terminal}/status` | `{busy}`: whether a terminal runs a command besides its shell |
| `GET /api/workspace/activity` | Language-service and analysis progress |

## Git and AHP changesets

Git stays on a small REST surface; there is no custom Git WebSocket.

| Route | Purpose |
| --- | --- |
| `GET /api/workspace/git` | Repository status: branch, HEAD, upstream counts and index/worktree files, or `null` if no repository |
| `GET /api/workspace/git/commits` | Commit history |
| `POST /api/workspace/git/commits` | Commit with the user's `message` |
| `GET /api/workspace/git/branches` | List local and known remote branches without fetching |
| `POST /api/workspace/git/branches` | Create and switch to a branch (`name`) |
| `POST /api/workspace/git/checkout` | Switch branches (`name`, optional `remote`) |
| `POST /api/workspace/git/fetch` | Explicitly refresh remotes and return the branch list |
| `POST /api/workspace/git/pull` | Pull |
| `POST /api/workspace/git/push` | Push |
| `POST /api/workspace/git/init` | Initialize a repository |
| `GET /api/workspace/git/diff?base=…&head=…&mode=…` | Create an immutable AHP comparison channel; return its URI and revision metadata, without file content |

Comparison mode is `direct` or `merge-base`. Mutation bodies are bounded to
1 MiB, reject unknown/unrelated fields, and have a two-minute deadline. Requests
are not automatically retried. AI commit-message generation is removed; commits
use the message entered by the user. The former `/api/git/*` routes are removed.
Status and insights have workspace endpoints; stage, unstage, and discard
operations use AHP.

Three shared AHP channels describe workspace changes:
`ahp-changeset:/<namespace>/uncommitted`, `ahp-changeset:/<namespace>/staged`,
and `ahp-changeset:/<namespace>/unstaged`, in the project's namespace. The browser subscribes to standard
`ChangesetState`, applies the official reducer, and retrieves immutable
before/after `ContentRef`s through `resourceRead` and computes diffs from them.
A rename has different before/after URIs; binary content has the
`application/octet-stream` content type and no text.

The host advertises standard `invokeChangesetOperation` operations: `stage` on
unstaged changes and `unstage` on staged changes, at changeset or resource scope;
`discard-changes` on unstaged changes, at resource scope only. Discard restores
the worktree from the index and preserves staged content. Resource operations
validate membership, scope, side, and any supplied file nonce against a freshly
read snapshot; stale nonces fail with AHP Conflict (`-32011`). Range operations
are not advertised. Commit stays in `POST /api/workspace/git/commits` because standard changeset
operation parameters do not carry a manually entered commit message.

Subscribed layers refresh every two seconds and after host Git mutations.
Fingerprints skip re-reading unchanged file contents; operation validation always
forces a fresh read. Comparisons are immutable and have no mutation operations.
Retention is bounded to 64 MiB per changeset, 128 MiB shared content, and 32
comparison slots in addition to the three workspace layers. Inactive comparisons
can be evicted after one minute when capacity is needed. Re-run a comparison if
its channel has expired.

## AHP support

Supported features include provider/model discovery; session creation, listing
and reopening; forked chats; history and streaming text/reasoning; tool
activity; client-provided tools; chat attachments; approvals and typed
questions; cancellation; history truncation; per-chat model, effort, mode and
unattended settings; and editable, reorderable queued/steering messages. The
controller remains the single authority when browser and IDE clients act
concurrently. Retry receipts are bounded and scoped to a running host; they do
not survive a server restart. Session and chat URI mappings are saved under
`WINGMAN_HOME/ahp`.

Session summaries carry what a list needs without subscribing to each session.
`activity` names what the session waits for (an approval's or question's first
line) or does (a retry, compaction or tool progress, else its latest tool with
its intention or file); it is empty when idle, and `root/sessionSummaryChanged`
sends `""` to clear it. `changes` counts the lines and files the builtin
agent's turns changed, net per file and without undone turns, recounted when a
turn ends, after an undo or a rewind, and when the session loads; a fork's
copied history is not counted again. Sessions keep both, with their read flag,
for listing while their project is closed.

A chat is unread from the start or end of a live turn until a client marks it
read with `chat/isReadChanged`; replaying history when a session loads does not
mark it. Reducers do not clear the flag for a turn's end, so the host announces
it. A session is unread once one of its chats is and read when a client reads
all of them; `session/isReadChanged` sets every chat. Sessions saved before
this, and native sessions no client opened, list as read.

Sessions and chats are identified only by their AHP URIs. A native session that
was never given an alias is reachable at
`ahp-session:/wingman/<base64url(backend)>/<base64url(id)>/<base64url(project)>`,
with its default chat at the same path under `ahp-chat:`. When the session
lives under an alias, subscribing to that canonical URI fails with
InvalidParams and error data `{ "resource": "<alias>" }`. The same redirect
answers the earlier form without the project segment on `wingman server`,
pointing at the web UI's project.

Each chat projects one native session. `createChat` with
`source: { kind: "fork", chat, turnId }` forks a completed turn of a chat in the
same session into a new native session, which becomes a chat of that session.
Forks from an earlier turn copy the journal up to the next input, so the source
may keep running; only the builtin agent supports this. The fork's turns keep
what only the host knows of them: their timing, usage and messages as sent. `disposeChat` deletes a
forked chat, and `disposeSession` deletes the session with all its chats. Each
chat has its own queue, turns and settings. As the proposed per-chat
configuration describes, a chat's config is an overlay on its session's values:
what a client set on that chat, and any setting that differs from the session's,
such as a model a message chose or an effort its backend adjusted.
`session/configChanged` changes the session's values and every chat that does
not set them; a new chat starts with the session's values and a fork with its
source's. A replaced overlay returns the values it omits to the session's.
The session's values start from its default chat and, like the values clients
set, last for the host process. The default chat has an empty title, which
inherits the session's, as in VS Code's host: 1.1 has no action that renames a
chat.

`chat/queuedMessagesReordered` moves the named pending messages to the head of
the queue in the given order; the others keep theirs, and the host answers with
the order the queue then has. A turn that consumes a pending message carries
its id as `queuedMessageId`, after a `chat/pendingMessageRemoved` for it; this
also holds for a message that an idle chat runs at once. A `chat/turnStarted`
that arrives while the last turn settles starts once it has; one that arrives
while another turn or a compaction runs is rejected. Steering that a turn took
stays part of that turn.

`chat/truncated` rewinds a chat of the builtin agent: it keeps the history
through `turnId`, or none without one, in the agent's journal as well, so the
model no longer sees the dropped turns. A running turn is stopped and the queue
is cleared first, as when a turn is stopped. Files changed by the dropped turns
stay as they are. External agents reject the action.

Stopping a turn (`chat/turnCancelled`) also removes the messages queued behind
it. A message `/compact [focus]` runs as a compaction turn when the backend can
compact: the chat's `activity` reads `Compacting context…`, and the turn ends
with a system notification `Context compacted · ~X → ~Y tokens`. `completions`
offers `/compact` only for such backends. Compaction turns are not part of the
agent's history and last for the host process. A queue recovered after a
restart stays paused until the user sends the next message.

Tool calls carry the tool hint as `intention`. Their content holds the result
text, file locations as `resource` items (`file:` URIs with an optional
`#L<line>` fragment) and images as `embeddedResource` items; `_meta.toolKind`
follows VS Code's convention (`terminal` for shell commands). Pending approvals
offer the confirmation options `once` and `deny`; each approval is answered once. Retries, tool
progress and compaction are reported as the chat's `activity`. A steering
message consumed by a running turn appears as a system notification with the
user's text. A steering message sent while no turn runs waits in the queue
(`kind: "steering"`), where it can be edited and removed; several can wait. The
next turn takes them as soon as its agent accepts steering, in their order;
while one is handed over it can no longer be edited, and stopping the turn
removes it unless the agent took it. The builtin agent saves them with its queue. An
agent that cannot steer answers them after the turn. A question or approval
that a turn did not raise itself, such as one from a background task, is the
session's: it is listed in `inputNeeded` and answered with `chat/inputCompleted`.
A turn's final usage and its failure arrive before its end. `chat/usage` reports the latest request's prompt tokens as
`inputTokens`, and the turn's output and cache-read tokens; for a provider that
counts no tokens it names only the model.

A message may attach a chat of any session (`type: "chat"`). The host resolves
the chat's turns through `endTurn`, or all it has, when it accepts the message,
and rejects an unknown chat and a turn that is unknown, running or cut away. The
transcript holds the user texts and markdown answers; attachments inside it stay
references.

What an attachment gives the model and not the transcript (a chat's transcript,
a `simple` attachment's `modelRepresentation`, a selection, an embedded text
resource) precedes the message. The builtin agent keeps it as a message of its
own that leaves the history with the message's turn; other agents and a steering
message read it inline. Queued messages and the turns that consume them keep the
message as the client sent it, a queued one also across a restart; turns loaded
from an agent's history show only its text, files and images.

The tools of a session's active clients (`session/activeClientSet`) join the
builtin agent's tools in every chat of the session, from the chat's next turn.
A client's tool replaces the agent's tool of the same name; subagents keep the
agent's own tools, and names that model APIs reject are skipped. A call to such
a tool carries the client as its `contributor`. It stays `streaming` until the
agent runs it, so a client never starts a call that the agent then refuses:
plan mode runs only tools with `readOnlyHint`. The call then waits in `running`
for its owner, which also finds it as a `toolClientExecution` request in the
session's `inputNeeded`. `toolInput` is the model's unmodified input. Only the
owner may dispatch `chat/toolCallContentChanged` and `chat/toolCallComplete`.
The call fails when its owner leaves the session, the turn is stopped, or the
agent's tool timeout passes. Wingman asks for no approval of a client's tool;
its client does. The contributor and the structured result last for the host
process: reloaded history keeps the result as text. External agents receive no
client tools.

Terminal channels share the workspace terminal manager, including debugger
terminals. Following the [terminal URI specification](https://microsoft.github.io/agent-host-protocol/specification/terminal-channel.html#uri),
Wingman accepts client-chosen schemes, including VS Code's `agenthost-terminal`
and custom schemes such as `x-my-terminal`. URIs must have the form
`<scheme>:/<id>`; other `ahp-*` namespaces remain reserved for protocol channels.
The original URI is preserved in the catalogue, subscriptions, actions, and
reconnection. Wingman-created terminals use `ahp-terminal:/<namespace>/<id>`.

Terminals support discovery, creation, ownership, input, resize, titles,
clear, disposal, reconnect scrollback, and retained exit status. A disconnected
owner can be replaced; a connected owner's terminal remains read-only to other
clients. Scrollback is bounded to 256 KiB. Shell-integration markers are filtered
without splitting UTF-8 characters. Close confirmation asks the terminal status
route whether a foreground process runs.

Workspace file operations use standard AHP `resourceList`, `resourceRead`,
`resourceResolve`, `resourceWrite`, `resourceCopy`, `resourceMove`,
`resourceDelete`, and `resourceMkdir`. Directory creation uses `mkdir -p`
semantics. Copy/move from the web UI refuse existing destinations. Deleting an
already-missing resource is treated as success by the web UI. `resourceResolve`
returns content-hash `etag` values for files up to 16 MiB, and saves use `ifMatch`
to reject stale contents. The browser derives the same SHA-256 revision from the
exact bytes read or saved, avoiding a later metadata read racing another writer.
Reads and writes are bounded to 16 MiB; binary reads use base64. Larger binary
files remain available through the streaming content URL. The file tree derives
paths and directory flags, applies its UI filtering locally, and no longer
transfers unused entry sizes. Language and text sizes are derived locally.

The former batch-save API had no UI callers and has been deleted. Multi-file
editor changes remain unsaved buffers; each save retains its own conflict check.
File picker, text search and reveal use the REST routes above; upstream
`completions` targets chat input and attachments, not workspace search.

Workspace resource access is confined to the project. Recursive resource
watches support include/exclude globs. The shared uncommitted changeset exposes
immutable before/after content, content/file nonces, and advisory
`ChangesetFileStatus.Editing` flags. Content from replaced changesets is not retained indefinitely: refresh
the changeset if an old content reference is unavailable. An editing flag is
presentation information, not a lock or proof that applying a change is safe.

The host supports AHP 1.0 and 1.1, including mixed steering/queued pending
messages, changeset file nonces and `status` activity flags, model `vendor` and
per-model effort schemas, chat activity, and host-measured reasoning
`startedAt`/`duration`. A reasoning run ends when the next markdown, tool or
reasoning part starts, or before the turn's terminal action; timing lasts for
the host process and history from earlier processes is untimed. The initialize
response supplies standard `environment.pathStyle` metadata for native host
paths. The web UI negotiates 1.1; 1.0 clients receive the legacy queue/steering projection.
Side chats, chats from other providers, TCP channels, experimental channel
windows, and protocol-native automations are not advertised. Chat drafts and a chat's own read and archived flags last while its project is open.
The remaining Wingman extensions are the `x-wingman/workspaceChanged` root
notification (`files_changed`, `workspace_changed`, `skills_changed` and
`tasks_changed`), the root's `_meta["wingman.workspace"]` identity used to
detect a replaced workspace, and `createTerminal`'s `_meta["wingman.shell"]`.
Sessions, models, terminals and Git changes reach clients through the standard
root notifications and changeset channels. Wingman also follows VS Code's
`_meta` conventions: `vscode.agentHost.resources` on the root, `toolKind` on tool
calls, and `command`, `description`, `argumentHint` and `isSkill` on completion
attachments.

## Skills and commands

`GET /api/workspace/skills` lists the workspace's effective skill catalogue,
including bundled, personal, project and plugin skills. Plugin entries retain
their `plugin:skill` names. Listing is independent of the backend and session
and never starts an agent; `/api/skills` has been removed.

The standard chat-scoped AHP `completions` request supplies the combined skill
and native command picker for an existing session. `/` is advertised as a
completion trigger. Native commands come from that session's backend, take
precedence over same-named skills at the start of a message, and are not offered
inline. Skill instructions are expanded by the host; native commands are passed
to the backend without expanding a conflicting skill. Completion attachments
identify `skill` versus `command` with `displayKind` and carry VS Code's
`command`, `description`, `argumentHint` and `isSkill` metadata. The WebUI
preserves those attachments through draft recovery
and delivery. Draft chats use the workspace catalogue until a session exists.
`/compact` is offered as a command when the backend can compact.

## LSP and DAP scope

Debugger discovery uses `POST /api/workspace/debug/targets`; launch planning
and installation progress use `POST /api/workspace/debug/plan`. Debugger
execution and inspection run over `/dap`. The old `/api/debug/targets` and
`/api/debug/plan` routes are removed.

LSP uses full-document synchronization with increasing versions and UTF-16
positions. Unsaved buffers are restored after reconnect. Editor buffers are
kept current in order; a request never pushes its own, possibly older, copy to
the language server. When several connections edit one file, each request is
answered for its own connection's buffer, closing one restores another's, and
an edit computed while another connection's buffer took over answers -32801.
A buffer that matches the file on disk lets agent reads
refresh it. The host scans the workspace on its file-change checks, skipping
hidden, dependency and build directories such as `node_modules`; changes reach
language servers as `workspace/didChangeWatchedFiles` for the watchers they
register, and documents opened from disk are refreshed or closed. Supported
requests cover
completion/resolve, hover, definitions, implementations, references, signatures,
symbols, highlights, folding, semantic tokens, rename, code actions/resolve,
commands, formatting, inlay hints, and pull diagnostics. Type-aware methods
return their empty result for files that no language server covers; structural
methods use tree-sitter fallbacks. Cancellation uses `$/cancelRequest` and
answers -32800; timeouts answer -32803, and language-server error codes pass
through. The gateway also implements LSP 3.18 `workspace/textDocumentContent`
for read-only external source files explicitly returned by language servers;
this does not require those servers to implement 3.18.

`workspace/executeCommand` accepts only commands the language server provides
and carries `textDocument` to select the underlying language service. A
`workspace/applyEdit` the server sends while the command runs is forwarded to the
browser for review, and the server receives the user's decision; the command's
two-minute limit pauses during the review. Edit metadata retains revision checks
and multi-file review, and document versions in edits are the editor's.
`wingman/documentCapabilities` reports the selected file's capabilities. When a
client declares `workspace.diagnostics.refreshSupport`, the host sends
`workspace/diagnostic/refresh` after workspace or language-service changes. Edit
and diagnostic results carry optional `_wingman` metadata.

DAP supports initialize, launch/attach, configurationDone, source and exception
breakpoints, continue/next/stepIn/stepOut/stepBack/reverseContinue/pause,
threads, stackTrace, scopes, variables, evaluate, cancel, terminate, and
disconnect. It negotiates native paths and one-based positions. Requests on one
connection run in order; launch, attach, and evaluate run beside them. Launch
and attach reference a reviewed plan by `_wingmanPlanId`: the host keeps the
plans its setup flow produced, and prelaunch commands, adapter, and project come
only from that plan. Breakpoints changed while a session starts reach it.
`initialize` advertises gateway features; adapter-dependent features, including
exception breakpoint filters, arrive in a `capabilities` event when they change.
Breakpoints have stable ids, are reported `pending` until the adapter verifies
them, and change through `breakpoint` events. Adapter `runInTerminal` requests
are handled by the host's shared PTY manager, and exited debugger terminals are
removed. `wingman/status` restores the current shared debug session, output, and
breakpoint verification; namespaced session/epoch arguments protect browser
actions from stale state. Each lifecycle event is sent once per connection, and
a new connection restores through `wingman/status` rather than replayed events.
Output keeps its category and is sent incrementally with a `_wingmanOffset`;
telemetry is not shown. Stops and continues are tracked per thread. Browser
reference IDs are scoped to their connection and stop and cannot refer to an
object from a later stop. Errors use short `message` codes such as `cancelled`
and `notStopped`, with readable text in `body.error`. Closing a socket leaves the
process alive. `disconnect` terminates a launched debuggee and leaves an attached
one running unless `terminateDebuggee` says otherwise.

## SDK provenance

The host uses the official Go SDK
`github.com/microsoft/agent-host-protocol/clients/go@v1.1.0` and the browser uses
`@microsoft/agent-host-protocol@1.1.0` from npm, both released November 12, 2026.
The Go module and npm lockfiles pin the releases. The browser integration tests
also install the published 1.0.0 SDK under an alias to exercise older clients.
The former development archive and its generation script are no longer needed.

### Protocol simplification review (November 14, 2026)

Upstream [1.1.0](https://github.com/microsoft/agent-host-protocol/tree/spec/v1.1.0)
is released; [the changelog](https://github.com/microsoft/agent-host-protocol/blob/main/CHANGELOG.md)
lists 1.2.0 as unreleased. Its changelog entry is still empty, but the
[changes since 1.1](https://github.com/microsoft/agent-host-protocol/compare/spec/v1.1.0...main)
include one-time automation triggers (`kind: "oneTime"`, `runAt`) and the
`AutomationCapabilities.oneTime` presence capability. SDK versions and spec
versions are independent; the host does not advertise unreleased features.

The stable SDK removes the handwritten reconnect-result object: it now
serializes the reconnect discriminator correctly. The Go terminal subscription
test helper still decodes its known channel directly: 1.1.0 continues to mistake
a terminal lifecycle object for a session lifecycle string in `SnapshotState`.
File activity uses the final
`ChangesetFile.status` bitset rather than the draft `isEditing` field. Queue,
reasoning timing, model effort schemas, chat titles and per-chat configuration
already use standard 1.1 fields and actions, so they need no extra protocol.

The most useful next migration is scheduled tasks. AHP's automation catalogue,
update/remove actions and subscriptions could replace the schedule-specific
REST list/delete/pause/resume endpoints in `server/handler_schedules.go` and
`server/ui/src/api/tasks.ts`. The 1.2 draft's one-time trigger maps directly to
our timestamp schedules, alongside 1.1's recurring schedule triggers. Map
`Task.ID` to a stable automation URI, paused state to `enabled: false`, and
`nextRunAt` to an absolute timestamp that the client formats locally. The draft
requires at-most-once firing for each `runAt`, and changing `runAt` moves the
occurrence; an adapter must reset the native one-shot bookkeeping accordingly.

That migration is not implemented here. It needs capability negotiation,
per-client catalogue filtering for older clients, authoritative automation/run
state and action acknowledgements, and mappings for existing stored tasks.
Script gates, failure backoff, jitter, persistence and native queue delivery
remain host execution policy; standard protocol types do not replace the
runner. The published SDKs do not yet contain the one-time trigger, so adopting
it now would reintroduce an unreleased SDK pin. Keep the scheduling REST API
until the full adapter and its compatibility tests are ready.

The remaining extensions listed above still have specific jobs. Standard
resource-watch channels cover file changes, but do not replace workspace
identity or skill/task catalogue invalidation. The transport-boundary browser
notification handling remains necessary because the SDK's bounded async event
queues can discard older events. Experimental flow control could address large
stream delivery, but requires host framing/credit support and is not a drop-in
replacement. Ranged resource reads are optional and do not replace revision
checks or workspace path restrictions. Cross-provider chats, TCP forwarding,
canvas channels and native automations are separate capabilities; upgrading an
SDK does not implement or advertise them.

## Validation

`TestAHPConformance` checks selected upstream subscription and recovery contracts
against the real WebSocket host for both AHP 1.0 and 1.1. It covers version
fallback, initial snapshots, multi-client broadcast, duplicate subscriptions,
channel isolation, unsubscribe/resubscribe, server-only action rejection, and
reconnect followed by live delivery. Raw frames expose unexpected deliveries;
Microsoft's Go reducer checks that client state agrees with fresh snapshots.
The provider fixture needs no model credentials, Node, or VS Code. This is a
focused local suite based on the upstream contracts, not the complete VS Code
conformance runner.

```bash
go test -race ./server -run '^TestAHPConformance$' -count=1 -v
```

Tests exercise the official pinned TypeScript SDK and the released 1.0 npm SDK
against the Go host, including shared sessions, actions, typed questions,
queues, compatibility, terminals, resource watches, and real Git changesets.
LSP/DAP tests cover real WebSockets, unsaved buffer versions, cancellation,
workspace confinement, stdio adapter launch/inspection, process survival across
reconnect, and expired references. Browser tests cover the complete UI flows.

```bash
go test -race ./server ./pkg/dap ./pkg/terminal
npm --prefix server/ui run test:unit
npm --prefix server/ui run build
go test -tags=e2e ./server -run TestWebUIE2E -count=1
node server/ui/bench/sessionStore.ts
```
