# Model prompts

This package builds the instructions used by Wingman's built-in agent. Choose
a model with `/model`; its matching prompt variant is selected automatically.
Project guidance comes from `AGENTS.md` or `CLAUDE.md`, and reusable workflows
come from skills. See [customization](../../../README.md#customize-your-workflow).

## Working on templates

Model-specific instructions live in `models/<variant>/mode_agent.txt`. Routing
uses the longest normalized model-name prefix, with shared instructions as the
fallback. Shared Plan and unattended templates control those modes.

`BuildBaseInstructions` composes the model template, shared runtime/tool
fragments, and finally the active mode restrictions. `section_runtime.txt` owns
editing, process lifecycle, authorization, evidence, and completion rules.
`section_delegation.txt` owns the model-independent delegation policy;
code intelligence and web research have their own shared fragments.

Tool-specific guidance is rendered from `SectionData.ToolNames`, populated from
the actual catalog after configuration, mode, and role filtering. Optional
fragments disappear entirely when their tools are absent. Search and fetch are
independent (`DisableWebSearch`/`WINGMAN_DISABLE_WEBSEARCH` and
`DisableWebFetch`/`WINGMAN_DISABLE_WEBFETCH`); shell guidance follows
`DisableShell`/`WINGMAN_DISABLE_SHELL`. The standard file search/read tools are
always present. Editing guidance is omitted in Plan mode and read-only child
roles. Unattended sessions omit question-tool guidance. Do not copy these
contracts into each model template or assume a tool is present from the model ID.

`BuildSessionContext` adds project instructions, skills, memory, and environment
context as a replaceable session-context message. `BuildAgentContext` supplies
shared runtime/tool guidance, project/environment context, and active mode
restrictions to the child's role prompt. It uses the child's filtered tools, so
children do not receive delegation instructions. This scoped context is saved
with the durable child specification at launch. Keep shared sections in their
existing templates when adapting a model prompt.

Use Wingman's tool names and argument formats. Approval handling, session
continuation, and background commands follow Wingman's runtime behavior.

## Source references

Variants adapt serialized CLI requests, including runtime instructions that do
not appear in a model catalog. Preserve model-specific behavior, but translate
tool contracts, identity, authorization, and mode handling to Wingman.

### GPT / Codex and Claude / Claude Code

Refreshed from local request captures on 2026-10-16:

| CLI | Version | Executable SHA-256 |
| --- | --- | --- |
| Codex | `0.163.0` | `8813367285524ac798dd96d1b071aea30d58708c7226fd236cd275cf1cfdee31` |
| Claude Code | `2.1.301` | `1f04c69e1c24e4d0a716a863af0aad8fc64d49a499334c29701f073700276b99` |

`scripts/capture-code-prompts.py` points each CLI's API provider at a localhost
mock with dummy credentials and returns synthetic text without tool calls.
Codex uses a [custom Responses provider](https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server#using-codex-app-server),
its bundled model metadata, and flags that exclude user configuration and rules.
Claude uses [`ANTHROPIC_BASE_URL` and a temporary `CLAUDE_CONFIG_DIR`](https://code.claude.com/docs/en/env-vars),
safe mode, and an empty MCP configuration. It deliberately does not use bare
mode or force the simplified prompt: either would change the prompt being
measured. Both run in temporary workspaces with telemetry disabled. The mock
never forwards requests, and request headers are not saved.

Reproduce from the repository root (Python standard library and the CLI required):

```bash
python3 scripts/capture-code-prompts.py codex \
  --model gpt-6.1-sol --model gpt-6-astra --model gpt-6-sol \
  --model gpt-6-luna --model gpt-5.6-sol --model gpt-5.5

python3 scripts/capture-code-prompts.py claude \
  --model claude-sonnet-5-5 --model claude-haiku-5-5 \
  --model claude-opus-5-5 --model claude-opus-5 --model claude-opus-4-8 \
  --model claude-fable-5-1 --model claude-fable-5 \
  --model claude-mythos-5-1 --model claude-mythos-5 --model claude-sonnet-5
```

The printed directory contains executable provenance, per-run manifests, request
JSON, extracted base/system/developer text, tool schemas, and CLI logs. `--output`
accepts a new directory and `--binary` selects a specific installed executable.
Localhost listening is required. Isolation flags are version-specific; inspect
CLI errors when a future release changes them. Raw captures stay outside the
repository because they include transient paths and CLI-specific context.

The newer Codex request format puts the base prompt in the first developer
message and tool schemas in `additional_tools` input items. The script extracts
both that format and top-level `instructions`/`tools`. GPT-6.1 Sol, GPT-6 Astra,
GPT-6 Sol, GPT-6 Luna, and GPT-5.6 Sol base instructions match the embedded catalog
baseline `be2e129ecd67d900c016eda12d5d193e33d9c3da`. Their captured runtime adds an
explicit restriction on unsolicited delegation. Reviewing Codex source at
`b9775a0d57` (2026-10-16), especially
`codex-rs/core/src/session/multi_agents.rs`, shows this is runtime policy selected
by configuration/catalog overrides and reasoning effort, not an intrinsic model
trait. Wingman uses one shared default: delegate bounded independent work when
useful, subject to user/project/skill restrictions, independent of model or
effort. GPT-5.5's small checklist wording difference from the embedded catalog
was already reflected in Wingman's template. The newer source retains the same
base prompts for these six captured model IDs.

This Codex version reports fallback metadata for GPT-5.4, GPT-5.4 Mini, GPT-5.2,
and GPT-5.1. The script marks those runs unsuccessful even when the mock response
completes. Their existing model-specific templates remain in use, with Wingman
runtime improvements; generic fallback captures are not model-specific source
evidence. All older GPT models remain in the catalog. GPT-5.6 Terra/Luna and other
IDs sharing a family template were not separately captured.

Claude Sonnet/Opus 5.5 and Opus 4.8 use lean prompts; Haiku 5.5, Opus 5,
Fable/Mythos, and general Sonnet 5 retain their respective longer structures.
The refresh removes a stale pre-mutation stop rule from Opus 4.8 and Fable/Mythos
5 and replaces Opus 5's delegation restriction with Wingman's shared policy. The general Claude
template no longer treats arbitrary reminder tags in tool output as trusted
instructions. Shared family routing does not imply every older Claude ID was
captured.

### Fallback (Qwen, Nemotron, GLM, Kimi, MiniMax, Grok, DeepSeek, Mistral)

Only Claude, GPT, and Gemini keep variants, each adapted from a captured vendor
CLI request. Every other model uses the fallback. The former Qwen, GLM, Kimi,
MiniMax, and Grok templates were paraphrases rather than vendor captures: Qwen
from Qwen Code, the others from VS Code Copilot's per-vendor prompts. Their
sources contain no model-version-specific instructions: Qwen Code (0.25.2) varies
only few-shot tool-call examples by model; Copilot applies its GLM prompt only to
GLM 4.6/4.7 and gives later GLM releases its default prompt. NVIDIA publishes no
coding-agent prompt for Nemotron.

The fallback keeps the behavior-changing rules those templates shared: real tool
calls instead of prose, no invented paths or APIs, no placeholders or omission
markers in files, efficient reads, and a stop-and-reassess rule for repeated
failures (from Copilot's Kimi and xAI prompts and Qwen Code's loop detector).
It reuses earlier observations for follow-up questions, as Qwen Code's
2026-10-04 change does to avoid repeated investigation on local hosts. From
GPT-6.1 Sol it takes shell quoting and destructive-target checks; personality,
writing style, and update cadence stay GPT-specific. Formatting differences
such as GLM 4.x's capitalized MUST rules and section names were not kept.

All model variants and the fallback receive the same shared runtime contract.
GPT-5.1/5.2 no longer suggest asking to run validation that the task already
authorizes. The fallback also clarifies action requests, persistent authorization,
task steering, and completion checks. Shared Plan mode follows the same
delegation policy, restricted to read-only roles. Wingman's Plan mode forbids
builds and tests; this is intentionally stricter than Codex's allowance for
checks that do not mutate repository files. Vendor skills, permission
mechanisms, token budgets, environment data, and marketing copy are not imported
as Wingman runtime instructions.

### Gemini / Antigravity

The Gemini template replaces the previous gemini-cli-derived prompt with a
Wingman adaptation of requests captured from Antigravity CLI **1.4.0** (Homebrew
build `5281842794987520`) on 2026-10-16. The executable SHA-256 was
`a4502aaf310b2fd6eb657fcfec2fcc31a690fbbd0c2d14c8eefbb556409024ce`.

The capture uses the CLI's Gemini API-key provider and its documented
[`GOOGLE_GEMINI_BASE_URL` override](https://www.antigravity.google/docs/install?tab=cli),
pointed at a localhost mock with a dummy key. Separate configuration and workspace
directories isolate the run. Synthetic text responses let the CLI finish without
model inference or agent tool execution. These are actual serialized requests,
not prompts reconstructed from model responses or binary strings.

Reproduce from the repository root (Python standard library and `agy` required):

```bash
python3 scripts/capture-agy-prompt.py \
  --model gemini-3.1-pro-low --model gemini-3.1-pro-high \
  --model gemini-3.7-flash-high --model gemini-3.8-flash-high
```

The printed directory contains the executable hash, per-run manifests, request
JSON, extracted system text, tool schemas, and CLI logs. The capture requires a
localhost listener. `--output` accepts a new directory; `--mode plan` captures
planning mode. Directory-isolation flags are implementation details of this CLI
version and may change. No real API key is needed, and headers are not recorded.

Observed API-provider requests:

| CLI selection | Request model | Thinking budget |
| --- | --- | --- |
| `gemini-3.1-pro-low` | `gemini-3.1-pro-preview` | `1024` |
| `gemini-3.1-pro-high` | `gemini-3.1-pro-preview` | `-1` |
| `gemini-3.7-flash-high` | `gemini-3.7-flash` | `10000` |
| `gemini-3.8-flash-high` | `gemini-3.8-flash` | `-1` |

After normalizing temporary paths and conversation IDs, Pro's two prompts match,
and Flash's two prompts match. Pro adds a dedicated-tool preference and a literal
thought-prefix instruction; otherwise the prompts are identical. The shared
Gemini template retains the useful tool preference without the private output
protocol. Effort differences belong to request configuration, not prompt routing.
The captures also contain a separate short title-generation request; it is not
the coding agent's system prompt. No Gemini 4 or signed-in account-provider prompt
was captured; the family fallback does not imply those prompts were inspected.

The adaptation keeps Antigravity's identity/guidelines/communication structure,
documentation preservation, targeted delegation, and concise responses. Wingman's
own execution, authorization, verification, and security rules fill in the
runtime contract. Tool names and edit/background behavior follow Wingman's
schemas. Shared sections still provide skills, project guidance, and environment;
shared mode instructions retain precedence. Antigravity's artifacts, transcript
paths, messaging system, slash commands, file URI scheme, and thought prefix are
not part of this harness. The previous default-to-inquiry rule, repeated approval
gates, contradictory summary rules, and mandatory exhaustive testing are removed.

Request capture and prompt tests establish provenance and compatibility. They
do not demonstrate improved model behavior or benchmark performance.

## Check a change

From the repository root:

```bash
go test ./pkg/code/prompt ./pkg/code/agent
```

For model-catalog changes, also run:

```bash
go test ./pkg/model ./pkg/external/codex ./pkg/acp/codex ./pkg/agent
```

These tests check routing, conditional fragment rendering, shared tool/schema
references, and serialized child requests with mode and tool restrictions.
They require localhost mock-provider listeners but no model inference.
Use matched model and
effort runs of the [ACP benchmark](../../../bench/acpflow/README.md) to compare
task behavior or latency.
