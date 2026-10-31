# Claude Code through ACP

Use an installed Claude Code CLI from an ACP-compatible editor or client:

```bash
wingman acp claude
```

Configure the client to launch `wingman` with arguments `["acp", "claude"]`.
The default backend uses Claude Code's existing login, settings, and session
storage. Install Claude Code and sign in through its CLI first.

To use a Wingman gateway:

```bash
wingman acp claude --backend wingman
```

Set `WINGMAN_URL` and `WINGMAN_TOKEN` if the gateway requires them. The default
gateway is `http://localhost:4242`.

## Models and planning

Use the model and effort controls offered by your ACP client, or pass
`--model ID` and `--effort LEVEL` at startup. Available choices come from the CLI.

Plan mode is supported. Approve the resulting plan in the client to continue
with implementation. Task-list notifications are not forwarded as ACP plan
updates, so a client may show the plan as conversation text.

Wingman disables Ultracode workflows and their keyword trigger, remote control,
and the `ultrathink` effort override in both native and gateway launches. The
client's model, effort, and mode controls remain authoritative. Gateway launches
also restrict model choices to the gateway catalog and disable Claude's
background telemetry and account-management commands.

## Troubleshooting

If authentication fails, check the CLI's login and inherited provider environment
variables. A stalled or terminated Claude process can require reconnecting the
client. Cancelling a turn ends its unfinished tool calls; background subagents
can continue while the CLI remains running.

See the [ACP setup guide](../README.md) for client configuration and compatibility.
