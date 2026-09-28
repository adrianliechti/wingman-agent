# Model prompt maintenance

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
explicit-request-only testing policy. Plan guidance and the independent unattended
policy append Wingman's shared templates to the selected model's instructions.
Plan's read-only restrictions take precedence over implementation guidance.

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

## Claude Sonnet 5.5

`models/claude-sonnet-5-5/mode_agent.txt` adapts the effective Claude Code prompt
captured on 2026-09-28 with `--model claude-sonnet-5-5`. The source is Claude Code
2.1.284, release commit `2b8ce618c24de26410e4bdfc4e1d592accd61f61`, built on
2026-09-28. Its temporary Darwin ARM64 binary was downloaded from Anthropic's
[release distribution](https://downloads.claude.ai/claude-code-releases/2.1.284/darwin-arm64/claude)
and checked against the
[release manifest](https://downloads.claude.ai/claude-code-releases/2.1.284/manifest.json).
SHA-256: `50a14c2f50f56668380fdda490167f1d3630d5cc18fb8aed3073c2c7ea7314fe`.

Capture the first Messages API request's `system` blocks using a localhost mock
API and a dummy API key, an empty working directory and `CLAUDE_CONFIG_DIR`,
`--setting-sources ""`, `--strict-mcp-config --mcp-config '{"mcpServers":{}}'`,
`--settings '{"disableAllHooks":true,"autoMemoryEnabled":false}'`, and
`--disable-slash-commands --no-session-persistence --max-turns 1 -p`.
Disable nonessential traffic and automatic updates. This extracts the CLI's
rendered instructions without asking a model to reproduce them or making an
inference request to Anthropic. The same capture with `claude-sonnet-5` confirms
that the two models select different prompts in this release.

Sonnet 5.5 selects a lean prompt containing harness guidance, code style,
careful actions, truthful reporting, context continuation, and acting on
established information. Preserve this structure rather than inheriting the
general Claude prompt's task and output sections or Opus's communication,
delivery, and correction sections.

Wingman's adaptation replaces the product identity and dedicated tool names,
describes its appended session-context messages instead of Claude Code's
mid-conversation system turns, and retains Wingman's existing software-security
and explicit-request-only Git rules. Billing metadata, the Claude model/product
catalog, token countdown, and unrelated identity guidance are omitted. Project,
skills, memory, and environment sections still come from the shared templates.
The existing longest-prefix router selects this variant for Sonnet 5.5,
including normalized, provider-prefixed, and dated IDs; plan and unattended
instructions still use the shared modes.

Run `go test ./pkg/code/prompt ./pkg/code/agent` to check routing, rendering,
and session integration. These checks do not measure Sonnet 5.5's behavior.
