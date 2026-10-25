# Run Terminal-Bench 4

Wingman runs Terminal-Bench tasks through Harbor's ACP runner. Each task uses
its own disposable container and verifier. Harbor installs the Wingman release
selected in [agent.json](agent.json).

The dataset is pinned to `terminal-bench/terminal-bench@4.0.0`. Following the
[official run guide](https://www.tbench.ai/run), full runs use Modal because
the dataset includes GPU tasks. Local Docker is available for CPU-only subsets.

## Prepare

Install `uv` and Task, then prepare Harbor and authenticate Modal:

```bash
task -t bench/Taskfile.yml setup
bench/.venv/bin/modal token new
```

Alternatively, set `MODAL_TOKEN_ID` and `MODAL_TOKEN_SECRET`. For local runs,
install Docker and make sure it is running; Modal credentials are unnecessary
when `ENVIRONMENT=docker`.

Set either `WINGMAN_URL` with an optional `WINGMAN_TOKEN`, or `OPENAI_API_KEY`
with an optional `OPENAI_BASE_URL`. The endpoint must be reachable from the
selected sandbox, including from Modal for cloud runs.

The Taskfile installs Harbor 0.24.0 with its Modal dependencies in an isolated
Python 3.12 environment automatically, using `bench/uv.lock`.

## Run tasks

From the repository root, run one attempt of the CPU-only
`terminal-bench/html-js-filter` task on Modal:

```bash
task -t bench/Taskfile.yml quick
```

Override the model for your provider:

```bash
task -t bench/Taskfile.yml quick MODEL=openai/gpt-5.4 WINGMAN_MODEL=gpt-5.4
```

Run the full Terminal-Bench 4 dataset on Modal with four concurrent trials
and five attempts per task, as in the official guide:

```bash
task -t bench/Taskfile.yml tbench CONCURRENCY=4 ATTEMPTS=5
```

The `default` target also runs the full dataset on Modal. Defaults are one
concurrent trial and one attempt per task. Use `INCLUDE`, `EXCLUDE`, and `LIMIT`
to select subsets. For example:

```bash
task -t bench/Taskfile.yml quick ENVIRONMENT=docker
```

`DATASET` can override the pinned dataset reference for comparisons with other
versions.
GPU tasks require a sandbox with GPU access; local Docker on macOS cannot run
the complete dataset.

Keep `AGENT_TIMEOUT_MULTIPLIER=1` for comparable results. Lower values reduce
the agent's execution budget for exploratory runs.

## Compare Wingman and Claude CLI

Run the same two CPU tasks with Wingman 0.19.2 and Claude CLI 2.1.310:

```bash
task -t bench/Taskfile.yml compare
```

The [comparison config](compare.yaml) uses Claude Sonnet 5.5 with medium
reasoning effort for both agents, one attempt per task, and a five-minute
agent budget per task. Trials run sequentially on Docker. The selected tasks
are `bun-sourcemap-leak` and `wal-recovery-ordering`. These shortened budgets
are for checking the integration and exploring behavior; they do not produce
full-budget Terminal-Bench scores.

The comparison's [Docker overlay](docker-smoke.yaml) uses Docker's existing
bridge for these single-service tasks and their separate verifiers. It avoids
allocating new Compose networks on hosts with exhausted address pools.

Both agents connect to the same Wingman gateway at
`http://host.docker.internal:4242` by default. Override `WINGMAN_BENCH_URL` and
`WINGMAN_BENCH_TOKEN` in the host environment for another gateway. This endpoint
must support both Wingman's model API and Claude's Anthropic API. No separate
Claude login is required when using the gateway.

To validate the configuration without starting trials, run:

```bash
PYTHONPATH=. bench/.venv/bin/harbor run --config bench/tbench/compare.yaml --dry-run --yes
```

## Results and configuration

Results go under `bench/jobs`, including rewards, verifier logs, ACP events,
and an ATIF `trajectory.json`.

Runs approve actions automatically in the disposable container and answer
questions using defaults. Required free-text questions are declined. The
[instructions](instructions.md) apply to every task and ask the agent to check
its deliverables within the container's resource limits.

To measure another released Wingman version, update the version, archive URLs,
and checksums together in `agent.json`. The adapter uses Harbor's ACP dependency
installer and cached-token reporting directly.

Inspect results with:

```bash
bench/.venv/bin/harbor view bench/jobs
```
