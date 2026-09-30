# Model prompt maintenance

Wingman's GPT prompts adapt Codex instructions to Wingman's tools and session
lifecycle. Compare the effective model template, not just a file named
`*_prompt.md`: current Codex renders `model_messages.instructions_template` from
`codex-rs/models-manager/models.json` and supplies permissions, collaboration,
project instructions, and environment context separately.

## Comparison baseline

Reviewed all current GPT catalog variants on 2026-09-30 against Codex commit
`92bc601ad60542c92bf0bb1e7a2eb70b84ac49d2` using its pinned
[model catalog](https://github.com/openai/codex/blob/92bc601ad60542c92bf0bb1e7a2eb70b84ac49d2/codex-rs/models-manager/models.json).
All base instruction templates match the previous 2026-09-29 baseline at
`b1e72963c3b71a9265a551e54beff078384efed9`, including GPT-6.1 Sol. The embedded
external catalog now preserves the full new upstream entries, including updated
descriptions, priorities, and auto-review metadata. Historical GPT-5.4 remains
available with its retained entry. GPT prompt text required no changes.

The prior September 29 review introduced GPT-6.1 Sol and compared GPT-6,
GPT-5.6, GPT-5.5, and historical GPT-5.1/GPT-5.2 against
`83b56bc5ad9489a47a0eb3edc3e5852f597125be`.

| Wingman prompt | Codex reference |
| --- | --- |
| `models/gpt-6-1-sol` | GPT-6.1 Sol catalog template; Astra-style structure with its own writing and correction guidance |
| `models/gpt-6-{astra,sol,luna}` | Matching catalog model's `instructions_template` |
| `models/gpt` | Identical GPT-5.6 Sol/Terra/Luna catalog templates; shared fallback for other GPT models |
| `models/gpt-5-5` | GPT-5.5 catalog template |
| `models/gpt-5-4` | Retained GPT-5.4 template from the previous review; current upstream catalog removed this model |
| `models/gpt-5-1`, `models/gpt-5-2` | Historical `core/gpt_5_1_prompt.md` and `core/gpt_5_2_prompt.md` |
| `models/gpt-5-4-mini` | Retained separate adaptation; this checkout has no matching catalog entry |

The review adopts GPT-6's structured multiline command-body guidance, restores
the GPT-5.6 progress-update cadence, and restores concise GPT-5.5/GPT-5.4
collaboration guidance omitted from the earlier adaptations. GPT-6.1 Sol keeps
its added guidance on direct wording, avoiding unnecessary apologies, and
acknowledging meaningful mistakes. GPT-5.1, GPT-5.2, and GPT-5.4 Mini retain
their existing adaptations; there is no revised upstream source to adopt.

Model routing uses the longest normalized prefix in `VariantFor`. Preserve
distinct variants and their model-specific guidance, including GPT-6 Luna's
explicit-request-only testing policy. Plan guidance and the independent unattended
policy append Wingman's shared templates to the selected model's instructions.
Plan's read-only restrictions take precedence over implementation guidance.

GPT-6.1 Sol uses Codex's `low` effort default and `low` verbosity. Its native
effort choices are `low`, `medium`, `high`, `xhigh`, and `max`, as supported by the
[OpenAI API model documentation](https://developers.openai.com/api/docs/models/gpt-6.1-sol).
Codex's `ultra` orchestration setting is not a native API reasoning effort.
The full upstream GPT-6.1 Sol entry is copied into the embedded external Codex
catalog; existing entries, including GPT-5.4, are retained.

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
- Keep personality guidance focused on collaboration and judgment. Omit
  product identity, claims about an inner life, and unrelated stylistic bans.
- Codex's optional persistent-mode prompt depends on its async messaging and
  scheduling tools. Keep Wingman's session and unattended-mode contracts.

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

For model catalog changes, also run `go test ./pkg/model ./pkg/external/codex
./pkg/acp/codex ./pkg/agent` to cover discovery, backend IDs, Codex metadata,
and request construction. Prompt text changes still require matched behavioral
evaluations before claiming task-success or latency improvements.

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

## Claude Code review on September 30

Captured the rendered requests for Claude Sonnet 5/5.5, Opus 4.8/5/5.5,
Fable 5/5.1, and Mythos 5/5.1 with Claude Code 2.1.285. The installed Darwin
ARM64 binary matches the official
[release manifest](https://downloads.claude.ai/claude-code-releases/2.1.285/manifest.json):
commit `afb212976052ab038df25d5e871f6c049e094d3b`, built September 29,
SHA-256 `51f09bd1e021d9fa8a1864c179799bd37cb39962a937935c5cf6823398e86db4`.
The capture uses the isolation described above, default CLI tools, a local mock
API, and a dummy credential. It makes no model inference requests. The source
captures and a dated report are retained in `../wingman-ops/runs/2026-09-30`.

| Prompt variant | Result |
| --- | --- |
| `claude-sonnet-5-5` | Retain the lean adaptation; current capture confirms the same structure. |
| `claude-opus-5-5` | Refresh to its current lean prompt; remove the older communication, delivery, and correction sections absent from this capture. |
| `claude-fable-5-1`, `claude-mythos-5-1` | Add separate adaptations with delivery and writing guidance; retain the distinct Fable identity guidance. |
| `claude-fable-5`, `claude-mythos-5` | Retain their older communication structure; leave unattended assumptions in the shared mode. |
| `claude-opus-4-8` | Retain its lean adaptation and intentional pre-mutation state check. |
| `claude-opus-5`, general `claude` | Retain the existing model-specific and general adaptations. |

Translate Claude's system-turn/reminder wording to Wingman's appended session
context, without granting ordinary tool results instruction authority. Keep
software-security guidance and explicit-request-only Git rules. Omit billing,
product catalogs, token countdowns, and unrelated identity guidance. The new
5.1 variants do not assume every session is unattended; the shared unattended
and plan modes remain the policy owners. Longest-prefix routing distinguishes
5.1 from 5, including provider-prefixed, normalized, dated, and tagged IDs.

Validate routing, rendering, and mode composition with the existing focused
Go suites. These checks do not establish a behavioral or latency improvement;
matched live model evaluations were not part of this refresh.
