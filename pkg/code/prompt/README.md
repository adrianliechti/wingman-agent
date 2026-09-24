# OpenAI prompt maintenance

Wingman's GPT prompts adapt Codex instructions to Wingman's tools and session
lifecycle. Compare the effective model template, not just a file named
`*_prompt.md`: current Codex renders `model_messages.instructions_template` from
`codex-rs/models-manager/models.json` and supplies permissions, collaboration,
project instructions, and environment context separately.

## Comparison baseline

Reviewed on 2026-09-24 against the local Codex checkout at
`83b56bc5ad9489a47a0eb3edc3e5852f597125be`. The GPT-6 Astra/Sol/Luna,
GPT-5.6 Sol, GPT-5.5, and GPT-5.4 catalog templates are unchanged from
`40eac3ce8a`, the previously recorded GPT-6 adaptation source.

| Wingman prompt | Codex reference |
| --- | --- |
| `models/gpt-6-{astra,sol,luna}` | Matching catalog model's `instructions_template` |
| `models/gpt` | GPT-5.6 Sol catalog template; shared fallback for other GPT models |
| `models/gpt-5-5`, `models/gpt-5-4` | Matching catalog model's `instructions_template` |
| `models/gpt-5-1`, `models/gpt-5-2` | Historical `core/gpt_5_1_prompt.md` and `core/gpt_5_2_prompt.md` |
| `models/gpt-5-4-mini` | Retained separate adaptation; this checkout has no matching catalog entry |

Model routing uses the longest normalized prefix in `VariantFor`. Preserve
distinct variants and their model-specific guidance, including GPT-6 Luna's
explicit-request-only testing policy. Plan mode and unattended mode use Wingman's
shared templates.

## Intentional harness adaptations

- Use Wingman's `grep`, `glob`, `read`, `edit`, `exec_command`, `exec_session`,
  and `agent` contracts. Do not import Codex-only tools or argument formats.
- `elicit` blocks. Resolve discoverable questions first, assume when a choice is
  cheap to correct, and ask only for a blocking user decision. Keep question
  formatting in the tool description instead of duplicating it in each prompt.
- Tool-execution approvals belong to Wingman's harness. Do not import Codex
  approval-mode names or its automatic reviewer instructions.
- Background command exits can arrive as notifications. Use bounded waits and
  avoid polling solely to discover an exit when notifications are available.
- Status questions and compaction preserve unfinished work. Hand off when the
  requested outcome is complete or a real blocker prevents further progress.
- Send useful progress updates; avoid thought-length-based update rules. Leave
  blank lines around Markdown headings and lists for consistent rendering.

`BuildBaseInstructions` contains model/mode guidance; `BuildSessionContext`
contains project instructions, skills, memory, and environment data. The harness
appends changed context snapshots to history without rewriting its cached prefix.
Keep these shared sections out of individual model prompts.

The clarification and validation changes also follow the official
[GPT-6 prompting guidance](https://developers.openai.com/api/docs/guides/latest-model/gpt-6-astra.md#prompting-best-practices):
calibrate initiative and testing to the workload and available tools.

## Validation

Run `go test ./pkg/code/prompt ./pkg/code/agent` for template routing, rendering,
mode selection, and session integration. These checks do not measure model
behavior. Use matched model/effort runs of `bench/acpflow` for behavioral or
latency comparisons, and include clarification and post-compaction continuation
cases when evaluating these instructions.

This review reduced the nine GPT prompt files from 112,092 to 108,400 UTF-8 bytes
in aggregate (3.3%). This is a text-size measurement, not a measured token,
latency, cost, or task-success improvement.
