# Structured output from a script

Install and configure Wingman, then run this example from the repository you
want it to inspect. Use the example script's full path if you are outside the
Wingman source checkout:

```sh
/path/to/wingman-agent/examples/non-interactive/run.sh
```

The script runs unattended, prints the final JSON to stdout, and saves it as
`project.json` beside the script. Wingman validates the response against
[project.schema.json](project.schema.json).

To use a schema directly:

```sh
wingman exec --schema /path/to/project.schema.json "Extract project metadata"
```

For JSON without a schema:

```sh
wingman exec --json "Summarize this repository" | jq
```

`exec` approves actions automatically. See the [main README](../../README.md#run-a-prompt-from-a-script)
for piping context and resuming a run.
