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
- Harbor is installed (`uv tool install harbor`).
- The selected model is reachable through Wingman's OpenResponses-compatible
  endpoint configuration.

The descriptor is pinned to a released binary for reproducibility. Update its
`version` and archive URLs together when benchmarking a newer Wingman release.

## Smoke test

Run from the repository root so Harbor can import `bench.tbench.agent`:

```bash
export OPENAI_API_KEY="..."

harbor run \
  --path /path/to/a/harbor-task \
  --agent bench.tbench.agent:WingmanAgent \
  --model openai/gpt-5.4 \
  --ae OPENAI_API_KEY="$OPENAI_API_KEY" \
  --ae OPENAI_DEFAULT_MODEL="gpt-5.4"
```

For a Wingman server or another OpenResponses-compatible gateway, pass the
corresponding variables instead:

```bash
harbor run \
  --path /path/to/a/harbor-task \
  --agent bench.tbench.agent:WingmanAgent \
  --model openai/gpt-5.4 \
  --ae WINGMAN_URL="$WINGMAN_URL" \
  --ae WINGMAN_TOKEN="$WINGMAN_TOKEN" \
  --ae WINGMAN_MODEL="gpt-5.4"
```

After a single task succeeds, run the official dataset with the same agent and
environment arguments:

```bash
harbor run \
  --dataset terminal-bench/terminal-bench-2 \
  --agent bench.tbench.agent:WingmanAgent \
  --model openai/gpt-5.4 \
  --ae OPENAI_API_KEY="$OPENAI_API_KEY" \
  --ae OPENAI_DEFAULT_MODEL="gpt-5.4" \
  --n-concurrent 4
```

Harbor captures ACP events and creates an ATIF `trajectory.json` alongside the
normal task reward and verifier logs. `auth_policy` is disabled because Wingman
uses environment credentials; `permission_mode` is set to `allow` because each
run is already isolated in a disposable benchmark container.
