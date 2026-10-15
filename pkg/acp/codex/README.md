# Codex through ACP

Use an installed Codex CLI from an ACP-compatible editor or client:

```bash
wingman acp codex
```

Configure the client to launch `wingman` with arguments `["acp", "codex"]`.
The default backend uses Codex's existing login, configuration, and session
storage. Install Codex and sign in through its CLI first.

To use a Wingman gateway:

```bash
wingman acp codex --backend wingman
```

Set `WINGMAN_URL` and `WINGMAN_TOKEN` if the gateway requires them. The default
gateway is `http://localhost:4242`.

## Models, plans, and history

Use the model and effort controls offered by your ACP client, or pass
`--model ID` and `--effort LEVEL` at startup.

A completed plan appears as conversation text. Clients with plan-implementation
support can offer to implement it; approval starts a separate backend turn.
Intermediate task lists are not shown as ACP plan notifications.

Loading a saved session replays its conversation and attachments. Command
output is shown when the command completes and retains at most the last 1 MiB
per command, with a marker when earlier output is omitted.

## Troubleshooting

If authentication fails, sign in through the CLI and check inherited API
credentials. If Codex reports an active writer, close the other process using
that thread before resuming it. Restore a missing workspace directory or open
a session whose workspace is still available.

A stalled backend start or cancellation can close the connection. Reconnect
before sending another prompt. See the [ACP setup guide](../README.md) for
client configuration and compatibility.
