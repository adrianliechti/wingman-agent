# Terminal-Bench compatibility

Wingman runs in Terminal-Bench task containers through Harbor's generic ACP
installed-agent runner. Harbor downloads the pinned Wingman release described in
`agent.json`, starts `wingman acp` over stdio, sends it the task instruction, and
runs the task verifier after Wingman finishes.

This deliberately installs Wingman into each task's own image. Do not use the
repository's general-purpose Dockerfile as the Terminal-Bench environment:
Terminal-Bench tasks provide their own images, files, tools, and services.

## Prerequisites

- Docker is installed and running.
- `uv` and Task are installed. The benchmark Taskfile creates an isolated
  Python 3.12 environment and installs the pinned Harbor version.
- The selected model is reachable through Wingman's OpenResponses-compatible
  endpoint configuration.

The descriptor is pinned to a released binary for reproducibility. Update its
`version` and archive URLs together when benchmarking a newer Wingman release.

## Quick benchmark

Run from the repository root. The default target uses the available Wingman
server credentials, falling back to `OPENAI_API_KEY` when `WINGMAN_URL` is not
set:

```bash
task -t bench/Taskfile.yml quick
```

Override the model when needed:

```bash
task -t bench/Taskfile.yml quick MODEL=openai/gpt-5.4 WINGMAN_MODEL=gpt-5.4
```

The quick target runs only Terminal-Bench's
`terminal-bench/openssl-selfsigned-cert` task. Run the full official dataset
with configurable concurrency using:

```bash
task -t bench/Taskfile.yml tbench CONCURRENCY=4
```

For a bounded exploratory run, reduce only the agent execution budget (for
example, `AGENT_TIMEOUT_MULTIPLIER=0.25`). Keep the default `1` for comparable
benchmark results.

Harbor captures ACP events and creates an ATIF `trajectory.json` alongside the
normal task reward and verifier logs. `auth_policy` is disabled because Wingman
uses environment credentials; `permission_mode` is set to `allow` because each
run is already isolated in a disposable benchmark container.

`instructions.md` adds brief generic unattended-run guidance: it authorizes use
of network access already exposed by the task and asks the agent to keep
self-tests within the container's resource limits. It contains no task-specific
hints and is appended to every benchmark task.
The Taskfile also sets `WINGMAN_ELICITATION=accept`. Compatible Wingman releases
automatically answer boolean yes/no fields affirmatively and use explicit
defaults; required free-text questions are cancelled rather than fabricated.

The adapter pins the container-side `agent-client-protocol` package to 0.10.1.
Harbor 0.18's ACP runner still uses that SDK's `set_session_model` API, which was
removed in ACP Python SDK 0.11. The pin can be removed once Harbor migrates its
runner to `session/set_config_option`.
