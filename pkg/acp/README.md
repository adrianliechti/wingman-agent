# Use Wingman with an ACP client

Connect an editor or another client that supports Agent Client Protocol (ACP)
to Wingman over stdio. Configure the client to launch `wingman` with the
arguments `acp` and use your project directory as its working directory.

```json
{
  "command": "wingman",
  "args": ["acp"]
}
```

This is the command entry to adapt to your client's configuration format.
The built-in agent uses the [provider settings](../../README.md#configure-your-provider)
from the environment inherited by the client.

## Use an installed agent

Wingman can also bridge installed agents to ACP:

```bash
wingman acp claude
wingman acp codex
wingman acp pi
```

The default `native` backend uses that CLI's login and configuration. Install
and sign in through the agent's own CLI before connecting. To route it through
a Wingman gateway instead:

```bash
wingman acp codex --backend wingman
wingman acp claude --backend wingman
wingman acp pi --backend wingman
```

The gateway defaults to `http://localhost:4242`. Set `WINGMAN_URL` and, if needed,
`WINGMAN_TOKEN` in the client environment to use another gateway.

Claude and Codex also accept `--model ID` and `--effort LEVEL`. Run
`wingman acp --help` for supported options. See the [Claude](claude/README.md)
and [Codex](codex/README.md) guides for agent-specific behavior.

## Compatibility and troubleshooting

Wingman supports ACP version 1. The client must accept that version. Available
model settings, plan controls, forms, and approvals depend on the selected
agent and the client's capabilities.

If the client cannot launch Wingman, use an absolute executable path and check
its working directory and environment. If cancelling a stalled turn closes the
backend connection, reconnect before sending another prompt.

To use installed agents directly in Wingman's own terminal or browser UI, see
[Get started](../../README.md#get-started).
