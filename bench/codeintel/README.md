# Code intelligence benchmarks

Compare search, code graph, and language-server tools on reference lookup,
rename, and call-path tasks. Model runs use a configured inference provider and
make model requests.

## Run the model matrix

From the repository root, install managed `gopls`, configure your provider, and
select the model to measure:

```bash
WINGMAN_LSP_EVAL_MODEL=gpt-6.1-sol go test ./bench/codeintel \
  -run '^TestModelToolMatrix$' -count=1 -parallel=2 -timeout=30m -v \
  -codeintel-repetitions=3 -codeintel-report=/tmp/codeintel.json
python3 bench/codeintel/report.py /tmp/codeintel.json
```

The matrix uses low reasoning effort. Keep the model, fixtures, and repetition
count consistent when comparing runs. Model latency and provider caches can
vary between runs.
