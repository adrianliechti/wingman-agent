# Run Terminal-Bench

Wingman runs Terminal-Bench tasks through Harbor's ACP runner. Each task uses
its own disposable container and verifier. Harbor installs the Wingman release
selected in [agent.json](agent.json).

## Prepare

Install Docker, `uv`, and Task. Make sure Docker is running and set either
`WINGMAN_URL` with an optional `WINGMAN_TOKEN`, or `OPENAI_API_KEY` with an
optional `OPENAI_BASE_URL`. The endpoint must be reachable from task containers.

The Taskfile installs its pinned Harbor dependencies in an isolated Python 3.12
environment automatically.

## Run tasks

From the repository root, run a quick check with the
`terminal-bench/openssl-selfsigned-cert` task:

```bash
task -t bench/Taskfile.yml quick
```

Override the model for your provider:

```bash
task -t bench/Taskfile.yml quick MODEL=openai/gpt-5.4 WINGMAN_MODEL=gpt-5.4
```

Run the full Terminal-Bench 2 dataset with four concurrent trials:

```bash
task -t bench/Taskfile.yml tbench CONCURRENCY=4
```

The `default` target skips Windows, nested-virtualization, and selected heavy
tasks for a lighter local run. Use `INCLUDE`, `EXCLUDE`, and `LIMIT` to select
other subsets. For example:

```bash
task -t bench/Taskfile.yml tbench INCLUDE=openssl-selfsigned-cert LIMIT=1
```

Keep `AGENT_TIMEOUT_MULTIPLIER=1` for comparable results. Lower values reduce
the agent's execution budget for exploratory runs.

## Results and configuration

Results go under `bench/jobs`, including rewards, verifier logs, ACP events,
and an ATIF `trajectory.json`.

Runs approve actions automatically in the disposable container and answer
questions using defaults. Required free-text questions are declined. The
[instructions](instructions.md) apply to every task and ask the agent to check
its deliverables within the container's resource limits.

To measure another released Wingman version, update the version, archive URLs,
and checksums together in `agent.json`. The adapter pins the container-side
ACP Python SDK to 0.10.1 for Harbor 0.18 compatibility.
