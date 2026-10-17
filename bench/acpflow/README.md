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

The dashboard runs two turns in one disposable repository and checks a
production build after each turn. Dependencies come from
`server/ui/node_modules`; the agent is instructed to use them without changing
dependency versions or accessing the network.

## Read the results

`-output` writes the run JSON and a neighboring `.otel.json` telemetry file.
It also generates comparison JSON and HTML from matching result pairs in the
same directory. Open the HTML directly in a browser or print it to PDF.

Use `-telemetry-output PATH` to choose a telemetry path, `-report PATH` to
choose the HTML path, or `-report none` to skip report generation.

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

Run `go run ./bench/acpflow -help` for timeouts and other options. The timeout
covers the complete scenario; reaching it stops the wrapper and its child CLI.
