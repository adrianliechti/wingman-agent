# Model prompts

This package builds the instructions used by Wingman's built-in agent. Choose
a model with `/model`; its matching prompt variant is selected automatically.
Project guidance comes from `AGENTS.md` or `CLAUDE.md`, and reusable workflows
come from skills. See [customization](../../../README.md#customize-your-workflow).

## Working on templates

Model-specific instructions live in `models/<variant>/mode_agent.txt`. Routing
uses the longest normalized model-name prefix, with shared instructions as the
fallback. Shared Plan and unattended templates control those modes. Prefix
routing gives a new minor version an older sibling's prompt without any error,
so `routing_test.go` lists the directory every Claude and GPT catalog model
must use; a new catalog model fails that test until its routing is decided.

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
`DisableShell`/`WINGMAN_DISABLE_SHELL`. With the default read roots, the file
tools can read ordinary files outside the workspace and refuse credential
stores. Hosts can supply narrower read roots. File-tool writes are confined
to the workspace and the writable roots listed in the environment. A session
with a shell is told to use it for a change outside those roots. When reads
stopped at the workspace, GPT-6.2 took a refused path as off limits and
handed the task back. Editing guidance is omitted in Plan mode and read-only
child roles. Unattended sessions omit question-tool guidance. Do
not copy these contracts into each model template or assume a tool is present
from the model ID.

The session passes `fileReadRoots` to the filesystem tools and uses the same
list to render the prompt. The default empty list permits ordinary local reads.
With a nonempty list, reads are confined to the workspace and those roots.
The `read`, `grep`, and `glob` path parameters describe that scope. `edit` and `delete` expose different argument
schemas and use the same transaction for validation, undo metadata, and rollback.
Older edit calls with a delete flag still run.

`BuildSessionContext` adds project instructions, skills, memory, and environment
context as a replaceable session-context message. `BuildAgentContext` supplies
shared runtime/tool guidance, project/environment context, and active mode
restrictions to the child's role prompt. It opens with `section_subagent.txt`,
which tells every child role who reads its result and that the workspace is
shared. It uses the child's filtered tools, so
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

The GPT templates were refreshed from local request captures on 2026-10-16;
GPT-6.1 Terra was added from a capture on 2026-11-10 and the other GPT
templates were not re-adapted then. The Claude templates were adapted on
2026-10-16, and on 2026-11-10 all eleven Claude models in the command below
were captured again and each template was compared with its capture:

| CLI | Version | Executable SHA-256 | Captured | Templates |
| --- | --- | --- | --- | --- |
| Codex | `0.163.0` | `8813367285524ac798dd96d1b071aea30d58708c7226fd236cd275cf1cfdee31` | 2026-10-16 | GPT through GPT-6.1 Sol |
| Codex | `0.167.0` | `030316ecfbb41f4e7946e584b0063ef333b7a36cd312ba349f2f6869e39b62cc` | 2026-11-10 | GPT-6.1 Terra |
| Codex | `0.169.0` | `739cda86e15d30a6e2b40cf8dcbbfb988aaf48b305b27065573bb8e9fd41d002` | 2026-11-18 | GPT-6.2 Sol and Astra, nine earlier GPT models checked |
| Claude Code | `2.1.301` | `1f04c69e1c24e4d0a716a863af0aad8fc64d49a499334c29701f073700276b99` | 2026-10-16 | Claude, first adaptation |
| Claude Code | `2.1.319` | `1f53587e92dee8e089fa349188a4cda64fa34ace6e7b0ca2ee278ddaa3cc2ebd` | 2026-11-10 | Claude, second comparison; Fable 5.2 added |
| Codex | `0.171.0` | `fc8d77cf7585aded2b9bc487dad29c4ee2f066123e4e76dd3b17653a8322ce42` | 2026-11-22 | GPT-6 templates re-adapted from the unchanged catalog text |
| Claude Code | `2.1.330` | `f7c5093b9dd6560d08af28bc27b123fb55091deae3180ac39df199522da86815` | 2026-11-22 | Claude, current |

`scripts/capture-code-prompts.py` points each CLI's API provider at a localhost
mock with dummy credentials and returns synthetic text without tool calls.
Codex uses a [custom Responses provider](https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server#using-codex-app-server),
the catalog selected by `--codex-catalog` (by default the CLI's bundled metadata),
and flags that exclude user configuration and rules.
Claude uses [`ANTHROPIC_BASE_URL` and a temporary `CLAUDE_CONFIG_DIR`](https://code.claude.com/docs/en/env-vars),
safe mode, and an empty MCP configuration. It deliberately does not use bare
mode or force the simplified prompt: either would change the prompt being
measured. Both run in temporary workspaces with telemetry disabled. The mock
never forwards requests, and request headers are not saved.

Reproduce from the repository root (Python standard library and the CLI required):

```bash
python3 scripts/capture-code-prompts.py codex --codex-catalog pkg/external/codex/models.json \
  --model gpt-6.2-sol --model gpt-6.2-astra \
  --model gpt-6.1-sol --model gpt-6.1-terra --model gpt-6-astra \
  --model gpt-6-sol --model gpt-6-luna --model gpt-5.6-sol \
  --model gpt-5.6-terra --model gpt-5.6-luna --model gpt-5.5

python3 scripts/capture-code-prompts.py claude \
  --model claude-sonnet-5-5 --model claude-haiku-5-5 \
  --model claude-opus-5-5 --model claude-opus-5 --model claude-opus-4-8 \
  --model claude-fable-5-2 --model claude-fable-5-1 --model claude-fable-5 \
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
effort. From Codex's delegation guidance at `1885417cd4` (2026-11-09) the shared
policy takes three rules: keep urgent work that blocks the next step, give each
editing agent ownership of files or a responsibility, and review a child's
edits and load-bearing claims. The child context carries the other two: the
workspace is shared, and the result goes to the launching agent.
GPT-5.5's small checklist wording difference from the embedded catalog
was already reflected in Wingman's template. The newer source retains the same
base prompts for these six captured model IDs.

GPT-6.1 Terra entered the Codex catalog in `126a0ee92e` (2026-10-26) with an
instruction template byte-identical to GPT-6 Astra's. The 0.167.0 capture
completed without fallback metadata and confirmed identical base instructions,
so `models/gpt-6-1-terra` started as a copy of the Astra template. The
capture's only Terra-specific runtime text prefers reasonable assumptions over
questions in the default mode; the template has carried that sentence since
2026-11-22.

Codex 0.167.0 reports fallback metadata for GPT-5.4, GPT-5.4 Mini, GPT-5.2,
and GPT-5.1. The script marks those runs unsuccessful even when the mock response
completes. Their existing model-specific templates remain in use, with Wingman
runtime improvements; generic fallback captures are not model-specific source
evidence.

The 2026-11-18 capture used the catalog introduced by Codex commit
`6c0cee502d`. All eleven model requests contained their catalog's exact base
instructions. GPT-6.2 Sol and Astra share one base prompt (SHA-256
`aa47d2dee261594c4944241b646088730d74d87afa4da8d2da0554540a5e7623`),
so both route to `models/gpt-6-2`. The first Wingman adaptation kept its
writing, coding, frontend, and task-completion guidance in condensed form; the
2026-11-22 adaptation below replaced it. The external Codex runner
receives each model's own upstream entry, including its runtime instructions.

Compared with the previous embedded catalog, the older GPT-6 base prompts
changed only in Codex-specific skill and plugin instructions. GPT-6.1 Terra
still matches GPT-6 Astra. The GPT-5.6 and GPT-5.5 base prompts are identical
to their previous versions. The external runner also retains the last upstream
GPT-5.4 entry in `models_legacy.json` for gateways that still serve it.

The 2026-11-22 capture with Codex 0.171.0 and the Codex checkout at
`cf3ff1530d` carry the same catalog as the 2026-11-18 capture, so no base
prompt changed. The adaptation did. The GPT-6 templates had been paraphrases
of the catalog text, and the GPT-6.2 template kept two sentences of its
`When to ask the user for permission` and `Autonomy and persistence` sections.
The first re-adaptation followed the catalog section by section. GPT-6.2
now keeps the captured clauses on scope, persistence, permission, and routine
decisions in a shorter template. Its writing, coding, and frontend guidance
uses the earlier compact wording. The other five GPT-6 templates shorten
the common instructions on progress updates, compaction, formatting, and
shell use. They retain their own permission, writing, and testing guidance,
including Terra's default-mode preference and Luna's testing rule.

| Catalog text | Template |
| --- | --- |
| Codex identity | Wingman identity and the security paragraphs |
| `commentary` and `final` channels | progress updates and the final answer |
| `request_user_input_async`, `send_user_message_async` | `elicit`, which blocks, so the advice to keep working while a question is open is dropped |
| `exec_command` and its `cmd` argument | `exec` and `command` |
| Markdown file links with absolute targets | `file_path:line_number` |
| auto-review rejection | Wingman's automatic review |
| worktrees and draft pull requests as examples of autonomous progress | reading beyond the workspace, edits, builds, tests, and merge conflicts; the shared runtime leaves commits, branches, and publishing to explicit authorization |

Not imported: the skills and plugin sections, which Wingman's shared skills
section replaces, except the sentence on naming the instruction that made the
agent pause; `rg` and the `functions.exec` batching rules, which the shared
runtime replaces with the dedicated search tools and parallel calls; the font
list and image-generation advice of the frontend guidance; Mermaid and
interactive visuals. The templates before GPT-6.2 keep Wingman's short
`Destructive actions` section and its two rules on long-running commands.

Codex sends GPT-6.2 and GPT-6.1 Terra one more sentence in its default
collaboration mode: strongly prefer reasonable assumptions and executing the
request over stopping to ask. Those two templates carry it and the others do
not, so `models/gpt-6-1-terra` is the Astra template plus that sentence.
`TestGPT6TemplatesCarryUpstreamAutonomyGuidance` keeps the sentences that
decide between working and handing back a proposal in each template.

VS Code Copilot's agent prompts
(`extensions/copilot/src/extension/prompts/node/agent` at `817607ae52b`) were
read for model hints on the same day: the GPT-5.5, 5.6, and 6 prompts, the
Claude 4.5 to 5 prompts, and the Gemini, Grok, GLM, Kimi, MiniMax, and
hidden-model prompts with their reminders. Its GPT-6 prompt states the default
the catalog text now supplies, to implement instead of stopping at a proposal
unless the user asks for a plan, asks a question, or is brainstorming. Its
Claude Opus 5 prompt asks to implement changes rather than only suggest them
and to act as soon as the context supports a decision, which the Claude Code
captures already say. Nothing was copied from it. What Copilot has and Wingman
does not is the placement: it repeats a short "keep going until the request is
resolved" reminder next to every user message for the GPT and hidden models.

Claude Sonnet/Opus 5.5 and Opus 4.8 use lean prompts, and Fable 5.2 adds
its own sections to the lean prompt; Haiku 5.5, Opus 5, Fable/Mythos 5 and
5.1, and general Sonnet 5 retain their respective longer structures.
The 2026-10-16 refresh removed a stale pre-mutation stop rule from Opus 4.8 and
Fable/Mythos 5 and replaced Opus 5's delegation restriction with Wingman's
shared policy. The general Claude template no longer treats arbitrary reminder
tags in tool output as trusted instructions. Shared family routing does not
imply every older Claude ID was captured.

The 2.1.319 comparison found one change in agent guidance. No 2.1.319 request
carries the sentence about matching the surrounding code's comment density,
naming, and idiom, in the base prompt, the context entry, or a tool
description. It was removed from the Sonnet/Opus 5.5, Opus 4.8, Haiku 5.5,
Opus 5, and Fable/Mythos 5 templates; Fable/Mythos 5 keep their separate rule
on when to write a comment. Fable/Mythos 5.1 and the general Claude template
did not have the sentence and match their captures in substance, so they are
unchanged. After adaptation the Sonnet 5.5, Opus 5.5, and Opus 4.8 templates
remain identical.

Other 2.1.319 differences are deliberately not adapted. A paragraph on default
pronouns, sent to every model, and a paragraph on recreating existing games,
sent only to Opus 5.5, are vendor content policy; `TestAgentPromptPolicy`
keeps that kind of text out. The reworded harness bullet about mid-conversation
system turns describes Claude Code's delivery mechanism, where the templates
keep Wingman's session-context sentence. The "operating autonomously" paragraph
appears because the capture runs non-interactively and belongs to the shared
unattended mode. The dedicated-tool and parallel-call bullet is owned by the
shared runtime.

Claude Code 2.1.319 sends Fable 5.2 the lean prompt plus the Fable/Mythos
distinction. It sends the same system text for an unrecognized `claude-fable-*`
ID and reports no display name or knowledge cutoff for 5.2, so this is the
CLI's default for the Fable line, not a prompt tuned to 5.2; re-capture when a
release adds 5.2 metadata. Release 2.1.330 is that release: it names Fable
5.2 and adds the sections listed below.

The 2.1.330 capture differs from the templates in four ways. Every model's
prompt opens with "an agent working with the user toward their goals, using
your own judgment along the way", which the templates had rendered as "an
interactive software engineering agent", and every model receives a paragraph
on keeping what it learns about the user and the project out of requests to
outside services. All Claude templates now carry both. Fable and Mythos 5
require the final message to come "with no tool calls after it". Fable 5.2 is
no longer the lean prompt alone. It adds a `Finishing work` section against ending a turn while
requested work is still owed, a `Memory, notes and feedback` section on what
is worth saving and on re-examining old cautions, a note that its training
limited a narrow set of capabilities such as accelerator kernel optimization,
where it should trust documentation over its preconceptions, and the rule
against unrequested planning documents. The Fable 5.2 template carries all
four in the capture's wording.

Not adapted from 2.1.330: the bullet on `<pasted_content>` tags and the note
that Edit no longer needs a prior Read, which describe Claude Code mechanisms;
the model descriptions that now name Fable 5.2, 5.1, and 5 with a link, where
the templates keep the neutral Fable and Mythos distinction; the fork
guidance; and, as before, the pronoun and game paragraphs.

That version also moves environment, deferred-tool, agent-type, and skill
listings into a `system`-role entry in `messages`. The script extracts that
entry as the developer text and appends it to the system text. The base text,
and the manifest's `base_sha256`, cover only the top-level `system`, which does
not contain per-run paths.

### Fallback (Inkling, Gemini, Qwen, Nemotron, GLM, Kimi, MiniMax, Grok, DeepSeek, Mistral)

Only Claude and GPT keep variants, each adapted from a captured vendor CLI
request. Every other model uses the fallback. The former Qwen, GLM, Kimi,
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

On 2026-11-22 the fallback's rules on acting, asking, and stopping were
replaced with the wording the captured prompts use. Before that it told the
model to answer requests to "diagnose" without changing files and to
"investigate read-only" while a goal was unclear, and Gemini 4 Carbon answered
a reported test failure plus "can you take a look?" with a diagnosis and an
offer to fix it. The rules now come from these sources:

| Rule | Source |
| --- | --- |
| Assume the work is wanted unless the user asks for a plan, asks a question, or is weighing options; "can you…" and "let's…" are requests to act | Copilot's GPT prompts and the GPT-6.1 Sol catalog text |
| Answer a question about why, how, or what a change would take without changing files, even when the fix looks small | Claude Code, Haiku 5.5 and Fable 5.1; the last clause was added after Qwen 3.8 Max and Mistral Large 4 fixed a bug they had only been asked about |
| Prefer a stated assumption over a question; ask first only when the most likely reading cannot be named or the task depends on something only the user has | Codex default mode and Claude Code, Haiku 5.5 |
| Errors and a tool that refuses a path are obstacles to work through with another tool or source; hand back only for a deliberate blocker | Claude Code, Fable 5.2 |
| Before ending a turn on a change request, check that the last paragraph is not a plan, an offer, or a promise | Claude Code, Fable and Mythos 5.1 |

All model variants and the fallback receive the same shared runtime contract.
Templates do not restate rules it already covers, such as commit limits,
preserving user changes, and claims about checks;
`TestTemplatesLeaveRuntimeRulesToSharedSection` checks this for the fallback
and generic GPT templates. The GPT-5.x, generic GPT, and general Claude
templates keep their own emphasis on batching independent tool calls.
GPT-5.1/5.2 no longer suggest asking to run validation that the task already
authorizes. The fallback also clarifies action requests, persistent authorization,
task steering, and completion checks. Shared Plan mode follows the same
delegation policy, restricted to read-only roles. Wingman's Plan mode forbids
builds and tests; this is intentionally stricter than Codex's allowance for
checks that do not mutate repository files. Vendor skills, permission
mechanisms, token budgets, environment data, and marketing copy are not imported
as Wingman runtime instructions. Neither are vendor limits on what the agent
may create, such as rules about copyrighted material or recreating existing
games: Wingman does not restrict that, and `TestAgentPromptPolicy` rejects the
wording when a refreshed capture brings it in.

### Gemini / Antigravity

Gemini models use the fallback. The removed Gemini template adapted requests
captured from Antigravity CLI **1.4.0** (Homebrew build `5281842794987520`) on
2026-10-16; the executable SHA-256 was
`a4502aaf310b2fd6eb657fcfec2fcc31a690fbbd0c2d14c8eefbb556409024ce`. Unlike the
Claude and GPT variants, it was a Wingman rewrite rather than a close vendor
adaptation. Once it stopped restating shared runtime rules and took the
fallback's context-reuse and focused-change guidance, it was mostly a subset of
the fallback.

The capture uses the CLI's Gemini API-key provider and its documented
[`GOOGLE_GEMINI_BASE_URL` override](https://www.antigravity.google/docs/install?tab=cli),
pointed at a localhost mock with a dummy key. Separate configuration and workspace
directories isolate the run. Synthetic text responses let the CLI finish without
model inference or agent tool execution. Re-capture a newer release to check
whether it adds model-specific instructions worth a variant.

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
runtime's tool preference covers the former; the private output protocol is not
used. Effort differences belong to request configuration, not prompt routing.
The captures also contain a separate short title-generation request; it is not
the coding agent's system prompt. No Gemini 4 or signed-in account-provider prompt
was captured.

Request capture and prompt tests establish provenance and compatibility. They
do not demonstrate improved model behavior or benchmark performance.

## Measured on 2026-11-22

The 2026-11-22 changes were run through the
[ACP benchmark](../../../bench/acpflow/README.md) against the build before
them, through the gateway. Cells are passed runs out of runs.

These historical sibling pass counts used a source-text verifier. It accepted a
retry comment or a changed client file while the retry behavior stayed the same.
The current verifier exercises the running client and evaluates the sibling policy.

A request that points next to the workspace, on GPT-6.2 Sol at low effort:

| Build | `adopt_sibling` | `edit_sibling` |
| --- | --- | --- |
| Before | 1/5 | 0/5 |
| Re-adapted GPT-6.2 template only | 3/5 | 1/5 |
| Environment lines on paths outside the file-tool roots only | 5/5 | 5/5 |
| Both | 5/5 | 5/5 |
| Codex CLI with the same model | 2/2 | 2/2 |

The failing runs answered that the path was outside the allowed roots and
asked for a copy of the files. GPT-6.1 Terra went from 1/3 to 3/3 on
`edit_sibling`; GPT-6.2 Astra, GPT-6 Luna, and Claude Fable 5.2 reached the
files through the shell before the change as well.

On the scenarios that stay inside the workspace, both builds passed every run
on GPT-6.2 Sol and Astra, GPT-6.1 Terra, GPT-6 Luna, and Claude Fable 5.2,
with request counts within run-to-run noise. The re-adapted templates
therefore show there as no regression, not as a gain. A first wording of the
environment line moved Fable 5.2 from the file tools to `cat` inside the
workspace, which is why it ends with "Inside them, keep using the file tools".

Later that day the file tools were opened for reading. With no read roots,
`read`, `grep`, and `glob` reach any path except the credential stores, as the
shell sandbox and Codex's `workspace-write` mode do; `edit` and `delete` still
stop at the workspace and the writable roots. The environment section says so,
and the shell line above renders only for a host that sets read roots. The
build with that shell line against this one, as the range of requests per run;
every run passed on both:

| Model | `adopt_sibling` before | after | `edit_sibling` before | after |
| --- | --- | --- | --- | --- |
| GPT-6.2 Sol, low | 7 to 12 (5 runs) | 6 to 9 (9) | 7 to 9 (5) | 7 to 9 (9) |
| GPT-6.2 Astra, low | 6, 8 (2) | 6 to 7 (5) | 5 to 6 (5) | 5 to 6 (5) |
| GPT-6.1 Terra, low | 13 to 25 (8) | 12 to 28 (11) | 7 (3) | 6 to 7 (6) |
| Claude Fable 5.2 | 4 (3) | 4 to 6 (7) | 5 (3) | 5 to 6 (7) |
| Claude Sonnet 5.5 | 4 (2) | 5 to 7 (6) | 3 (2) | 4 to 7 (12) |
| Inkling 2 Small | 8 to 11 (3) | 7 to 17 (7) | 8 to 13 (7) | 7 to 13 (13) |

Every model now reads the sibling files with the file tools; before, none
could. That is cheaper for GPT-6.2 Sol on `adopt_sibling` and costs Sonnet
5.5 one to three requests: in the build before, it did the whole task in
three shell commands, writing the workspace file with a heredoc. GPT-6.1
Terra and Inkling 2 Small also tend higher on `adopt_sibling`, with medians
of 20 against 15 and 12 against 8 in ranges that overlap. Gemini 4 Carbon
passed the five runs it completed. The workspace scenarios passed all 92 runs
on GPT-6.2 Sol, GPT-6.1 Terra, Fable 5.2, Sonnet 5.5, and Inkling 2 Small,
at request counts in the range measured earlier that day.

Three causes showed only in the call lists of that comparison:

- A file outside the workspace that was read with `read` and then changed
  through the shell was announced as changed on disk, and Fable 5.2 spent one
  to three requests re-reading it. Files outside every root are no longer
  tracked; its `edit_sibling` went from 6 to 8 requests back to 5 to 6.
- An `edit` batch that named a workspace file and the sibling file was refused
  for the second path, and Inkling 2 Small went on as if the first entry had
  applied, failing one run in three. Batch parsing and edit validation errors
  now end with "(no files changed)". Its next twelve runs passed without sending such a
  batch again, so a test covers this fix, not a measurement.
- Having read a file outside the roots, Sonnet 5.5 and Inkling 2 Small sent
  `edit` for it, were refused, and fell back to the shell, in 11 of 12 runs
  on `edit_sibling`. With "they change files only in the writable roots below"
  in the read line it was 5 of 11.

The fallback, on `problem_report` (a reported test failure with "can you take
a look?") and on a paraphrase of it: Gemini 4 Carbon stopped at a diagnosis
and an offer to fix it in 7 of 7 runs before and fixed it in 5 of 5 after.
The first rewrite overshot: Qwen 3.8 Max and Mistral Large 4 then fixed the
bug in `question_only` once in three runs each. With the wording that names
"what a change would take" as a question, `question_only` left the files
unchanged and `problem_report` was fixed in every run on Gemini 4 Carbon,
Qwen 3.8 Max, Mistral Large 4, Grok 4.7, GLM 5.3, and Inkling 2 Small, 3 to 5
runs each.

The same runs showed a cause no prompt wording fixed, in the `edit` tool
rather than in a prompt. Inkling 2 Small sent entries without `new_string` in
10 to 13 calls per 30 runs and repeated the rejected call up to eight times.
A direct probe showed its provider writing arguments in schema order and
dropping an optional one it had passed, so it could not send the `delete`
flag after `file_path` either and sent two empty strings instead. `edit` now
marks `old_string` and `new_string` as required, and deletion is a separate
`delete` tool with one required list. Over the same twelve runs Inkling 2
Small went from 10 failed calls to none. GPT-6.2 Sol and Claude Fable 5.2
pass either way; where one change both edits and deletes they now use up to
one request more, because they send the two calls in separate responses more
often than in one.

### Shorter GPT-6 templates

Rendered base sizes in bytes with the same standard tools enabled are below.
They include shared runtime instructions and exclude session context, tool
schemas, and conversation history.

| Model | Main (`616aace1`) | Expanded branch (`f3e1ff45`) | GPT-6 revision tested below |
| --- | ---: | ---: | ---: |
| GPT-6.2 Sol | 10,531 | 25,247 | 11,398 |
| GPT-6.1 Sol | 13,405 | 22,669 | 16,499 |
| GPT-6.1 Terra | 12,945 | 22,442 | 16,272 |
| GPT-6 Astra | 12,941 | 22,319 | 16,149 |
| GPT-6 Sol | 13,449 | 19,932 | 15,008 |
| GPT-6 Luna | 12,702 | 18,980 | 14,056 |

Each build ran each model and scenario below twice at low effort, with the same
verifier for each pair. Both builds passed all eight runs. The table gives medians.

| Model | Scenario | Requests before → after | Total input tokens before → after | Seconds before → after |
| --- | --- | ---: | ---: | ---: |
| GPT-6.2 Sol | `adopt_sibling` | 7 → 6.5 | 109,377 → 89,233 | 24.3 → 32.8 |
| GPT-6.2 Sol | `edit_sibling` | 8.5 → 9 | 134,682 → 116,532 | 23.8 → 18.8 |
| GPT-6.1 Terra | `edit_sibling` | 7 → 6.5 | 104,932 → 89,889 | 25.0 → 23.9 |
| GPT-6.1 Terra | `question_only` | 5 → 4 | 73,077 → 54,311 | 10.6 → 19.9 |

An intermediate GPT-6.2 prompt passed its four runs with median request counts
of 9 and 9.5. Its tool traces included preliminary shell lookups for named
files. The final shared guidance tells the agent to read known files directly.
All four comparisons used fewer total input tokens. Request counts and elapsed
time varied by scenario. These runs compare the complete builds, including
the shared guidance and file-tool fixes.

### Fallback, Claude, and file-tool choices

The fallback uses examples to distinguish a request to investigate and fix a
reported failure from a question about a possible change. The Claude variants
distinguish an approval denial from a file-tool path error. Both families
treat session-context updates as state. In an intermediate run, Inkling
reverted a requested sibling edit and saved a preference the user had never
expressed. The fallback now asks the model to identify the user message
before taking either action. The shared runtime sends pure renames to `edit` with `move_to`,
including when the file is binary or exceeds the 10 MiB text-edit limit.

Inkling 2 Small passed all 12 runs with the revised instructions at high
effort. Claude Fable 5.2 passed all four. This includes one final binary-rename
smoke test per model. Both fixed the reported test
failure and left the explanation-only fixture untouched. The 12 MiB binary
move preserved every byte in all six runs. Claude used the native edit tool
in both runs. Inkling used it three times and used shell `mv` once. A shell move of
this size falls outside the shell's content snapshot limit.

The two repeated editing scenarios had three runs per build. The baseline
here is the working tree at the start of the file-tool round. Inkling's
tool schemas shrank from 37,707 to 36,652 bytes. Its request counts and
total input tokens increased on both scenarios despite the shorter schemas.

| Inkling scenario | Median requests before → after | Median total input tokens before → after |
| --- | ---: | ---: |
| `multi_part` | 10 → 12 | 188,983 → 225,596 |
| `edit_sibling` | 9 → 10 | 161,307 → 175,749 |

Inkling used the dedicated delete tool for all three package removals. Two
of its three sibling-edit runs tried the file tool outside its write roots
before completing the task with the shell.

## Check a change

From the repository root:

```bash
go test ./pkg/code/prompt ./pkg/code/agent
```

For model-catalog changes, also run:

```bash
go test ./pkg/model ./pkg/external/codex ./pkg/acp/codex ./pkg/harness
```

These tests check routing, conditional fragment rendering, shared tool/schema
references, and serialized child requests with mode and tool restrictions.
They require localhost mock-provider listeners but no model inference.
Use matched model and
effort runs of the [ACP benchmark](../../../bench/acpflow/README.md) to compare
task behavior or latency.
