# Use Wingman from VS Code

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

## Supported scope

The initial integration supports agent and model discovery, session creation
and listing, a single chat per session, history, streamed text and reasoning,
tool activity, approvals, questions, cancellation, and model/effort/mode
settings. Reconnection uses snapshots. Session URI mappings are saved under
`WINGMAN_HOME/ahp` so VS Code can reopen sessions after a host restart; saved
conversation history remains owned by the selected backend.

File browsing and reads are restricted to the server's project directory.
Each host serves one project. Start another server for a different project.

Terminal channels, changesets, automations, multiple chats per session,
client-provided tools, and AHP queue/steering actions are not implemented.
Drafts, read/archive flags, and title overrides set through AHP currently last
for the host process. The browser UI retains its existing features.

## Validation

The integration tests connect over WebSockets using Microsoft's official Go
client and apply its state reducers. They cover the real built-in agent path
with a local model fixture, shared clients, browser commands and approvals,
questions, cancellation, duplicate requests, reconnects, saved session URIs,
cross-origin connections, and workspace file confinement.

```bash
go test -race ./server -run '^TestAHP' -count=1
```
