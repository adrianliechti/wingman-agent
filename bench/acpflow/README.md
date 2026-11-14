# ACP coding-flow benchmark

Compare Wingman, Codex, and Claude on the same coding task through ACP. The
runner saves task outcomes, timings, token usage, and telemetry for each run.

## Prepare

You need Go, a local Wingman gateway without token authentication, and the
Wingman UI's npm dependencies. Install Codex or Claude Code as well when
measuring those agents.

From the repository root:

```bash
npm --prefix server/ui install
mkdir -p /tmp/acpflow-results
go build -o /tmp/wingman-acpflow ./cmd/wingman
```

## Run a case

```bash
go run ./bench/acpflow \
  -binary /tmp/wingman-acpflow \
  -gateway http://127.0.0.1:4242 \
  -scenario dashboard \
  -agent wingman \
  -model gpt-5.6-sol \
  -effort high \
  -output /tmp/acpflow-results/wingman-dashboard.json
```

Use `-agent codex` or `-agent claude` to measure an installed CLI through its
Wingman gateway backend. Choose a model supported by that agent and gateway.

| Scenario | Task |
|----------|------|
| `dashboard` | Build a React finance dashboard, then add a chart range selector |
| `invoice` | Run a smaller coding smoke test |
| `invoice_dense` | Run the invoice task with guidance to batch work |
| `refactor_move` | Move a file to another package, delete a file, fix a rounding bug |
| `concurrency_bug` | Find and fix a deadlock and an eviction bug under `go test -race` |
| `plan_then_build` | Plan a fix, then implement it in a second turn |
| `bug_review` | Review an uncommitted diff with two seeded defects |
| `file_ops` | Rename a package directory, rename a file, remove a package |
| `mode_switch` | Plan, implement, plan again; only the session mode forbids edits |
| `agent_fanout` | Run three background subagents, then combine their reports |
| `fork_context` | Hand work to a subagent that inherits the conversation |
| `image_read` | Describe an image read from the workspace |
| `unattended` | Finish a request that invites a question, without asking |
| `slash_review` | Review the seeded diff through `/review` |

The dashboard runs two turns in one disposable repository and checks a
production build after each turn. Dependencies come from
`server/ui/node_modules`; the agent is instructed to use them without changing
dependency versions or accessing the network.

The scenarios from `file_ops` down each target one harness capability. Their
prompts state the task and never name a tool or a mode rule, and their checks
look only at the resulting workspace and answer. `mode_switch` and `unattended`
need Wingman's session modes; the others run with any agent.

`-scenario` also takes a comma-separated list, or `suite` for every Go
scenario. A list or suite writes one result per scenario into the `-output`
directory; `-repeat N` runs each scenario N times.

## Read the results

`-output` writes the run JSON and a neighboring `.otel.json` telemetry file.
It also generates comparison JSON and HTML from matching result pairs in the
same directory. Open the HTML directly in a browser or print it to PDF.

Use `-telemetry-output PATH` to choose a telemetry path, `-report PATH` to
choose the HTML path, or `-report none` to skip report generation.

The runner also places itself between the agent and the gateway and stores
every model request and response in a neighboring `.requests.jsonl` file. From
it, each result gets a `behavior` section: model requests (all, and those of
the lead conversation), input, cached, and output tokens, tool calls and
failed tool calls by name, and the full call list with arguments and output.
`signals` counts the choices the scenarios are about:

| Signal | Meaning |
|--------|---------|
| `edit_move`, `edit_delete` | Files moved or deleted through `edit` |
| `shell_file_op` | Shell commands that move or remove files |
| `agent_background`, `agent_fork_context` | Subagents started in the background, or with the conversation |
| `plan_blocked`, `unattended_blocked` | Calls the harness refused because of the session mode |

The requests file contains prompts and tool output; keep it with the run
artifacts. `-record none` turns recording off, `-record PATH` chooses the file.

Regenerate a report from saved runs without making model requests:

```bash
go run ./bench/acpflow \
  -report-only /tmp/acpflow-results \
  -effort high \
  -report /tmp/acpflow-results/comparison.html
```

## Compare runs

Run cases sequentially against the same gateway, model, and effort. Concurrent
runs compete for gateway capacity and affect timings. Use `high` for a typical
coding comparison and keep output for different experiments in separate
directories.

## Compare two builds

A change to a prompt, a tool description, or the harness can leave every
scenario passing and still change how the model works. To see that, run the
same scenarios against a binary built from each revision and compare the
behavior, not only the outcome:

```bash
git worktree add /tmp/wingman-base main
(cd /tmp/wingman-base && go build -o /tmp/wingman-base-bin ./cmd/wingman)
go build -o /tmp/wingman-change-bin ./cmd/wingman

for build in base change; do
  go run ./bench/acpflow \
    -binary /tmp/wingman-$build-bin \
    -scenario file_ops,mode_switch,agent_fanout \
    -repeat 3 -model gpt-5.6-sol -effort medium \
    -report none -output /tmp/acpflow-results/$build
done

go run ./bench/acpflow -summary /tmp/acpflow-results/base,/tmp/acpflow-results/change
```

The summary prints one row per scenario, model, and directory: pass rate,
median duration, model requests, tokens, the share of input read from cache,
failed tool calls, and the summed signals. Models vary from run to run, so
repeat each scenario and several models before drawing a conclusion; read the
call list of a run that looks odd.

Run `go run ./bench/acpflow -help` for timeouts and other options. The timeout
covers the complete scenario; reaching it stops the wrapper and its child CLI.
