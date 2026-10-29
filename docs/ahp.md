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
restores the current state.

The local AHP endpoint accepts WebSocket clients from any origin without a
token or cookie. The server binds to loopback and validates the request Host.

## Shared host and browser architecture

The web UI and VS Code use the same AHP host for sessions, chat, approvals,
queues, and terminals. Session controllers and the turn manager own execution,
backend persistence, pending questions, and duplicate-command receipts. A client
subscribes to a channel snapshot and applies ordered actions using Microsoft's
TypeScript reducers. Reconnection obtains fresh snapshots; it does not restart
an agent or automatically replay uncertain terminal input or debugger commands.

| Endpoint | Browser responsibilities | Host responsibilities |
| --- | --- | --- |
| `/ahp` | Render sessions/chat/terminals; submit actions | Own agents, queues, approvals, PTYs, watches, and changesets |
| `/lsp?instance=…` | Synchronize open buffers; request language features; review/apply edits | Route LSP requests to managed language servers and structural fallbacks |
| `/dap?instance=…` | Show debugger state; request breakpoints, stepping, frames, and variables | Own adapters/debuggees, validate launch plans, retain output, and invalidate references |

Each WebSocket text frame contains one complete UTF-8 JSON message. AHP and LSP
use JSON-RPC 2.0. DAP uses its own `seq`, `type`, `command`, `request_seq`, and
`event` envelopes. Native language servers and debug adapters retain their
Content-Length framing on stdio/TCP. The `instance` value comes from
`/api/v2/bootstrap`; it prevents an old browser from sending operations to a
replacement workspace. It is an identity check, not an authentication token.

The former session-command/event, terminal, LSP-operation, and debug-operation
HTTP routes are removed. HTTP still serves the UI, bootstrap, files, settings,
Git/reviews, tasks/schedules, debugger discovery/installation/planning, and
read-only external files explicitly returned by a language server.

## AHP support

Supported features include provider/model discovery; session creation, listing,
reopening, and forks; one chat per session; history and streaming text/reasoning;
tool activity; approvals and typed questions; cancellation; model/effort/mode
settings; and editable queued/steering messages. The controller remains the
single authority when browser and IDE clients act concurrently. Retry receipts
are bounded and scoped to a running host; they do not survive a server restart.
Session URI mappings are saved under `WINGMAN_HOME/ahp`.

Terminal channels share the workspace terminal manager, including debugger
terminals. They support discovery, creation, ownership, input, resize, titles,
clear, disposal, reconnect scrollback, and retained exit status. A disconnected
owner can be replaced; a connected owner's terminal remains read-only to other
clients. Scrollback is bounded to 256 KiB. Shell-integration markers are filtered
without splitting UTF-8 characters. The small `x-wingman/terminalStatus`
extension supplies the foreground-process check used by close confirmation.

Workspace resource access is confined to the project. Recursive resource
watches support include/exclude globs. The shared uncommitted changeset exposes
immutable before/after content, content/file nonces, and advisory `isEditing`
flags. Content from replaced changesets is not retained indefinitely: refresh
the changeset if an old content reference is unavailable. An editing flag is
presentation information, not a lock or proof that applying a change is safe.

The host supports AHP 1.0 and selected 1.1 features. The web UI negotiates 1.1;
1.0 clients receive the legacy queue/steering projection. Multiple chats,
mixed providers within one session, client-provided tools, TCP channels,
experimental channel windows, and protocol-native automations are not advertised.
Draft/archive/title overrides currently last for the host process. Wingman's
additional presentation information uses `_meta.wingman`; compacting, queue
pause/resume, forks, focus, and workspace notifications use documented
`x-wingman/*` commands or namespaced metadata.

## LSP and DAP scope

LSP uses full-document synchronization with increasing versions and UTF-16
positions. Unsaved buffers are restored after reconnect. Supported requests cover
completion/resolve, hover, definitions, implementations, references, signatures,
symbols, highlights, folding, semantic tokens, rename, code actions/resolve,
commands, formatting, inlay hints, and pull diagnostics. Cancellation uses
`$/cancelRequest`. Command edits use `workspace/applyEdit` requests and receive
the browser's application result. Edit metadata retains revision checks and
multi-file review. `wingman/documentCapabilities` reports the selected file's
capabilities; `wingman/activity` reports managed-language-service progress.
`workspace/executeCommand` also carries `textDocument` to select the underlying
language service, and edit/diagnostic results carry optional `_wingman` metadata.

DAP supports initialize, launch/attach, configurationDone, source breakpoints,
continue/next/stepIn/stepOut/stepBack/pause, threads, stackTrace, scopes, variables,
evaluate, cancel, and disconnect. It negotiates native paths and one-based
positions. Launch/attach arguments include the reviewed `_wingmanPlan` produced
by the existing setup flow. Adapter `runInTerminal` requests are handled by the
host's shared PTY manager. `wingman/status` restores the current shared debug
session and output; namespaced session/epoch arguments protect browser actions
from stale state. Standard stopped/continued/output/terminated events drive UI
updates, with infrequent polling for recovery. Browser reference IDs are scoped
to their connection and stopped epoch and cannot accidentally refer to an object
from a later stop. Closing a socket leaves the process alive; an explicit
`disconnect` with `terminateDebuggee: true` stops it.

## SDK provenance

The host's Go types and the browser SDK are pinned to upstream commit
[`0b7b7f9f4ceb2fbd8846557416f765ac73da647d`](https://github.com/microsoft/agent-host-protocol/commit/0b7b7f9f4ceb2fbd8846557416f765ac73da647d).
The checked-in npm archive combines the official TypeScript client with that
commit's canonical generated protocol types/reducers. It is labeled
`1.1.0-dev.0b7b7f9f4ceb`; it is not a published 1.1 release. Installation does not
require a local upstream checkout. To reproduce the archive:

```bash
node scripts/vendor-ahp.mjs /path/to/agent-host-protocol
```

## Validation

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
