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

Pi launches set `PI_OFFLINE=1`, `PI_SKIP_VERSION_CHECK=1`, and `PI_TELEMETRY=0`
for both backends. These suppress startup update/package/catalog requests and
install telemetry while allowing normal model requests. Native launches keep
their existing Pi configuration directory; gateway launches use Wingman's Pi
directory.

## Compatibility and troubleshooting

All four commands support ACP v1 and experimental ACP v2 on the same executable.
The initial `initialize.protocolVersion` selects the highest supported version
at or below the client's version: `1` selects v1; `2` or later selects v2.
Different connections can use different versions concurrently. A connection
keeps its negotiated version for its lifetime.

V2 follows the draft shipped with the ACP TypeScript SDK 2.2. Prompt responses
acknowledge accepted input; `state_update` reports completion and cancellation.
Modes appear in `configOptions`. To replay history, use `session/resume` with
`replayFrom: {"type":"start"}` after the active turn finishes. Omitting
`replayFrom` resumes without replay. Other replay cursors are rejected.

Available model settings, plan controls, forms, and approvals depend on the
selected agent and the client's capabilities. Wingman's internal agent-host
adapter continues to use v1.

If the client cannot launch Wingman, use an absolute executable path and check
its working directory and environment. If cancelling a stalled turn closes the
backend connection, reconnect before sending another prompt.

To use installed agents directly in Wingman's own terminal or browser UI, see
[Get started](../../README.md#get-started).

## Protocol tests

Run the bridge and agent-host regressions with:

```bash
go test ./pkg/acp/... ./pkg/code/... ./pkg/external/... ./server
```

The [official ACP TCK](https://github.com/agentclientprotocol/acp-tck) exercises
stdio framing, negotiation, sessions, streaming, cancellation, and replay. The
runner pins the TCK revision and runs both versions against all four bridges:

```bash
# Real bridges with deterministic backend fixtures; requires Go, uv, curl, tar.
scripts/test-acp-tck.sh

# Built wingman executable and installed CLIs, using their native settings.
# Sends real prompts and creates sessions; optional logout tests stay disabled.
scripts/test-acp-tck.sh --live

# Route every live command through a Wingman gateway.
WINGMAN_URL=http://localhost:4242 ACP_TCK_BACKEND=wingman \
  scripts/test-acp-tck.sh --live
```

Append `claude`, `codex`, `pi`, or `server` to select bridges. Set
`ACP_TCK_REPORT_DIR` to retain JSON reports and logs at a chosen path, or
`ACP_TCK_DIR` to use an existing TCK checkout. `ACP_TCK_VERSIONS=2` selects
just v2; the default is `1 2`. Live runs inherit model and
provider settings, including `WINGMAN_MODEL` and the `WINGMAN_*_PATH` CLI
overrides. The TCK is experimental; its reports describe the checks exercised,
not an official certification. Fixture backends do not launch external MCP
servers, so their advisory MCP-connection check requires the live runs.
